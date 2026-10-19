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
