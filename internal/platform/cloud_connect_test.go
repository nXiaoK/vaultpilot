package platform

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func newPendingCloudHarness(t *testing.T, loginPassword string) *testHarness {
	t.Helper()
	dir := t.TempDir()
	r, err := New(Config{StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"), AllowLocal: true})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	r.Register(mux)
	h := &testHarness{r: r, h: r.Handler(mux)}
	t.Cleanup(func() { h.r.Close() })
	res := h.call("POST", "/api/setup", map[string]any{"username": "reader-admin", "password": loginPassword, "mode": "cloud"}, false)
	if res.Code != 201 {
		t.Fatalf("cloud setup: %d %s", res.Code, res.Body.String())
	}
	h.cookie = res.Result().Cookies()[0]
	var data map[string]any
	if err = json.Unmarshal(res.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	h.csrf = data["csrf_token"].(string)
	return h
}

func TestPendingCloudSetupHasLocalAdminWithoutProvisionalVault(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	if !h.r.Initialized() || !h.r.CloudPending() || !h.r.RecoveryMode() || h.r.VaultID() != "" || h.r.Keys.Unlocked() {
		t.Fatal("pending node must be initialized, read-only and without vault identity or keys")
	}
	if raw, err := h.r.meta("recovery_kit"); err != nil || raw != "" {
		t.Fatal("pending node persisted provisional vault unlock data")
	}
	var status map[string]any
	res := h.call("GET", "/api/status", nil, false)
	json.Unmarshal(res.Body.Bytes(), &status)
	if status["initialized"] != true || status["cloud_pending"] != true || status["unlocked"] != false {
		t.Fatalf("pending status: %s", res.Body.String())
	}
	res = h.call("POST", "/api/setup", map[string]any{"username": "other-admin", "password": adminPassword, "mode": "cloud"}, false)
	if res.Code != 409 {
		t.Fatal("pending administrator was overwritten by another setup")
	}
	if res = h.call("GET", "/api/accounts", nil, false); res.Code != 200 {
		t.Fatalf("pending account management unavailable: %s", res.Body.String())
	}
	config := h.r.Config
	if err := h.r.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	h.r = restarted
	mux := http.NewServeMux()
	restarted.Register(mux)
	h.h = restarted.Handler(mux)
	h.cookie = nil
	if !restarted.Initialized() || !restarted.CloudPending() || restarted.VaultID() != "" || restarted.Keys.Unlocked() {
		t.Fatal("pending node did not survive restart")
	}
	res = h.call("POST", "/api/auth/login", map[string]string{"username": "reader-admin", "password": adminPassword}, false)
	if res.Code != 200 {
		t.Fatalf("pending administrator cannot log in: %s", res.Body.String())
	}
}

func TestCloudBootstrapUsesOriginalPasswordAndDoesNotUnlockWriter(t *testing.T) {
	w := newHarness(t, false)
	exportPassword := "independent-export-protection-password"
	res := w.call("POST", "/api/security/recovery-kit", map[string]string{"admin_password": adminPassword, "password": exportPassword}, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	w.r.Keys.Lock()
	if _, err := w.r.BuildCloudBootstrap(exportPassword); err == nil {
		t.Fatal("offline export password replaced the original backup password")
	}
	raw, err := w.r.BuildCloudBootstrap(recoveryPassword)
	if err != nil {
		t.Fatal(err)
	}
	if w.r.Keys.Unlocked() {
		t.Fatal("building a connection file unexpectedly unlocked the writer")
	}
	payload, err := UnlockCloudBootstrap(raw, recoveryPassword, w.r.VaultID())
	if err != nil || payload.VaultID != w.r.VaultID() {
		t.Fatalf("valid bootstrap: %v", err)
	}
	if bytes.Contains(raw, []byte(payload.MasterKey)) || bytes.Contains(raw, []byte(recoveryPassword)) || bytes.Contains(raw, []byte("master_key")) {
		t.Fatal("cloud bootstrap exposed a plaintext secret")
	}
}

func TestCloudBootstrapRejectsTamperingAndBindsVaultIdentity(t *testing.T) {
	w := newHarness(t, false)
	raw, err := w.r.BuildCloudBootstrap(recoveryPassword)
	if err != nil {
		t.Fatal(err)
	}
	var original cloudBootstrap
	json.Unmarshal(raw, &original)
	encode := func(b cloudBootstrap) []byte { encoded, _ := json.Marshal(b); return encoded }
	wrongVersion := original
	wrongVersion.Version++
	wrongKDF := original
	wrongKDF.Kit.Memory++
	tampered := original
	box, _ := base64.StdEncoding.DecodeString(tampered.Kit.Ciphertext)
	box[len(box)-1] ^= 1
	tampered.Kit.Ciphertext = base64.StdEncoding.EncodeToString(box)
	otherID := strings.Repeat("a", 32)
	otherHeader := original
	otherHeader.VaultID = otherID
	for _, tc := range []struct {
		name, password, expected string
		raw                      []byte
	}{
		{"wrong password", "incorrect-backup-password", w.r.VaultID(), raw},
		{"tampered ciphertext", recoveryPassword, w.r.VaultID(), encode(tampered)},
		{"unsupported version", recoveryPassword, w.r.VaultID(), encode(wrongVersion)},
		{"unbounded KDF", recoveryPassword, w.r.VaultID(), encode(wrongKDF)},
		{"wrong Drive path", recoveryPassword, otherID, raw},
		{"header does not match encrypted identity", recoveryPassword, otherID, encode(otherHeader)},
		{"oversized", recoveryPassword, w.r.VaultID(), bytes.Repeat([]byte(" "), CloudBootstrapMaxBytes+1)},
		{"truncated", recoveryPassword, w.r.VaultID(), raw[:len(raw)-1]},
		{"trailing JSON", recoveryPassword, w.r.VaultID(), append(append([]byte(nil), raw...), []byte(" {}")...)},
		{"invalid path ID", recoveryPassword, "../outside", raw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := UnlockCloudBootstrap(tc.raw, tc.password, tc.expected); err == nil {
				t.Fatal("invalid connection file accepted")
			}
		})
	}
}

func TestCloudConnectionIsAtomicIndependentAndRestartUnlockable(t *testing.T) {
	w := newHarness(t, false)
	raw, err := w.r.BuildCloudBootstrap(recoveryPassword)
	if err != nil {
		t.Fatal(err)
	}
	r := newPendingCloudHarness(t, adminPassword)
	beforeKit, _ := r.r.meta("recovery_kit")
	if err = r.r.ConnectCloudBootstrap(raw, "incorrect-backup-password", w.r.VaultID()); err == nil {
		t.Fatal("wrong password connected")
	}
	if err = r.r.ConnectCloudBootstrap(raw, recoveryPassword, strings.Repeat("a", 32)); err == nil {
		t.Fatal("wrong vault path connected")
	}
	afterKit, _ := r.r.meta("recovery_kit")
	if r.r.VaultID() != "" || !r.r.CloudPending() || r.r.Keys.Unlocked() || beforeKit != afterKit {
		t.Fatal("failed connection mutated pending node")
	}
	plain := []byte("catalog from original A node")
	sealed, err := w.r.Keys.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.r.ConnectCloudBootstrap(raw, recoveryPassword, w.r.VaultID()); err != nil {
		t.Fatal(err)
	}
	if r.r.CloudPending() || !r.r.RecoveryMode() || r.r.VaultID() != w.r.VaultID() || !r.r.Keys.Unlocked() {
		t.Fatal("node did not transition to original vault recovery mode")
	}
	opened, err := r.r.Keys.Open(sealed)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatal("B did not install A's master key")
	}
	if bytes.Equal(r.r.nodeKey, w.r.nodeKey) {
		t.Fatal("connection reused A's node credential key")
	}
	if err = r.r.ConnectCloudBootstrap(raw, recoveryPassword, w.r.VaultID()); err == nil {
		t.Fatal("connected vault allowed replacement")
	}
	if _, err = r.r.BuildCloudBootstrap(recoveryPassword); err == nil {
		t.Fatal("recovery reader can publish a writer bootstrap")
	}
	config := r.r.Config
	if err = r.r.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	r.r = restarted
	if restarted.Keys.Unlocked() || restarted.VaultID() != w.r.VaultID() || !restarted.RecoveryMode() {
		t.Fatal("connected vault identity did not survive restart")
	}
	if err = restarted.unlock(recoveryPassword); err != nil {
		t.Fatal(err)
	}
	opened, err = restarted.Keys.Open(sealed)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatal("restarted reader cannot decrypt original catalog")
	}
	username, _ := restarted.meta("admin_username")
	if username != "reader-admin" {
		t.Fatal("connection replaced local administrator")
	}
}

func TestCloudConnectionRequiresSeparateLoginAndRecoveryPasswords(t *testing.T) {
	w := newHarness(t, false)
	raw, err := w.r.BuildCloudBootstrap(recoveryPassword)
	if err != nil {
		t.Fatal(err)
	}
	r := newPendingCloudHarness(t, recoveryPassword)
	if err = r.r.ConnectCloudBootstrap(raw, recoveryPassword, w.r.VaultID()); err == nil {
		t.Fatal("backup password can be reused as local admin password")
	}
	if !r.r.CloudPending() || r.r.VaultID() != "" || r.r.Keys.Unlocked() {
		t.Fatal("credential separation failure mutated pending node")
	}
	if err = w.r.ConnectCloudBootstrap(raw, recoveryPassword, w.r.VaultID()); err == nil {
		t.Fatal("writer vault can be replaced by a cloud connection")
	}
}
