package platform

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func googleAuthorizationURL() *url.URL {
	return &url.URL{Scheme: "https", Host: "accounts.google.com", Path: "/o/oauth2/auth", RawQuery: url.Values{
		"state": {"google-test-state"}, "redirect_uri": {"http://127.0.0.1:53682/"}, "response_type": {"code"},
		"access_type": {"offline"}, "scope": {"https://www.googleapis.com/auth/drive"},
		"client_id": {"202264815644.apps.googleusercontent.com"},
	}.Encode()}
}

func TestGoogleRcloneAuthorizationURLValidation(t *testing.T) {
	auth, err := validatedRcloneAuthURL(googleAuthorizationURL().String(), "google-test-state", backendGoogleDrive)
	if err != nil || auth.Query().Get("prompt") != "consent select_account" {
		t.Fatal("Google authorization did not require consent and account selection")
	}
	invalid := []struct {
		name   string
		mutate func(*url.URL)
	}{
		{"scheme", func(u *url.URL) { u.Scheme = "http" }},
		{"host", func(u *url.URL) { u.Host = "accounts.google.com.evil.example" }},
		{"path", func(u *url.URL) { u.Path = "/o/oauth2/v2/auth" }},
		{"userinfo", func(u *url.URL) { u.User = url.User("user") }},
		{"fragment", func(u *url.URL) { u.Fragment = "redirect" }},
		{"redirect", func(u *url.URL) {
			q := u.Query()
			q.Set("redirect_uri", "http://localhost:53682/")
			u.RawQuery = q.Encode()
		}},
		{"scope", func(u *url.URL) {
			q := u.Query()
			q.Set("scope", "https://www.googleapis.com/auth/drive.file")
			u.RawQuery = q.Encode()
		}},
		{"client", func(u *url.URL) { q := u.Query(); q.Set("client_id", "evil-client"); u.RawQuery = q.Encode() }},
		{"client-suffix", func(u *url.URL) {
			q := u.Query()
			q.Set("client_id", "good.apps.googleusercontent.com.evil.example")
			u.RawQuery = q.Encode()
		}},
		{"state", func(u *url.URL) { q := u.Query(); q.Set("state", "wrong"); u.RawQuery = q.Encode() }},
		{"response-type", func(u *url.URL) { q := u.Query(); q.Set("response_type", "token"); u.RawQuery = q.Encode() }},
		{"access-type", func(u *url.URL) { q := u.Query(); q.Set("access_type", "online"); u.RawQuery = q.Encode() }},
		{"duplicate-scope", func(u *url.URL) {
			q := u.Query()
			q.Add("scope", "https://www.googleapis.com/auth/drive.file")
			u.RawQuery = q.Encode()
		}},
		{"duplicate-client", func(u *url.URL) {
			q := u.Query()
			q.Add("client_id", "evil.apps.googleusercontent.com")
			u.RawQuery = q.Encode()
		}},
		{"bad-query", func(u *url.URL) { u.RawQuery += "&bad=%" }},
	}
	for _, test := range invalid {
		t.Run(test.name, func(t *testing.T) {
			u := googleAuthorizationURL()
			test.mutate(u)
			if _, err := validatedRcloneAuthURL(u.String(), "google-test-state", backendGoogleDrive); err == nil {
				t.Fatal("unsafe Google authorization URL accepted")
			}
		})
	}
	microsoft := "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?state=state&redirect_uri=http%3A%2F%2Flocalhost%3A53682%2F"
	auth, err = validatedRcloneAuthURL(microsoft, "state", backendOneDrive)
	if err != nil || auth.Query().Get("prompt") != "select_account" {
		t.Fatal("OneDrive authorization behavior changed")
	}
	if _, err := validatedRcloneAuthURL(microsoft, "state", backendGoogleDrive); err == nil {
		t.Fatal("Google authorization accepted Microsoft destination")
	}
}

