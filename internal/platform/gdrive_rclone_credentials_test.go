package platform

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func googleRcloneAccount(t *testing.T, h *testHarness) Account {
	t.Helper()
	a := Account{ID: newID(), Name: "Google", Backend: backendGoogleDrive, Remote: "vp_google", Version: 1}
	if _, err := h.r.Store.Save(t.Context(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGoogleRcloneImportFormatsAndConfigAllowlist(t *testing.T) {
	token := importToken(t, time.Now().UTC().Add(time.Hour))
	secret := base64.RawURLEncoding.EncodeToString([]byte("0123456789abcdefobscured-secret"))
	remote := map[string]string{"type": "drive", "token": token, "scope": "drive", "root_folder_id": "hint-only", "client_id": "imported.apps.googleusercontent.com", "client_secret": secret, "use_trash": "true", "stop_on_upload_limit": "false", "chunk_size": "256Mi", "auth_url": "https://accounts.google.com/o/oauth2/auth", "token_url": "https://oauth2.googleapis.com/token"}
	blob, _ := json.Marshal(remote)
	dump, _ := json.Marshal(map[string]any{"google": remote, "microsoft": map[string]string{"type": "onedrive"}})
	conf := "[google]\ntype = drive\ntoken = " + token + "\nroot_folder_id = hint-only\nuse_trash = true\nstop_on_upload_limit = false\n"
	formats := []string{token, string(blob), string(dump), base64.RawURLEncoding.EncodeToString(blob), "Paste the following into your remote machine --->\n" + token + "\n<---End paste", conf}
	r := &Runtime{}
	for i, input := range formats {
		entries, err := parseRcloneImport(input, backendGoogleDrive)
		if err != nil || len(entries) != 1 {
			t.Fatalf("format %d: %v", i, err)
		}
		entry := entries[0]
		if entry.Credential.Mode != "rclone" || credentialProvider(entry.Credential) != backendGoogleDrive || entry.Credential.Scopes != "drive" {
			t.Fatalf("format %d lost provider, mode or scope", i)
		}
		content, err := r.credentialConfig(t.Context(), "safe", entry.Credential, "verified-root", "my_drive")
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{"type = drive\n", "scope = drive\n", "root_folder_id = verified-root\n", "use_trash = false\n", "stop_on_upload_limit = true\n"} {
			if !strings.Contains(content, required) {
				t.Fatalf("format %d missing %q", i, required)
			}
		}
		allowed := map[string]bool{"type": true, "scope": true, "token": true, "vp_credential_id": true, "client_id": true, "client_secret": true, "root_folder_id": true, "stop_on_upload_limit": true, "use_trash": true}
		for _, line := range strings.Split(content, "\n") {
			key, _, ok := strings.Cut(line, "=")
			if ok && !allowed[strings.TrimSpace(key)] {
				t.Fatalf("untrusted setting in Google config: %s", key)
			}
		}
		if strings.Contains(content, "hint-only") || strings.Contains(content, "use_trash = true") {
			t.Fatal("imported runtime settings or root hint became trusted")
		}
	}
	entries, err := parseRcloneImport(string(blob), backendGoogleDrive)
	if err != nil || entries[0].DriveID != "hint-only" || !entries[0].CustomClient {
		t.Fatal("preview lost root hint or custom client indicator")
	}
	if _, err := parseRcloneImport(conf, backendOneDrive); err == nil || !strings.Contains(err.Error(), "OneDrive") {
		t.Fatal("OneDrive import accepted Google config")
	}
	if _, err := parseRcloneImport("[x]\ntype=onedrive\ntoken="+token, backendGoogleDrive); err == nil || !strings.Contains(err.Error(), "Google Drive") {
		t.Fatal("Google import accepted OneDrive config")
	}
}

func TestGoogleRcloneImportRejectsUnsupportedSettings(t *testing.T) {
	token := importToken(t, time.Now().UTC().Add(time.Hour))
	invalid := []struct{ key, value string }{
		{"scope", "drive.file"}, {"scope", "drive.readonly"}, {"scope", "drive drive.file"},
		{"team_drive", "shared"}, {"service_account_file", "/tmp/service.json"}, {"service_account_credentials", "{}"},
		{"impersonate", "person@example.com"}, {"resource_key", "resource"}, {"env_auth", "true"}, {"env_auth", "1"}, {"shared_with_me", "TRUE"},
		{"auth_url", "https://evil.example/auth"}, {"auth_url", "https://accounts.google.com/o/oauth2/auth?evil=1"},
		{"token_url", "http://oauth2.googleapis.com/token"}, {"token_url", "https://evil.example/token"},
		{"client_secret", "plaintext-secret"}, {"client_id", "line\nbreak"}, {"scope", strings.Repeat("a", 65537)},
	}
	for _, test := range invalid {
		t.Run(test.key+"="+test.value[:min(50, len(test.value))], func(t *testing.T) {
			config, _ := json.Marshal(map[string]string{"type": "drive", "token": token, test.key: test.value})
			if _, err := parseRcloneImport(string(config), backendGoogleDrive); err == nil {
				t.Fatal("unsupported Google setting accepted")
			}
		})
	}
	for _, token := range []string{`{"access_token":"a","token_type":"Bearer","expiry":"2030-01-01T00:00:00Z"}`, `{"access_token":"a","refresh_token":"r","token_type":"Basic","expiry":"2030-01-01T00:00:00Z"}`, `{"access_token":"a\nb","refresh_token":"r","token_type":"Bearer","expiry":"2030-01-01T00:00:00Z"}`} {
		if _, err := parseRcloneImport(token, backendGoogleDrive); err == nil {
			t.Fatal("invalid Google token accepted")
		}
	}
}

func TestGoogleRcloneCredentialProviderAndImportAccountBackend(t *testing.T) {
	h := newHarness(t, false)
	a := googleRcloneAccount(t, h)
	c := accountCredential{Mode: "rclone", Token: OAuthToken{AccessToken: "access"}}
	if _, err := h.r.installCredential(t.Context(), a.ID, a.Version, c, "test"); err == nil || err.Error() != "授权类型与账号不匹配" {
		t.Fatal("OneDrive credential accepted by Google account")
	}
	if _, err := h.r.loadCredential(a.ID); err == nil {
		t.Fatal("provider mismatch wrote credentials")
	}
	c.Provider = backendGoogleDrive
	c.Mode = "custom"
	if err := h.r.saveCredential("invalid-mode", c); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.loadCredential("invalid-mode"); err == nil {
		t.Fatal("Google custom web OAuth credential accepted")
	}
	c.Mode = "rclone"
	c.Provider = "unknown"
	if err := h.r.saveCredential("invalid-provider", c); err != nil {
		t.Fatal(err)
	}
	if _, err := h.r.loadCredential("invalid-provider"); err == nil {
		t.Fatal("unknown provider accepted")
	}
	token := importToken(t, time.Now().UTC().Add(time.Hour))
	config := "[google]\ntype=drive\nroot_folder_id=hint-only\ntoken=" + token
	preview := h.call("POST", "/api/rclone/import/preview", map[string]string{"config": config, "backend": backendGoogleDrive}, true)
	if preview.Code != 200 || strings.Contains(preview.Body.String(), "private") {
		t.Fatalf("Google preview failed or exposed token: %d", preview.Code)
	}
	res := h.call("POST", "/api/accounts/"+a.ID+"/rclone/import", map[string]any{"config": config, "backend": backendOneDrive, "remote": "google", "version": a.Version}, true)
	if res.Code != 200 {
		t.Fatalf("request backend overrode stored account backend: %s", res.Body.String())
	}
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if a.DriveID != "" || a.Verified || a.Status != "awaiting_drive_selection" {
		t.Fatal("Google import trusted root hint or bypassed verification")
	}
	stored, err := h.r.loadCredential(a.ID)
	if err != nil || stored.Provider != backendGoogleDrive || stored.GrantID == "" {
		t.Fatal("Google provider or grant lost in encrypted storage")
	}
}

func TestGoogleRcloneRefreshForcesExpiryAndReadsBackToken(t *testing.T) {
	h := newHarness(t, false)
	dir := t.TempDir()
	bin, capture := filepath.Join(dir, "rclone"), filepath.Join(dir, "captured.conf")
	fresh := OAuthToken{AccessToken: "fresh-google-access", RefreshToken: "fresh-google-refresh", TokenType: "Bearer", Expiry: time.Now().UTC().Add(time.Hour)}
	token, _ := json.Marshal(fresh)
	script := `#!/bin/sh
[ "$1" = about ] && [ "$2" = vp_refresh: ] && [ "$3" = --json ] || exit 2
while [ "$#" -gt 0 ]; do
  if [ "$1" = --config ]; then shift; conf="$1"; fi
  shift
done
grep -q '^type = drive$' "$conf" || exit 3
grep -q '^use_trash = false$' "$conf" || exit 4
grep -q '^stop_on_upload_limit = true$' "$conf" || exit 5
cp "$conf" '` + capture + `'
cat > "$conf" <<'CONFIG'
[vp_refresh]
type = drive
token = ` + string(token) + `
CONFIG
printf '%s\n' '{"total":100,"free":50,"used":50}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h.r.Config.RcloneBin = bin
	c := accountCredential{Provider: backendGoogleDrive, Mode: "rclone", GrantID: "google-grant", ClientID: "imported.apps.googleusercontent.com", Scopes: "drive", Token: OAuthToken{AccessToken: "still-valid", RefreshToken: "old-refresh", TokenType: "Bearer", Expiry: time.Now().UTC().Add(time.Minute)}}
	if err := h.r.saveCredential("google-refresh", c); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	got, err := h.r.accessToken(t.Context(), "google-refresh")
	if err != nil || got != fresh {
		t.Fatalf("Google refresh failed: %v", err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := parseRcloneImport(string(raw), backendGoogleDrive)
	if err != nil || !entries[0].Credential.Token.Expiry.Before(started) {
		t.Fatal("Google refresh did not force token expiry")
	}
	stored, err := h.r.loadCredential("google-refresh")
	if err != nil || stored.Token != fresh || stored.Provider != c.Provider || stored.GrantID != c.GrantID || stored.ClientID != c.ClientID || stored.Scopes != c.Scopes {
		t.Fatal("Google refresh lost origin or rotated token")
	}
	files, _ := filepath.Glob(filepath.Join(h.r.Config.CacheDir, "oauth-refresh-*.conf"))
	if len(files) != 0 {
		t.Fatal("Google refresh left plaintext credentials")
	}
}

func TestGoogleRcloneRefreshFailureClassification(t *testing.T) {
	h := newHarness(t, false)
	for _, test := range []struct{ output, category string }{{"invalid_grant: token revoked", "authorization_required"}, {"network unavailable", "refresh_failed"}} {
		t.Run(test.category, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "rclone")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' '"+test.output+"' >&2\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			h.r.Config.RcloneBin = bin
			c := accountCredential{Provider: backendGoogleDrive, Mode: "rclone", Token: OAuthToken{AccessToken: "old", RefreshToken: "refresh", TokenType: "Bearer", Expiry: time.Now().UTC().Add(-time.Hour)}}
			if err := h.r.saveCredential(test.category, c); err != nil {
				t.Fatal(err)
			}
			_, err := h.r.accessToken(t.Context(), test.category)
			var issue authorizationIssue
			if !errors.As(err, &issue) || issue.category != test.category {
				t.Fatalf("wrong Google refresh error category: %v", err)
			}
			stored, err := h.r.loadCredential(test.category)
			if err != nil || stored.Token != c.Token {
				t.Fatal("failed Google refresh changed saved token")
			}
		})
	}
}
