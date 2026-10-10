package platform

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func importToken(t *testing.T, expiry time.Time) string {
	t.Helper()
	b, e := json.Marshal(OAuthToken{AccessToken: "import-private-access", RefreshToken: "import-private-refresh", TokenType: "Bearer", Expiry: expiry})
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func TestRcloneImportFormatsAndUnsafeOptions(t *testing.T) {
	token := importToken(t, time.Now().Add(time.Hour))
	simple := map[string]string{"type": "onedrive", "token": token, "drive_type": "personal", "drive_id": "hint-only"}
	blob, _ := json.Marshal(simple)
	dump, _ := json.Marshal(map[string]any{"personal": simple, "local": map[string]string{"type": "local"}})
	formats := []string{token, string(dump), string(blob), base64.RawURLEncoding.EncodeToString(blob), "Paste the following into your remote machine --->\n" + token + "\n<---End paste", "\ufeff[personal]\ntype = onedrive\ntoken = " + token + "\n"}
	for _, input := range formats {
		entries, e := parseRcloneImport(input)
		if e != nil || len(entries) != 1 || entries[0].Credential.Mode != "rclone" {
			t.Fatalf("format failed: %v", e)
		}
	}
	cases := []string{
		"[x]\ntype=onedrive\ntoken=" + token + "\ntoken_url=http://169.254.169.254/token",
		"[x]\ntype=onedrive\ntoken=" + token + "\nauth_url=https://evil.example/",
		"[x]\ntype=onedrive\ntoken=" + token + "\nregion=cn",
		"[x]\ntype=onedrive\ntoken=" + token + "\ndrive_type=business",
		"[x]\ntype=onedrive\ntoken=" + token + "\nclient_credentials=true",
		"[x]\ntype=onedrive\ntoken=" + token + "\naccess_scopes=.default",
		"[x]\ntype=onedrive\ntoken=" + token + "\ntoken=another",
		"[x]\ntype=onedrive\ntoken=" + token + "\n[x]\ntype=onedrive",
		`{"access_token":"a","token_type":"Bearer"}`,
		"RCLONE_ENCRYPT_V0:\nabc", strings.Repeat("a", maxRcloneImport+1),
	}
	for i, input := range cases {
		if _, e := parseRcloneImport(input); e == nil {
			t.Fatalf("unsafe case %d accepted", i)
		}
	}
	// Unknown options cannot become engine flags or a config file directive.
	entries, e := parseRcloneImport("[x]\ntype=onedrive\ntoken=" + token + "\nproxy=http://evil.example\nencoding=Slash\n")
	if e != nil {
		t.Fatal(e)
	}
	h := newHarness(t, false)
	content, e := h.r.credentialConfig(context.Background(), "safe", entries[0].Credential, "", "")
	if e != nil || strings.Contains(content, "evil") || strings.Contains(content, "encoding") {
		t.Fatal("untrusted options forwarded")
	}
}
func TestRcloneImportEncryptedAtomicAndExplicitDrive(t *testing.T) {
	h := newHarness(t, false)
	h.r.Config.OAuthClientSecret = "global-private-secret"
	res := h.call("POST", "/api/accounts", map[string]string{"name": "Family"}, true)
	var a Account
	json.Unmarshal(res.Body.Bytes(), &a)
	config := "[first]\ntype=onedrive\ntoken=" + importToken(t, time.Now().Add(time.Hour)) + "\ndrive_id=untrusted-drive\ndrive_type=personal\n[second]\ntype=onedrive\ntoken=" + importToken(t, time.Now().Add(time.Hour)) + "\n"
	res = h.call("POST", "/api/rclone/import/preview", map[string]string{"config": config}, true)
	if res.Code != 200 || strings.Contains(res.Body.String(), "private") || strings.Contains(res.Body.String(), "token") {
		t.Fatal("preview leaked secrets")
	}
	path := "/api/accounts/" + a.ID + "/rclone/import"
	res = h.call("POST", path, map[string]any{"config": config, "version": a.Version}, true)
	if res.Code != 400 {
		t.Fatal("implicit remote accepted")
	}
	res = h.call("POST", path, map[string]any{"config": config, "version": a.Version, "remote": "first"}, false)
	if res.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	res = h.call("POST", path, map[string]any{"config": config, "version": a.Version + 1, "remote": "first"}, true)
	if res.Code != 409 {
		t.Fatal("stale import accepted")
	}
	if _, e := h.r.loadToken(a.ID); e == nil {
		t.Fatal("stale import wrote token")
	}
	res = h.call("POST", path, map[string]any{"config": config, "version": a.Version, "remote": "first"}, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	json.Unmarshal(res.Body.Bytes(), &a)
	if a.Verified || a.DriveID != "" || a.Status != "awaiting_drive_selection" || strings.Contains(res.Body.String(), "private") {
		t.Fatal("import trusted drive or exposed token")
	}
	raw, _ := h.r.meta("credential:" + a.ID)
	if strings.Contains(raw, "private") {
		t.Fatal("stored plaintext")
	}
	c, e := h.r.loadCredential(a.ID)
	if e != nil || c.Mode != "rclone" || c.ClientID != "" || c.ClientSecret != "" || c.GrantID == "" {
		t.Fatal("origin lost")
	}
	generated, e := h.r.credentialConfig(context.Background(), a.Remote, c, "real-drive", "personal")
	if e != nil || strings.Contains(generated, "global-private") || strings.Contains(generated, "test-client") || strings.Contains(generated, "client_id") {
		t.Fatal("global app contaminated standard credential")
	}
	if !strings.Contains(generated, "token_url = https://login.microsoftonline.com/common/oauth2/v2.0/token") {
		t.Fatal("standard refresh endpoint missing")
	}
	// API listing cannot expose imported credentials.
	res = h.call("GET", "/api/accounts", nil, false)
	if strings.Contains(res.Body.String(), "import-private") {
		t.Fatal("listing exposed tokens")
	}
	res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version}, true)
	if res.Code != 400 {
		t.Fatal("import bypassed explicit drive selection")
	}
	// Reopening the same node preserves origin without depending on globals.
	reopened, e := New(h.r.Config)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	reopened.Config.OAuthClientID = "different-global-client"
	got, e := reopened.loadCredential(a.ID)
	if e != nil || got.Mode != "rclone" || got.ClientID != "" || got.Token.AccessToken != c.Token.AccessToken {
		t.Fatal("restart lost credential")
	}
	// A refreshed config left over from the previous grant cannot overwrite it.
	old := c
	old.GrantID = "old-grant"
	old.Token.AccessToken = "stale-refreshed"
	old.Token.Expiry = time.Now().Add(2 * time.Hour)
	content, _ := h.r.credentialConfig(context.Background(), a.Remote, old, "real-drive", "personal")
	file, e := h.r.credentialFile("rclone-test-*.conf", content)
	if e != nil {
		t.Fatal(e)
	}
	defer os.Remove(file)
	if e = h.r.syncRcloneFile(file); e != nil {
		t.Fatal(e)
	}
	got, _ = h.r.loadCredential(a.ID)
	if got.Token.AccessToken != c.Token.AccessToken {
		t.Fatal("old grant replaced new authorization")
	}
}
func TestCustomCredentialRefreshUsesOriginalClient(t *testing.T) {
	h := newHarness(t, false)
	c := accountCredential{Token: OAuthToken{AccessToken: "expired", RefreshToken: "original-refresh", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}, Mode: "custom", ClientID: "original-client", ClientSecret: "original-secret", Tenant: "consumers", Scopes: oauthScope}
	if e := h.r.saveCredential("origin", c); e != nil {
		t.Fatal(e)
	}
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.ParseForm()
		if req.Form.Get("client_id") != "original-client" || req.Form.Get("client_secret") != "original-secret" || req.URL.Path != "/consumers/oauth2/v2.0/token" {
			t.Fatal("global credentials used for refresh")
		}
		return cloudResponse(200, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`), nil
	})}
	fresh, e := h.r.accessToken(context.Background(), "origin")
	if e != nil || fresh.AccessToken != "fresh" || fresh.RefreshToken != "original-refresh" {
		t.Fatal("refresh failed")
	}
	got, _ := h.r.loadCredential("origin")
	if got.ClientID != c.ClientID || got.ClientSecret != c.ClientSecret {
		t.Fatal("refresh erased client origin")
	}
}

func TestImportedRcloneRefreshPersistsOriginAndFiltersEnvironment(t *testing.T) {
	h := newHarness(t, false)
	bin := filepath.Join(t.TempDir(), "rclone-test-protocol")
	token := OAuthToken{AccessToken: "new-rclone-access", RefreshToken: "rotated-rclone-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}
	b, _ := json.Marshal(token)
	// A protocol fixture, not a cloud test: require the real noninteractive
	// command shape and private file, then simulate token rotation.
	script := `#!/bin/sh
if [ "$1" != config ] || [ "$2" != update ] || [ "$3" != vp_refresh ]; then exit 2; fi
if [ -n "$RCLONE_CONFIG_VP_REFRESH_CLIENT_ID" ]; then exit 3; fi
while [ "$#" -gt 0 ]; do
  if [ "$1" = --config ]; then shift; conf="$1"; fi
  if [ "$1" = --state ]; then shift; [ "$1" = onedrive ] || exit 4; fi
  shift
done
grep -q '^client_id = imported-client$' "$conf" || exit 5
grep -q 'vp_credential_id = imported-grant' "$conf" || exit 6
cat > "$conf" <<'CONFIG'
[vp_refresh]
type = onedrive
token = ` + string(b) + `
CONFIG
printf '%s\n' '{"State":"driveid_final","Error":""}'
`
	if e := os.WriteFile(bin, []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	h.r.Config.RcloneBin = bin
	t.Setenv("RCLONE_CONFIG_VP_REFRESH_CLIENT_ID", "contaminated-global")
	c := accountCredential{Mode: "rclone", ClientID: "imported-client", GrantID: "imported-grant", Scopes: "Files.ReadWrite offline_access", Token: OAuthToken{AccessToken: "expired", RefreshToken: "old-rclone-refresh", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)}}
	if e := h.r.saveCredential("imported", c); e != nil {
		t.Fatal(e)
	}
	got, e := h.r.accessToken(context.Background(), "imported")
	if e != nil || got.AccessToken != token.AccessToken || got.RefreshToken != token.RefreshToken {
		t.Fatalf("refresh: %v", e)
	}
	stored, e := h.r.loadCredential("imported")
	if e != nil || stored.ClientID != c.ClientID || stored.GrantID != c.GrantID || stored.Scopes != c.Scopes || stored.Mode != "rclone" {
		t.Fatal("refresh lost imported origin")
	}
	files, _ := filepath.Glob(filepath.Join(h.r.Config.CacheDir, "oauth-refresh-*.conf"))
	if len(files) != 0 {
		t.Fatal("refresh left plaintext config")
	}
}
func TestRcloneCallbackValidationAndSessionBinding(t *testing.T) {
	good := "http://localhost:53682/?code=code&state=expected"
	if _, e := validateRcloneCallback(good, "expected"); e != nil {
		t.Fatal(e)
	}
	invalid := []string{"https://evil.example/?code=x&state=expected", "http://169.254.169.254:53682/?code=x&state=expected", "http://localhost:53682/auth?code=x&state=expected", "http://localhost:53682/?code=x&state=wrong", good + "&state=expected", good + "#fragment", good + "&error=denied", "http://user@localhost:53682/?code=x&state=expected"}
	for _, s := range invalid {
		if _, e := validateRcloneCallback(s, "expected"); e == nil {
			t.Fatal("unsafe callback accepted")
		}
	}
	h := newHarness(t, false)
	_, sid, _ := h.r.session(requestWithCookie(h))
	f := &rcloneAuthFlow{ID: "flow", AccountID: "account", SessionID: sid, Status: "waiting", State: "expected", Expires: time.Now().Add(time.Minute), cancel: func() {}}
	h.r.rcloneAuth = map[string]*rcloneAuthFlow{"flow": f}
	path := "/api/accounts/account/rclone/authorize?flow_id=flow"
	res := h.call("GET", path, nil, false)
	if res.Code != 200 || strings.Contains(res.Body.String(), "expected") || strings.Contains(res.Body.String(), sid) {
		t.Fatal("flow exposed private state")
	}
	f.SessionID = "another-session"
	res = h.call("GET", path, nil, false)
	if res.Code != 404 {
		t.Fatal("other session accessed flow")
	}
	f.SessionID = sid
	res = h.call("POST", "/api/accounts/account/rclone/callback?flow_id=flow", map[string]string{"url": invalid[0]}, true)
	if res.Code != 400 || f.Status != "waiting" {
		t.Fatal("invalid URL reached listener")
	}
	f.Expires = time.Now().Add(-time.Second)
	res = h.call("POST", "/api/accounts/account/rclone/callback?flow_id=flow", map[string]string{"url": good}, true)
	if res.Code != 409 {
		t.Fatal("expired flow accepted")
	}
	f.Expires = time.Now().Add(time.Minute)
	f.Status = "exchanging"
	res = h.call("POST", "/api/accounts/account/rclone/callback?flow_id=flow", map[string]string{"url": good}, true)
	if res.Code != 409 {
		t.Fatal("replayed callback accepted")
	}
	f.Status = "completed"
}
func requestWithCookie(h *testHarness) *http.Request {
	req, _ := http.NewRequest("GET", "http://localhost/", nil)
	req.AddCookie(h.cookie)
	return req
}

func TestRealRcloneStandardAuthorizeAndCancel(t *testing.T) {
	bin := os.Getenv("VP_TEST_RCLONE")
	if bin == "" {
		t.Skip("set VP_TEST_RCLONE to test the real loopback authorization process")
	}
	h := newHarness(t, false)
	h.r.Config.RcloneBin = bin
	res := h.call("POST", "/api/accounts", map[string]string{"name": "Real Rclone probe"}, true)
	var a Account
	json.Unmarshal(res.Body.Bytes(), &a)
	res = h.call("POST", "/api/accounts/"+a.ID+"/rclone/authorize", map[string]int{"version": a.Version}, true)
	if res.Code != 202 {
		t.Fatal(res.Body.String())
	}
	var f map[string]any
	json.Unmarshal(res.Body.Bytes(), &f)
	path := "/api/accounts/" + a.ID + "/rclone/authorize?flow_id=" + f["id"].(string)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res = h.call("GET", path, nil, false)
		json.Unmarshal(res.Body.Bytes(), &f)
		if f["status"] == "waiting" || f["status"] == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f["status"] != "waiting" {
		t.Fatalf("real process failed: %v", f)
	}
	u, e := url.Parse(f["authorization_url"].(string))
	if e != nil || u.Host != "login.microsoftonline.com" || u.Query().Get("client_id") == "test-client" || u.Query().Get("redirect_uri") != "http://localhost:53682/" {
		t.Fatal("not standard Rclone authorization")
	}
	res = h.call("DELETE", path, map[string]any{}, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	for time.Now().Before(deadline) {
		res = h.call("GET", path, nil, false)
		json.Unmarshal(res.Body.Bytes(), &f)
		if f["status"] == "cancelled" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f["status"] != "cancelled" {
		t.Fatal("process not cancelled")
	}
	files, _ := filepath.Glob(filepath.Join(h.r.Config.CacheDir, "oauth-authorize-*.conf"))
	if len(files) != 0 {
		t.Fatal("auth temp file not cleaned")
	}
	if _, e = h.r.loadToken(a.ID); e == nil {
		t.Fatal("cancel created credentials")
	}
	// Exercise the remote relay with a real Rclone denial callback. No code
	// exchange or Microsoft login is attempted.
	res = h.call("POST", "/api/accounts/"+a.ID+"/rclone/authorize", map[string]int{"version": a.Version}, true)
	if res.Code != 202 {
		t.Fatal(res.Body.String())
	}
	json.Unmarshal(res.Body.Bytes(), &f)
	path = "/api/accounts/" + a.ID + "/rclone/authorize?flow_id=" + f["id"].(string)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		res = h.call("GET", path, nil, false)
		json.Unmarshal(res.Body.Bytes(), &f)
		if f["status"] == "waiting" || f["status"] == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f["status"] != "waiting" {
		t.Fatal("second authorization did not start")
	}
	u, _ = url.Parse(f["authorization_url"].(string))
	callback := "http://localhost:53682/?error=access_denied&state=" + url.QueryEscape(u.Query().Get("state"))
	res = h.call("POST", "/api/accounts/"+a.ID+"/rclone/callback?flow_id="+f["id"].(string), map[string]string{"url": callback}, true)
	if res.Code != 202 {
		t.Fatal(res.Body.String())
	}
	for time.Now().Before(deadline) {
		res = h.call("GET", path, nil, false)
		json.Unmarshal(res.Body.Bytes(), &f)
		if f["status"] == "failed" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if f["status"] != "failed" {
		t.Fatal("denial did not end flow")
	}
	if _, e = h.r.loadToken(a.ID); e == nil {
		t.Fatal("denial created credentials")
	}
}

func TestRealRclonePreservesCredentialGeneration(t *testing.T) {
	bin := os.Getenv("VP_TEST_RCLONE")
	if bin == "" {
		t.Skip("set VP_TEST_RCLONE to validate generated configuration with the actual engine")
	}
	h := newHarness(t, false)
	h.r.Config.RcloneBin = bin
	c := accountCredential{Mode: "rclone", GrantID: "test-generation", Token: OAuthToken{AccessToken: "configuration-test-only-access", RefreshToken: "configuration-test-only-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
	content, e := h.r.credentialConfig(context.Background(), "vp_probe", c, "", "")
	if e != nil {
		t.Fatal(e)
	}
	path, e := h.r.credentialFile("rclone-probe-*.conf", content)
	if e != nil {
		t.Fatal(e)
	}
	defer os.Remove(path)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "config", "update", "vp_probe", "config_refresh_token=false", "--non-interactive", "--config", path)
	cmd.Env = rcloneEnvironment()
	output, e := cmd.Output()
	if e != nil {
		t.Fatal("Rclone rejected the generated configuration")
	}
	var result struct{ State, Error string }
	if json.Unmarshal(output, &result) != nil || result.Error != "" || result.State != "choose_type_done" {
		t.Fatalf("unexpected noninteractive state %s / %s", result.State, result.Error)
	}
	// Stops before querying any cloud drive. Real Rclone rewrites the file and
	// must preserve the generation marker used to isolate refresh results.
	raw, e := os.ReadFile(path)
	if e != nil || !strings.Contains(string(raw), "vp_credential_id = test-generation") {
		t.Fatal("Rclone dropped credential generation")
	}
}
