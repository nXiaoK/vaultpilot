package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const adminPassword = "admin-test-strong-password"
const recoveryPassword = "recovery-test-independent-password"

type testHarness struct {
	r      *Runtime
	h      http.Handler
	cookie *http.Cookie
	csrf   string
}

func newHarness(t *testing.T, allowLocal bool) *testHarness {
	t.Helper()
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	source := filepath.Join(dir, "source")
	restore := filepath.Join(dir, "restore")
	if e = os.MkdirAll(source, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(restore, 0700); e != nil {
		t.Fatal(e)
	}
	r, e := New(Config{StateDir: filepath.Join(dir, "state"), CacheDir: filepath.Join(dir, "cache"), SourceRoots: []string{source}, RestoreRoots: []string{restore}, AllowLocal: allowLocal, OAuthClientID: "test-client", PublicURL: "http://localhost"})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	mux := http.NewServeMux()
	r.Register(mux)
	v := &testHarness{r: r, h: r.Handler(mux)}
	res := v.call("POST", "/api/setup", map[string]any{"username": "admin", "password": adminPassword, "recovery_password": recoveryPassword, "mode": "create"}, false)
	if res.Code != 201 {
		t.Fatalf("setup %d %s", res.Code, res.Body.String())
	}
	v.cookie = res.Result().Cookies()[0]
	var data map[string]any
	json.Unmarshal(res.Body.Bytes(), &data)
	v.csrf = data["csrf_token"].(string)
	return v
}
func (h *testHarness) call(method, path string, value any, csrf bool) *httptest.ResponseRecorder {
	var body io.Reader
	if value != nil {
		b, _ := json.Marshal(value)
		body = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, body)
	req.Host = "localhost"
	if h.cookie != nil {
		req.AddCookie(h.cookie)
	}
	if csrf {
		req.Header.Set("X-CSRF-Token", h.csrf)
	}
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, req)
	return w
}
func TestRecoveryKitRejectsTamperingAndFreshInstanceRecovers(t *testing.T) {
	h := newHarness(t, false)
	plain := []byte("catalog secret with 中文 filename")
	sealed, e := h.r.Keys.Seal(plain)
	if e != nil {
		t.Fatal(e)
	}
	res := h.call("POST", "/api/security/recovery-kit", map[string]string{"admin_password": adminPassword, "password": recoveryPassword}, true)
	if res.Code != 200 {
		t.Fatalf("export %s", res.Body.String())
	}
	var kit RecoveryKit
	if e = json.Unmarshal(res.Body.Bytes(), &kit); e != nil {
		t.Fatal(e)
	}
	if _, e = openRecovery(kit, "wrong-password"); e == nil {
		t.Fatal("wrong password accepted")
	}
	box, _ := base64.StdEncoding.DecodeString(kit.Ciphertext)
	box[0] ^= 1
	tampered := kit
	tampered.Ciphertext = base64.StdEncoding.EncodeToString(box)
	if _, e = openRecovery(tampered, recoveryPassword); e == nil {
		t.Fatal("tampered kit accepted")
	}
	payload, e := UnlockRecoveryKit(res.Body.Bytes(), recoveryPassword)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(res.Body.Bytes()), "master_key") || payload.VaultID != h.r.VaultID() {
		t.Fatal("recovery kit exposed or lost master key")
	}
	dir := t.TempDir()
	fresh, e := New(Config{StateDir: filepath.Join(dir, "fresh-state"), CacheDir: filepath.Join(dir, "fresh-cache")})
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Close()
	m := http.NewServeMux()
	fresh.Register(m)
	fh := &testHarness{r: fresh, h: fresh.Handler(m)}
	res = fh.call("POST", "/api/setup", map[string]any{"username": "new-admin", "password": adminPassword, "recovery_password": recoveryPassword, "mode": "recovery", "recovery_kit": kit}, false)
	if res.Code != 201 {
		t.Fatalf("recovery setup %d %s", res.Code, res.Body.String())
	}
	if !fresh.RecoveryMode() {
		t.Fatal("fresh instance not in recovery mode")
	}
	opened, e := fresh.Keys.Open(sealed)
	if e != nil || !bytes.Equal(opened, plain) {
		t.Fatal("new node cannot decrypt catalog")
	}
	if bytes.Equal(fresh.nodeKey, h.r.nodeKey) {
		t.Fatal("new node reused credential key")
	}
	fresh.Keys.Lock()
	if _, e = fresh.Keys.Open(sealed); e == nil {
		t.Fatal("locked vault decrypted")
	}
	if e = fresh.unlock(recoveryPassword); e != nil {
		t.Fatal(e)
	}
}
func TestSessionsCSRFVersionAndRateLimit(t *testing.T) {
	h := newHarness(t, false)
	res := h.call("POST", "/api/security/lock", map[string]any{}, false)
	if res.Code != 403 {
		t.Fatal("write without CSRF accepted")
	}
	req := httptest.NewRequest("POST", "/api/security/lock", strings.NewReader("{}"))
	req.Host = "localhost"
	req.AddCookie(h.cookie)
	req.Header.Set("X-CSRF-Token", h.csrf)
	req.Header.Set("Origin", "https://attacker.invalid")
	w := httptest.NewRecorder()
	h.h.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal("cross origin accepted")
	}
	path := h.r.Config.SourceRoots[0]
	res = h.call("POST", "/api/sources", map[string]any{"name": "source", "path": path}, true)
	if res.Code != 201 {
		t.Fatalf("source: %s", res.Body.String())
	}
	var s Source
	json.Unmarshal(res.Body.Bytes(), &s)
	s.Version = 99
	res = h.call("PUT", "/api/sources/"+s.ID, s, true)
	if res.Code != 409 {
		t.Fatal("stale source overwritten")
	}
	h.call("POST", "/api/auth/logout", map[string]any{}, true)
	if h.call("GET", "/api/accounts", nil, false).Code != 401 {
		t.Fatal("logged out session still accepted")
	}
	for i := 0; i < 11; i++ {
		res = h.call("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong-password"}, false)
		if i < 10 && res.Code != 401 {
			t.Fatalf("login attempt %d: %d %s", i+1, res.Code, res.Body.String())
		}
	}
	if res.Code != 429 || res.Header().Get("Retry-After") == "" {
		t.Fatal("login throttling missing")
	}
}
func TestSourceBoundarySymlinkAndMountIdentity(t *testing.T) {
	h := newHarness(t, false)
	root := h.r.Config.SourceRoots[0]
	outside := t.TempDir()
	if e := os.Symlink(outside, filepath.Join(root, "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e := h.r.ValidateSourcePath(filepath.Join(root, "escape")); e == nil {
		t.Fatal("symlink escape accepted")
	}
	inside := filepath.Join(root, "inside")
	os.Mkdir(inside, 0700)
	os.Symlink(inside, filepath.Join(root, "alias"))
	if _, e := h.r.ValidateSourcePath(filepath.Join(root, "alias")); e == nil {
		t.Fatal("source symlink accepted")
	}
	if _, e := h.r.ValidateSourcePath(h.r.Config.StateDir); e == nil {
		t.Fatal("state directory accepted")
	}
	before, e := MountIdentity(inside)
	if e != nil {
		t.Fatal(e)
	}
	os.Rename(inside, inside+"-old")
	os.Mkdir(inside, 0700)
	after, e := MountIdentity(inside)
	if e != nil || before == after {
		t.Fatal("directory replacement not detected")
	}
	res := h.call("GET", "/api/browse?path="+url.QueryEscape(root), nil, false)
	if res.Code != 200 || strings.Contains(res.Body.String(), "escape") || strings.Contains(res.Body.String(), "alias") {
		t.Fatal("browse followed symlink")
	}
}
func TestLocalRequiresExplicitFlagAndVerifiedPool(t *testing.T) {
	h := newHarness(t, false)
	res := h.call("POST", "/api/accounts", map[string]any{"name": "local", "backend": "local", "local_path": t.TempDir(), "budget_bytes": 1 << 30}, true)
	if res.Code != 403 {
		t.Fatal("local backend available by default")
	}
	h = newHarness(t, true)
	dir, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	res = h.call("POST", "/api/accounts", map[string]any{"name": "real disk", "backend": "local", "local_path": dir, "budget_bytes": 1 << 30}, true)
	if res.Code != 201 {
		t.Fatalf("local create %s", res.Body.String())
	}
	var a Account
	json.Unmarshal(res.Body.Bytes(), &a)
	if !a.Verified || a.TotalBytes <= 0 || a.FreeBytes <= 0 {
		t.Fatal("local did not query real filesystem")
	}
	if files, _ := os.ReadDir(dir); len(files) != 0 {
		t.Fatal("verification temporary file leaked")
	}
	res = h.call("POST", "/api/pools", map[string]any{"name": "pool", "account_ids": []string{a.ID}}, true)
	if res.Code != 400 {
		t.Fatal("default two catalog replicas silently degraded")
	}
	res = h.call("POST", "/api/pools", map[string]any{"name": "explicit single test pool", "account_ids": []string{a.ID}, "catalog_replicas": 1}, true)
	if res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	res = h.call("DELETE", "/api/accounts/"+a.ID, nil, true)
	if res.Code != 409 {
		t.Fatal("referenced account deleted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
func cloudResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
}

func TestStartupPreservesRcloneRefreshAfterCrash(t *testing.T) {
	h := newHarness(t, false)
	a := Account{ID: newID(), Name: "persistent token", Backend: "onedrive", Remote: "vp_test", DriveID: "persistent-drive", DriveType: "personal", Verified: true}
	if _, e := h.r.Store.Save(context.Background(), "accounts", a.ID, 0, a); e != nil {
		t.Fatal(e)
	}
	original := OAuthToken{AccessToken: "old-private-access", RefreshToken: "old-private-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	if e := h.r.saveToken(a.ID, original); e != nil {
		t.Fatal(e)
	}
	path, configErr := h.r.RcloneConfigFor(context.Background(), a.ID)
	if configErr != nil {
		t.Fatal("missing config")
	}
	updated := original
	updated.AccessToken = "updated-private-access"
	updated.RefreshToken = "updated-private-refresh"
	updated.Expiry = original.Expiry.Add(time.Hour)
	b, _ := json.Marshal(updated)
	credential, _ := h.r.loadCredential(a.ID)
	if e := os.WriteFile(path, []byte("["+a.Remote+"]\nvp_credential_id = "+credential.GrantID+"\ntoken = "+string(b)+"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	// Simulate abrupt termination: leave the transient Rclone config behind.
	if e := h.r.Store.DB.Close(); e != nil {
		t.Fatal(e)
	}
	restarted, e := New(h.r.Config)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	token, e := restarted.loadToken(a.ID)
	if e != nil || token.RefreshToken != updated.RefreshToken {
		t.Fatal("crash lost refreshed token")
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("stale plaintext config survived restart")
	}
	if restarted.Keys.Unlocked() {
		t.Fatal("manual unlock unexpectedly persisted in process restart")
	}
}
func TestOAuthStatePKCEExplicitSelectionDuplicateDriveAndRefresh(t *testing.T) {
	h := newHarness(t, false)
	res := h.call("POST", "/api/accounts", map[string]any{"name": "OneDrive test"}, true)
	var a Account
	json.Unmarshal(res.Body.Bytes(), &a)
	if a.DriveID != "" || a.Verified {
		t.Fatal("drive auto selected")
	}
	res = h.call("POST", "/api/accounts/"+a.ID+"/authorize", map[string]any{}, true)
	var auth map[string]string
	json.Unmarshal(res.Body.Bytes(), &auth)
	u, e := url.Parse(auth["authorization_url"])
	if e != nil {
		t.Fatal(e)
	}
	state := u.Query().Get("state")
	flow := h.r.oauth[state]
	hash := sha256.Sum256([]byte(flow.Verifier))
	if u.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	var calls atomic.Int64
	var uploaded []byte
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Host == "login.microsoftonline.com" {
			req.ParseForm()
			if req.Form.Get("grant_type") == "authorization_code" && req.Form.Get("code_verifier") != flow.Verifier {
				t.Fatal("wrong PKCE verifier")
			}
			return cloudResponse(200, `{"access_token":"private-access-token","refresh_token":"private-refresh-token","token_type":"Bearer","expires_in":3600}`), nil
		}
		if req.Header.Get("Authorization") == "" {
			t.Fatal("Graph missing token")
		}
		switch {
		case req.URL.Path == "/v1.0/me/drives":
			return cloudResponse(200, `{"value":[{"id":"metadata","name":"ODCMetadataArchive","driveType":"documentLibrary"},{"id":"personal-drive","name":"Documents","driveType":"personal"}]}`), nil
		case req.URL.Path == "/v1.0/drives/personal-drive":
			return cloudResponse(200, `{"id":"personal-drive","quota":{"total":10000000,"used":100000,"remaining":9900000}}`), nil
		case req.URL.Path == "/v1.0/drives/personal-drive/root":
			return cloudResponse(200, `{"id":"root","folder":{}}`), nil
		case req.Method == "GET" && strings.Contains(req.URL.Path, "/root:/VaultPilot/"):
			return cloudResponse(200, `{"id":"vault-folder","folder":{},"size":4096}`), nil
		case req.Method == "GET" && strings.Contains(req.URL.Path, "/root:/VaultPilot"):
			return cloudResponse(200, `{"id":"project-folder","folder":{}}`), nil
		case req.Method == "GET" && strings.Contains(req.URL.Path, "/items/project-folder:"):
			return cloudResponse(200, `{"id":"vault-folder","folder":{}}`), nil
		case req.Method == "PUT":
			uploaded, _ = io.ReadAll(req.Body)
			return cloudResponse(200, `{"id":"verification-file"}`), nil
		case req.Method == "GET" && strings.HasSuffix(req.URL.Path, "/items/verification-file/content"):
			return cloudResponse(200, string(uploaded)), nil
		case req.Method == "DELETE":
			return cloudResponse(204, ""), nil
		default:
			t.Fatalf("unexpected Graph %s %s", req.Method, req.URL.String())
			return nil, nil
		}
	})}
	res = h.call("GET", "/api/oauth/callback?state=wrong&code=code", nil, false)
	if calls.Load() != 0 || !strings.Contains(res.Header().Get("Location"), "state_invalid") {
		t.Fatal("invalid state exchanged token")
	}
	res = h.call("GET", "/api/oauth/callback?state="+state+"&code=code", nil, false)
	if res.Code != 303 || !strings.Contains(res.Header().Get("Location"), "authorized=") {
		t.Fatal("authorization failed")
	}
	raw, _ := h.r.meta("credential:" + a.ID)
	if strings.Contains(raw, "private-") {
		t.Fatal("plaintext token persisted")
	}
	h.r.Store.Get(context.Background(), "accounts", a.ID, &a)
	res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version}, true)
	if res.Code != 400 {
		t.Fatal("missing explicit drive accepted")
	}
	res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "metadata"}, true)
	if res.Code != 400 || len(uploaded) > 0 {
		t.Fatal("metadata drive accepted")
	}
	res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "personal-drive"}, true)
	if res.Code != 200 {
		t.Fatalf("verify %s", res.Body.String())
	}
	json.Unmarshal(res.Body.Bytes(), &a)
	if !a.Verified || a.FreeBytes != 9900000 || a.ProjectUsedBytes != 4096 {
		t.Fatal("verification omitted real quota")
	}
	res = h.call("POST", "/api/accounts", map[string]any{"name": "duplicate"}, true)
	var second Account
	json.Unmarshal(res.Body.Bytes(), &second)
	token, e := h.r.loadToken(a.ID)
	if e != nil {
		t.Fatal(e)
	}
	h.r.saveToken(second.ID, token)
	res = h.call("POST", "/api/accounts/"+second.ID+"/verify", map[string]any{"version": second.Version, "drive_id": "personal-drive"}, true)
	if res.Code != 409 || !strings.Contains(res.Body.String(), "duplicate_drive") {
		t.Fatal("duplicate drive accepted")
	}
	path, configErr := h.r.RcloneConfigFor(context.Background(), a.ID)
	if configErr != nil {
		t.Fatal("rclone config generation failed")
	}
	config, e := os.ReadFile(path)
	if e != nil || !strings.Contains(string(config), "access_scopes = "+oauthScope+"\n") || strings.Contains(string(config), "Files.ReadWrite.All") || strings.Contains(string(config), "Sites.Read.All") {
		t.Fatal("rclone refresh scopes differ from consented web OAuth grant")
	}
	if !strings.Contains(string(config), "token_url = https://login.microsoftonline.com/consumers/oauth2/v2.0/token\n") {
		t.Fatal("rclone refresh endpoint differs from web grant tenant")
	}
	info, e := os.Stat(path)
	if e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("rclone plaintext permissions incorrect")
	}
	updated := token
	updated.AccessToken = "rclone-new-access"
	updated.RefreshToken = "rclone-new-refresh"
	updated.Expiry = time.Now().Add(2 * time.Hour)
	b, _ := json.Marshal(updated)
	credential, _ := h.r.loadCredential(a.ID)
	os.WriteFile(path, []byte("["+a.Remote+"]\nvp_credential_id = "+credential.GrantID+"\ntoken = "+string(b)+"\n"), 0600)
	if e = h.r.ReleaseRcloneConfig(path); e != nil {
		t.Fatal(e)
	}
	persisted, e := h.r.loadToken(a.ID)
	if e != nil || persisted.RefreshToken != updated.RefreshToken {
		t.Fatal("rclone refresh lost")
	}
	if _, e = os.Stat(path); !os.IsNotExist(e) {
		t.Fatal("completed config not cleaned")
	}
	kitRes := h.call("POST", "/api/security/recovery-kit", map[string]string{"admin_password": adminPassword, "password": recoveryPassword}, true)
	payload, e := UnlockRecoveryKit(kitRes.Body.Bytes(), recoveryPassword)
	if e != nil || len(payload.Entries) != 1 || payload.Entries[0].DriveID != a.DriveID {
		t.Fatal("recovery discovery entries missing")
	}
	pJSON, _ := json.Marshal(payload)
	if strings.Contains(string(pJSON), "token") {
		t.Fatal("OAuth credentials leaked into recovery kit")
	}
}