func TestGoogleRcloneAuthorizationCustomClientValidation(t *testing.T) {
	base := googleAuthorizationURL()
	for _, test := range []struct {
		name string
		id   string
		mut  func(*url.URL)
	}{
		{"mismatch", "custom.apps.googleusercontent.com", nil},
		{"unknown-query", "", func(u *url.URL) {
			q := u.Query()
			q.Set("client_secret", "should-never-be-in-url")
			u.RawQuery = q.Encode()
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			u := *base
			q := u.Query()
			if test.id != "" {
				q.Set("client_id", test.id)
			}
			u.RawQuery = q.Encode()
			if test.mut != nil {
				test.mut(&u)
			}
			if _, err := validatedRcloneAuthURL(u.String(), "google-test-state", backendGoogleDrive, "other.apps.googleusercontent.com"); err == nil {
				t.Fatal("unexpected Google client accepted")
			}
		})
	}
	good, err := validatedRcloneAuthURL(base.String(), "google-test-state", backendGoogleDrive)
	if err != nil || good.Query().Get("client_secret") != "" {
		t.Fatal("standard URL exposed client secret or failed validation")
	}
	custom := *base
	q := custom.Query()
	q.Set("client_id", "custom.apps.googleusercontent.com")
	custom.RawQuery = q.Encode()
	if got, err := validatedRcloneAuthURL(custom.String(), "google-test-state", backendGoogleDrive, "custom.apps.googleusercontent.com"); err != nil || got.Query().Get("client_id") != "custom.apps.googleusercontent.com" {
		t.Fatalf("custom client URL rejected: %v", err)
	}
}

func googleString(value string) *string { return &value }

func TestGoogleRcloneAuthorizationClientPair(t *testing.T) {
	h := newHarness(t, false)
	a := googleRcloneAccount(t, h)
	if id, secret, err := h.r.rcloneAuthorizationClient(a, googleString("custom.apps.googleusercontent.com"), googleString("GOCSPX-secret")); err != nil || id == "" || secret != "GOCSPX-secret" {
		t.Fatalf("custom client pair rejected: %q %q %v", id, secret, err)
	}
	if _, _, err := h.r.rcloneAuthorizationClient(a, googleString("custom.apps.googleusercontent.com"), googleString("")); err == nil {
		t.Fatal("explicitly empty client secret accepted")
	}
	if _, _, err := h.r.rcloneAuthorizationClient(a, nil, nil); err != nil {
		t.Fatalf("omitted client pair did not select reuse/shared: %v", err)
	}
}

