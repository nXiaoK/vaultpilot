package platform

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

const transferPassword = "disposable-transfer-password"

func transferFixture(t *testing.T, h *testHarness, mode, drive string) Account {
	t.Helper()
	a := Account{ID: newID(), Version: 1, Name: "Family " + drive, Backend: "onedrive", DriveID: drive, DriveName: "OneDrive", DriveType: "personal", Region: "global", Verified: true, Status: "ready", Enabled: true, BudgetBytes: 1 << 40, AccountEmail: drive + "@outlook.com"}
	if _, err := h.r.Store.Save(t.Context(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	c := accountCredential{Mode: mode, ClientID: "original-client", Tenant: "consumers", Scopes: oauthScope, GrantID: "old-grant", Token: OAuthToken{AccessToken: "private-access", RefreshToken: "private-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	if mode == "custom" {
		c.ClientSecret = "original-private-secret"
	} else {
		c.ClientSecret = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	}
	if err := h.r.saveCredential(a.ID, c); err != nil {
		t.Fatal(err)
	}
	return a
}
func TestAccountTransferBetweenNodes(t *testing.T) {
	a := newHarness(t, false)
	b := newPendingCloudHarness(t, adminPassword)
	first := transferFixture(t, a, "rclone", "drive-one")
	second := transferFixture(t, a, "custom", "drive-two")
	res := a.call("POST", "/api/accounts/export", map[string]any{"admin_password": adminPassword, "password": transferPassword, "account_ids": []string{first.ID, second.ID}}, false)
	if res.Code != 403 {
		t.Fatal("export missing CSRF accepted")
	}
	res = a.call("POST", "/api/accounts/export", map[string]any{"admin_password": adminPassword, "password": transferPassword, "account_ids": []string{first.ID, second.ID}}, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	for _, secret := range []string{"private-access", "private-refresh", "original-private-secret", "drive-one@outlook.com", "master_key"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatalf("export leaked %s", secret)
		}
	}
	var kit RecoveryKit
	json.Unmarshal(res.Body.Bytes(), &kit)
	req := map[string]any{"password": transferPassword, "package": kit, "drive_ids": []string{"drive-one", "drive-two"}}
	res = b.call("POST", "/api/accounts/import", req, false)
	if res.Code != 403 {
		t.Fatal("import missing CSRF accepted")
	}

	res = b.call("POST", "/api/accounts/import/preview", req, true)
	if res.Code != 200 || !strings.Contains(res.Body.String(), "drive-one@outlook.com") || strings.Contains(res.Body.String(), "private-refresh") || strings.Contains(res.Body.String(), "original-client") {
		t.Fatal(res.Body.String())
	}
	// Unknown selection rejects the entire package before writing anything.
	req["drive_ids"] = []string{"drive-one", "unknown"}
	res = b.call("POST", "/api/accounts/import", req, true)
	if res.Code != 400 {
		t.Fatal("accepted unknown selection")
	}
	all, _ := b.r.Store.All(t.Context(), "accounts")
	if len(all) != 0 {
		t.Fatal("partial import")
	}
	req["drive_ids"] = []string{"drive-one", "drive-two"}
	res = b.call("POST", "/api/accounts/import", req, true)
	var result struct {
		Items   []Account
		Skipped []Account
	}
	json.Unmarshal(res.Body.Bytes(), &result)
	if res.Code != 201 || len(result.Items) != 2 {
		t.Fatal(res.Body.String())
	}
	for _, imported := range result.Items {
		if imported.ID == first.ID || imported.ID == second.ID || imported.Verified || imported.Version != 1 || imported.TotalBytes != 0 || imported.Status != "awaiting_drive_selection" {
			t.Fatalf("trusted original status: %+v", imported)
		}
		c, err := b.r.loadCredential(imported.ID)
		if err != nil {
			t.Fatal(err)
		}
		expectedMode := "rclone"
		if imported.DriveID == "drive-two" {
			expectedMode = "custom"
		}
		if c.Mode != expectedMode || c.ClientID != "original-client" || c.GrantID == "old-grant" || c.Token.RefreshToken != "private-refresh" {
			t.Fatalf("lost original credential settings")
		}
		if c.Mode == "custom" && c.ClientSecret != "original-private-secret" {
			t.Fatal("corrupted plaintext custom secret")
		}
		raw, _ := b.r.meta("credential:" + imported.ID)
		if strings.Contains(raw, "private-") {
			t.Fatal("plaintext stored")
		}
		if bytes.Equal(a.r.nodeKey, b.r.nodeKey) {
			t.Fatal("shared node key")
		}
		envelope, _ := base64.StdEncoding.DecodeString(raw)
		if _, err := decrypt(a.r.nodeKey, envelope, []byte("vaultpilot-oauth-v1:"+imported.ID)); err == nil {
			t.Fatal("A key decrypted B credential")
		}

	}
	// B is still locked and pending: credential transfer does not install a vault key.
	if b.r.Keys.Unlocked() || !b.r.CloudPending() {
		t.Fatal("transfer installed backup key")
	}
	// A repeated import is safe and never replaces local IDs or credentials.
	res = b.call("POST", "/api/accounts/import", req, true)
	json.Unmarshal(res.Body.Bytes(), &result)
	if res.Code != 201 || len(result.Items) != 0 || len(result.Skipped) != 2 {
		t.Fatal(res.Body.String())
	}
	all, _ = b.r.Store.All(t.Context(), "accounts")
	if len(all) != 2 {
		t.Fatal("duplicated drive")
	}
	// Verify imported credential using only reads on a recovery node.
	var account Account
	json.Unmarshal(all[0], &account)
	b.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != "GET" {
			t.Errorf("recovery verification writes: %s", req.Method)
		}
		switch req.URL.Path {
		case "/v1.0/me/drives":
			return cloudResponse(200, `{"value":[{"id":"drive-one","name":"OneDrive","driveType":"personal"},{"id":"drive-two","name":"OneDrive","driveType":"personal"}]}`), nil
		case "/v1.0/drives/" + account.DriveID:
			return cloudResponse(200, `{"id":"`+account.DriveID+`","quota":{"total":1000,"used":100,"remaining":900}}`), nil
		default:
			return cloudResponse(200, `{"id":"root","folder":{},"size":0}`), nil
		}
	})}
	res = b.call("POST", "/api/accounts/"+account.ID+"/verify", map[string]any{"version": account.Version, "drive_id": account.DriveID}, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
}
func TestAccountTransferRejectsTamperingAndUnsafeSettings(t *testing.T) {
	h := newHarness(t, false)
	a := transferFixture(t, h, "custom", "safe-drive")
	c, _ := h.r.loadCredential(a.ID)
	p := transferPayload{Entries: []transferEntry{{portableAccount(a), c}}}
	kit, err := protectAccounts(p, transferPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openAccounts(kit, "wrong-password"); err == nil {
		t.Fatal("wrong password accepted")
	}
	box, _ := base64.StdEncoding.DecodeString(kit.Ciphertext)
	box[0] ^= 1
	bad := kit
	bad.Ciphertext = base64.StdEncoding.EncodeToString(box)
	if _, err = openAccounts(bad, transferPassword); err == nil {
		t.Fatal("tampering accepted")
	}
	bad = kit
	bad.Memory = 1024 * 1024
	if _, err = openAccounts(bad, transferPassword); err == nil {
		t.Fatal("unsafe KDF accepted")
	}
	if _, err = openRecovery(kit, transferPassword); err == nil {
		t.Fatal("account package accepted as recovery kit")
	}
	p.Entries[0].Credential.Tenant = "evil.example/path"
	bad, err = protectAccounts(p, transferPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = openAccounts(bad, transferPassword); err == nil {
		t.Fatal("unsafe tenant accepted")
	}
	p.Entries[0].Credential = c
	p.Entries[0].Account.DriveID = "drive\nconfig=true"
	bad, _ = protectAccounts(p, transferPassword)
	if _, err = openAccounts(bad, transferPassword); err == nil {
		t.Fatal("config injection accepted")
	}
}
