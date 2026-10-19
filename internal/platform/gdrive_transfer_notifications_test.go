package platform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGoogleAccountPackageRoundTrip(t *testing.T) {
	writer := newHarness(t, false)
	reader := newPendingCloudHarness(t, adminPassword)
	a := transferFixture(t, writer, "rclone", "google-root")
	a.Backend, a.DriveType, a.Region, a.TenantID = backendGoogleDrive, "my_drive", "google", "personal"
	a.StorageGroup = "家庭 Google One"
	if _, err := writer.r.Store.Save(t.Context(), "accounts", a.ID, a.Version, a); err != nil {
		t.Fatal(err)
	}
	c, err := writer.r.loadCredential(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	c.Provider, c.Scopes, c.Tenant = backendGoogleDrive, "drive", ""
	if err = writer.r.saveCredential(a.ID, c); err != nil {
		t.Fatal(err)
	}
	res := writer.call("POST", "/api/accounts/export", map[string]any{"admin_password": adminPassword, "password": transferPassword, "account_ids": []string{a.ID}}, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	var kit RecoveryKit
	if err = json.Unmarshal(res.Body.Bytes(), &kit); err != nil {
		t.Fatal(err)
	}
	if kit.Version != 1 {
		t.Fatal("unexpected package version")
	}
	res = reader.call("POST", "/api/accounts/import", map[string]any{"password": transferPassword, "package": kit, "drive_ids": []string{a.DriveID}}, true)
	var result struct{ Items []Account }
	if err = json.Unmarshal(res.Body.Bytes(), &result); err != nil || res.Code != 201 || len(result.Items) != 1 {
		t.Fatalf("import: %v %s", err, res.Body.String())
	}
	imported := result.Items[0]
	if imported.Backend != backendGoogleDrive || imported.StorageGroup != a.StorageGroup || imported.Region != "google" || imported.TenantID != "personal" || imported.DriveType != "my_drive" || imported.Verified {
		t.Fatalf("imported account: %+v", imported)
	}
	loaded, err := reader.r.loadCredential(imported.ID)
	if err != nil || credentialProvider(loaded) != backendGoogleDrive || loaded.Token.RefreshToken != c.Token.RefreshToken || loaded.GrantID == c.GrantID {
		t.Fatalf("credential: %v", err)
	}
	if err = validTransferEntry(transferEntry{Account: portableAccount(a), Credential: accountCredential{Mode: "rclone", Token: c.Token}}); err == nil {
		t.Fatal("provider mismatch accepted")
	}
	bad := c
	bad.Mode = "custom"
	if err = validTransferEntry(transferEntry{Account: portableAccount(a), Credential: bad}); err == nil {
		t.Fatal("Google custom mode accepted")
	}
}

func TestGoogleRecoveryPackageVersionOne(t *testing.T) {
	payload := RecoveryPayload{Version: 1, VaultID: strings.Repeat("1", 32), MasterKey: base64.StdEncoding.EncodeToString(make([]byte, 32))}
	payload.Entries = []DiscoveryEntry{{Backend: backendGoogleDrive, DriveID: "google-root", Region: "google", TenantID: "personal", Path: "VaultPilot/" + payload.VaultID + "/catalog/"}}
	kit, err := protectRecovery(payload, transferPassword)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openRecovery(kit, transferPassword)
	if err != nil || kit.Version != 1 || len(opened.Entries) != 1 || opened.Entries[0] != payload.Entries[0] {
		t.Fatalf("recovery roundtrip: %+v %v", opened, err)
	}
}

func TestCloudExportPreservesUnsupportedOneDriveRejection(t *testing.T) {
	h := newHarness(t, false)
	a := transferFixture(t, h, "rclone", "unsupported-drive")
	a.DriveType = "business"
	if _, err := h.r.Store.Save(t.Context(), "accounts", a.ID, a.Version, a); err != nil {
		t.Fatal(err)
	}
	res := h.call("POST", "/api/accounts/export", map[string]any{"admin_password": adminPassword, "password": transferPassword, "account_ids": []string{a.ID}}, true)
	if res.Code != 400 {
		t.Fatalf("unsupported OneDrive exported as personal: %d %s", res.Code, res.Body.String())
	}
}

func TestGoogleRcloneFailureClassificationAndHealthProbe(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	a := Account{ID: "google", Backend: backendGoogleDrive, DriveID: "root", Enabled: true, Verified: true}
	if _, err := h.r.Store.Save(t.Context(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{"403 userRateLimitExceeded", "403 rateLimitExceeded", "403 dailyLimitExceeded", "403 storageQuotaExceeded", "403 quotaExceeded", "403 upload limit reached"} {
		h.r.ObserveRcloneFailure(a.ID, []byte(output), errors.New("exit 1"))
	}
	if notificationCount(t, h.r, "") != 0 {
		t.Fatal("quota or throttle raised authorization alert")
	}
	for i, output := range []string{"authError", "invalid credentials", "insufficientPermissions", "insufficient authentication scopes"} {
		h.r.AccountAuthorizationHealthy(a.ID, true)
		h.r.ObserveRcloneFailure(a.ID, []byte(output), errors.New("exit 1"))
		if notificationCount(t, h.r, "") != i+1 {
			t.Fatalf("missed authorization marker %q", output)
		}
	}
	if err := h.r.saveCredential(a.ID, accountCredential{Provider: backendGoogleDrive, Mode: "rclone", Token: OAuthToken{AccessToken: "test", Expiry: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	called := false
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Host != "www.googleapis.com" || req.URL.Path != "/drive/v3/files/root" || req.Method != "GET" {
			t.Errorf("incorrect health endpoint: %s %s", req.Method, req.URL)
		}
		return cloudResponse(200, `{"id":"root"}`), nil
	})}
	h.r.checkAccountAuthorizations(t.Context())
	if !called || notificationCount(t, h.r, "") != 4 {
		t.Fatal("health probe did not use Google read access")
	}
}

func TestGoogleRefreshQuotaFailureDoesNotRaiseAuthorizationAlert(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	a := Account{ID: "google-refresh", Backend: backendGoogleDrive}
	if _, err := h.r.Store.Save(t.Context(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "rclone")
	h.r.Config.RcloneBin = program
	for _, reason := range []string{"userRateLimitExceeded", "rateLimitExceeded", "dailyLimitExceeded", "storageQuotaExceeded", "upload limit"} {
		if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' '403 "+reason+"' >&2\nexit 1\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := h.r.saveCredential(a.ID, accountCredential{Provider: backendGoogleDrive, Mode: "rclone", Token: OAuthToken{AccessToken: "expired", RefreshToken: "refresh", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}}); err != nil {
			t.Fatal(err)
		}
		_, err := h.r.accessToken(t.Context(), a.ID)
		var cloud cloudError
		if !errors.As(err, &cloud) || (!cloud.throttled() && cloud.Reason != "storageQuotaExceeded") {
			t.Fatalf("incorrect refresh failure classification for %s: %v", reason, err)
		}
		if notificationCount(t, h.r, "") != 0 {
			t.Fatalf("refresh quota failure raised authorization alert: %s", reason)
		}
	}
}