func TestGoogleRcloneCustomAuthorizationPassesClientOnlyThroughEnvironmentAndReusesIt(t *testing.T) {
	h := newHarness(t, false)
	a := googleRcloneAccount(t, h)
	dir := t.TempDir()
	capture := filepath.Join(dir, "environment")
	bin := filepath.Join(dir, "rclone")
	token := importToken(t, time.Now().Add(time.Hour))
	const clientID = "custom.apps.googleusercontent.com"
	const clientSecret = "GOCSPX-test-secret"
	script := `#!/bin/sh
[ "$1" = authorize ] && [ "$2" = drive ] && [ "$3" = --auth-no-open-browser ] || exit 2
for arg in "$@"; do
  case "$arg" in
    *GOCSPX-test-secret*|*custom.apps.googleusercontent.com*) exit 3 ;;
  esac
done
printf '%s|%s|%s\n' "$RCLONE_DRIVE_CLIENT_ID" "$RCLONE_DRIVE_CLIENT_SECRET" "$RCLONE_DRIVE_SCOPE" > '` + capture + `'
printf '%s\n' '` + token + `'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h.r.Config.RcloneBin = bin
	t.Setenv("RCLONE_DRIVE_CLIENT_ID", "host-client.apps.googleusercontent.com")
	t.Setenv("RCLONE_DRIVE_CLIENT_SECRET", "host-secret")
	start := func(body any) (map[string]any, Account) {
		t.Helper()
		res := h.call("POST", "/api/accounts/"+a.ID+"/rclone/authorize", body, true)
		if res.Code != http.StatusAccepted {
			t.Fatal(res.Body.String())
		}
		var initial map[string]any
		if err := json.Unmarshal(res.Body.Bytes(), &initial); err != nil {
			t.Fatal(err)
		}
		if initial["custom_client"] != true {
			t.Fatalf("custom client flag missing: %s", res.Body.String())
		}
		flowID := initial["id"].(string)
		path := "/api/accounts/" + a.ID + "/rclone/authorize?flow_id=" + flowID
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			res = h.call("GET", path, nil, false)
			var current map[string]any
			if err := json.Unmarshal(res.Body.Bytes(), &current); err != nil {
				t.Fatal(err)
			}
			if current["custom_client"] != true {
				t.Fatalf("custom client flag missing from flow view: %s", res.Body.String())
			}
			if current["status"] == "completed" {
				var account Account
				if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &account); err != nil {
					t.Fatal(err)
				}
				return current, account
			}
			if current["status"] == "failed" {
				t.Fatalf("custom Google authorization failed: %v", current["message"])
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("custom Google authorization did not complete")
		return nil, Account{}
	}
	_, current := start(map[string]any{"version": a.Version, "client_id": clientID, "client_secret": clientSecret})
	got, err := os.ReadFile(capture)
	if err != nil || string(got) != clientID+"|"+clientSecret+"|drive\n" {
		t.Fatalf("custom client was not isolated in child environment: %q (%v)", got, err)
	}
	credential, err := h.r.loadCredential(a.ID)
	if err != nil || credential.ClientID != clientID || credential.ClientSecret != clientSecret {
		t.Fatalf("custom client was not saved with token: %+v (%v)", credential, err)
	}
	_, _ = start(map[string]any{"version": current.Version})
	got, err = os.ReadFile(capture)
	if err != nil || string(got) != clientID+"|"+clientSecret+"|drive\n" {
		t.Fatalf("saved custom client was not reused: %q (%v)", got, err)
	}
}
func TestGoogleRcloneCallbackProviderMessage(t *testing.T) {
	good := "http://127.0.0.1:53682/?code=code&state=expected"
	if _, err := validateRcloneCallback(good, "expected", backendGoogleDrive); err != nil {
		t.Fatal(err)
	}
	_, err := validateRcloneCallback("https://evil.example/?code=code&state=expected", "expected", backendGoogleDrive)
	if err == nil || !strings.Contains(err.Error(), "Google") || !strings.Contains(err.Error(), "http://127.0.0.1:53682/") || strings.Contains(err.Error(), "微软") {
		t.Fatal("Google callback error has the wrong provider or address")
	}
}

func TestGoogleRcloneCustomAuthorizationStoresClientWithoutArgvSecret(t *testing.T) {
	h := newHarness(t, false)
	a := googleRcloneAccount(t, h)
	dir := t.TempDir()
	bin := filepath.Join(dir, "rclone")
	clientID := "custom.apps.googleusercontent.com"
	clientSecret := "GOCSPX-test-secret"
	token := importToken(t, time.Now().Add(time.Hour))
	script := `#!/bin/sh
[ "$1" = authorize ] && [ "$2" = drive ] && [ "$3" = --auth-no-open-browser ] || exit 2
for arg in "$@"; do test "$arg" != "` + clientSecret + `" || exit 3; done
test "$RCLONE_DRIVE_CLIENT_ID" = "` + clientID + `" || exit 4
test "$RCLONE_DRIVE_CLIENT_SECRET" = "` + clientSecret + `" || exit 5
test "$RCLONE_DRIVE_SCOPE" = drive || exit 6
test -z "$RCLONE_CONFIG_VP_REFRESH_CLIENT_SECRET" || exit 7
printf '%s\n' '` + token + `'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h.r.Config.RcloneBin = bin
	t.Setenv("RCLONE_DRIVE_CLIENT_ID", "host-client.apps.googleusercontent.com")
	t.Setenv("RCLONE_DRIVE_CLIENT_SECRET", "host-secret")
	res := h.call("POST", "/api/accounts/"+a.ID+"/rclone/authorize", map[string]any{"version": a.Version, "client_id": clientID, "client_secret": clientSecret}, true)
	if res.Code != http.StatusAccepted || strings.Contains(res.Body.String(), clientSecret) {
		t.Fatalf("custom authorization request leaked or failed: %d %s", res.Code, res.Body.String())
	}
	var flow map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &flow); err != nil {
		t.Fatal(err)
	}
	if flow["custom_client"] != true {
		t.Fatalf("custom client marker missing: %v", flow)
	}
	path := "/api/accounts/" + a.ID + "/rclone/authorize?flow_id=" + flow["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res = h.call("GET", path, nil, false)
		if strings.Contains(res.Body.String(), clientSecret) {
			t.Fatal("authorization status exposed client secret")
		}
		if json.Unmarshal(res.Body.Bytes(), &flow) != nil {
			t.Fatal(res.Body.String())
		}
		if flow["status"] == "completed" {
			break
		}
		if flow["status"] == "failed" {
			t.Fatalf("custom authorization failed: %v", flow["message"])
		}
		time.Sleep(10 * time.Millisecond)
	}
	if flow["status"] != "completed" {
		t.Fatalf("custom authorization did not complete: %v", flow)
	}
	credential, err := h.r.loadCredential(a.ID)
	if err != nil || credential.ClientID != clientID || credential.ClientSecret != clientSecret || credential.Token.RefreshToken == "" {
		t.Fatalf("custom client was not bound to saved token: %+v %v", credential, err)
	}
}

func TestGoogleRcloneStandardAuthorizationCompletesAndStoresProvider(t *testing.T) {
	h := newHarness(t, false)
	a := googleRcloneAccount(t, h)
	dir := t.TempDir()
	token := importToken(t, time.Now().Add(time.Hour))
	bin := filepath.Join(dir, "rclone")
	script := `#!/bin/sh
[ "$1" = authorize ] && [ "$2" = drive ] && [ "$3" = --auth-no-open-browser ] || exit 2
printf '%s\n' '` + token + `'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h.r.Config.RcloneBin = bin
	res := h.call("POST", "/api/accounts/"+a.ID+"/rclone/authorize", map[string]int{"version": a.Version}, true)
	if res.Code != http.StatusAccepted {
		t.Fatal(res.Body.String())
	}
	var flow map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &flow); err != nil {
		t.Fatal(err)
	}
	flowID := flow["id"].(string)
	path := "/api/accounts/" + a.ID + "/rclone/authorize?flow_id=" + flowID
	wait := func(expected string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			res = h.call("GET", path, nil, false)
			if err := json.Unmarshal(res.Body.Bytes(), &flow); err != nil {
				t.Fatal(err)
			}
			if flow["status"] == expected {
				return
			}
			if flow["status"] == "failed" {
				t.Fatalf("Google authorization failed: %v", flow["message"])
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("Google authorization did not reach %s: %v", expected, flow["status"])
	}
	wait("completed")
	credential, err := h.r.loadCredential(a.ID)
	if err != nil || credential.Provider != backendGoogleDrive || credential.Mode != "rclone" || credential.Token.RefreshToken == "" {
		t.Fatal("completed Google authorization lost credential provider")
	}
	if credential.ClientID != "" || credential.ClientSecret != "" {
		t.Fatal("standard Google authorization used global Microsoft application settings")
	}
	files, _ := filepath.Glob(filepath.Join(h.r.Config.CacheDir, "oauth-authorize-*.conf"))
	if len(files) != 0 {
		t.Fatal("Google authorization left plaintext temporary configuration")
	}
}
