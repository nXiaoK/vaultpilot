package platform

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
)

func TestHostAllowlistBlocksRebinding(t *testing.T) {
	dir := t.TempDir()
	r, err := New(Config{StateDir: filepath.Join(dir, "state"), PublicURL: "https://vault.example:8443", AllowedHosts: []string{"192.168.1.20:8080", "nas.example"}})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mux := http.NewServeMux()
	r.Register(mux)
	h := r.Handler(mux)
	request := httptest.NewRequest("POST", "/api/setup", strings.NewReader(`{"username":"attacker","password":"attacker-password","recovery_password":"attacker-recovery"}`))
	request.Host = "evil.com:8080"
	request.Header.Set("Origin", "http://evil.com:8080")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest || !strings.Contains(response.Body.String(), `"host_invalid"`) || r.Initialized() {
		t.Fatalf("rebinding setup: %d %s; initialized=%v", response.Code, response.Body.String(), r.Initialized())
	}
	for _, test := range []struct {
		host string
		want int
	}{
		{"localhost:8080", 200}, {"LOCALHOST:9090", 200}, {"127.0.0.1:8080", 200},
		{"[::1]:8080", 200}, {"[::1]", 200}, {"::1", 200},
		{"vault.example:8443", 200}, {"VAULT.EXAMPLE:8443", 200},
		{"192.168.1.20:8080", 200}, {"nas.example", 200},
		{"vault.example", 421}, {"vault.example:8080", 421}, {"192.168.1.20:8081", 421},
		{"evil.com:8080", 421}, {"[localhost]", 421}, {"localhost]", 421}, {"localhost.evil.com:8080", 421}, {"127.0.0.1.evil.com:8080", 421},
	} {
		t.Run(test.host, func(t *testing.T) {
			request := httptest.NewRequest("GET", "/api/status", nil)
			request.Host = test.host
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("host %q: %d %s", test.host, response.Code, response.Body.String())
			}
		})
	}
	request = httptest.NewRequest("GET", "/", nil)
	request.Host = "evil.com:8080"
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusMisdirectedRequest || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("static rebinding: %d %s", response.Code, response.Body.String())
	}
	request = httptest.NewRequest("POST", "/api/setup", strings.NewReader(`{"username":"admin","password":"admin-test-strong-password","recovery_password":"recovery-test-independent-password"}`))
	request.Host = "localhost:8080"
	request.Header.Set("Origin", "http://localhost:8080")
	response = httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusCreated || !r.Initialized() {
		t.Fatalf("allowed setup: %d %s", response.Code, response.Body.String())
	}
}

func TestStatusHidesConfigurationWhenUnauthenticated(t *testing.T) {
	h := newHarness(t, false)
	cookie := h.cookie
	h.cookie = nil
	response := h.call("GET", "/api/status", nil, false)
	var value map[string]any
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &value) != nil {
		t.Fatalf("status: %d %s", response.Code, response.Body.String())
	}
	if len(value) != 2 || value["initialized"] != true || value["authenticated"] != false {
		t.Fatalf("unauthenticated status exposed configuration: %v", value)
	}
	h.cookie = cookie
	response = h.call("GET", "/api/status", nil, false)
	value = nil
	if json.Unmarshal(response.Body.Bytes(), &value) != nil {
		t.Fatal(response.Body.String())
	}
	for _, key := range []string{"vault_id", "source_roots", "restore_roots", "mode", "unlocked", "engines", "allow_local", "oauth_configured", "cloud_pending", "csrf_token"} {
		if _, ok := value[key]; !ok {
			t.Errorf("authenticated status missing %s", key)
		}
	}
}

func TestLoginSuccessResetsThrottle(t *testing.T) {
	h := newHarness(t, false)
	for i := 0; i < 9; i++ {
		response := h.call("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong-password"}, false)
		if response.Code != 401 {
			t.Fatalf("failed login %d: %d %s", i+1, response.Code, response.Body.String())
		}
	}
	response := h.call("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPassword}, false)
	if response.Code != 200 {
		t.Fatalf("successful login: %d %s", response.Code, response.Body.String())
	}
	response = h.call("POST", "/api/auth/login", map[string]string{"username": "admin", "password": "wrong-password"}, false)
	if response.Code != 401 {
		t.Fatalf("successful login did not reset attempts: %d %s", response.Code, response.Body.String())
	}
}

func TestThrottleIsPerAction(t *testing.T) {
	r := &Runtime{attempts: map[string]loginAttempt{}}
	request := httptest.NewRequest("POST", "/api/auth/login", nil)
	for i := 0; i < 11; i++ {
		if got := r.throttled(request, "login"); got != (i == 10) {
			t.Fatalf("attempt %d throttled=%v", i+1, got)
		}
	}
	for _, action := range []string{"setup", "unlock", "cloud_secret", "transfer"} {
		if !r.ConsumeSecretAttempt(request, action) {
			t.Errorf("login throttle leaked into %s", action)
		}
	}
	r.resetAttempts(request, "unlock")
	if !r.throttled(request, "login") {
		t.Fatal("unlock reset cleared login attempts")
	}
	r.ResetSecretAttempts(request, "login")
	if r.throttled(request, "login") {
		t.Fatal("successful secret attempt did not reset its action")
	}
	r.attempts["login|"+r.clientIP(request)] = loginAttempt{Count: 10, Since: time.Now().Add(-16 * time.Minute)}
	if r.throttled(request, "login") {
		t.Fatal("expired attempts retained")
	}
}

func TestTrustedProxyUsesForwardedFor(t *testing.T) {
	trusted := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8::/32")}
	for _, test := range []struct {
		name   string
		remote string
		shared bool
	}{
		{"trusted", "10.0.0.2:54321", false},
		{"untrusted", "192.0.2.1:54321", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := &Runtime{Config: Config{TrustedProxies: trusted}, attempts: map[string]loginAttempt{}}
			request := httptest.NewRequest("POST", "/api/auth/login", nil)
			request.RemoteAddr = test.remote
			request.Header.Set("X-Forwarded-For", "198.51.100.1")
			for i := 0; i < 11; i++ {
				r.throttled(request, "login")
			}
			request.Header.Set("X-Forwarded-For", "198.51.100.2")
			if r.throttled(request, "login") != test.shared {
				t.Fatal("forwarded clients have incorrect throttle isolation")
			}
		})
	}
	r := &Runtime{Config: Config{TrustedProxies: trusted}}
	for _, test := range []struct {
		remote, forwarded, want string
	}{
		{"10.0.0.2:54321", "203.0.113.99, 198.51.100.1, 10.0.0.1", "198.51.100.1"},
		{"10.0.0.2:54321", "198.51.100.1, invalid", "10.0.0.2"},
		{"10.0.0.2:54321", "", "10.0.0.2"},
		{"[2001:db8::2]:54321", "2001:db9::1, 2001:db8::1", "2001:db9::1"},
		{"[::ffff:10.0.0.2]:54321", "::ffff:198.51.100.1", "198.51.100.1"},
		{"192.0.2.1:54321", "198.51.100.1", "192.0.2.1"},
	} {
		request := httptest.NewRequest("GET", "/api/status", nil)
		request.RemoteAddr = test.remote
		request.Header.Set("X-Forwarded-For", test.forwarded)
		if got := r.clientIP(request); got != test.want {
			t.Errorf("remote=%q forwarded=%q: got %q, want %q", test.remote, test.forwarded, got, test.want)
		}
	}
	request := httptest.NewRequest("GET", "/api/status", nil)
	request.RemoteAddr = "10.0.0.2:54321"
	request.Header.Add("X-Forwarded-For", "198.51.100.1")
	request.Header.Add("X-Forwarded-For", "10.0.0.1")
	if got := r.clientIP(request); got != "198.51.100.1" {
		t.Fatalf("multiple forwarded headers: %s", got)
	}
}

func TestLoginRejectsOversizedBody(t *testing.T) {
	h := newHarness(t, false)
	response := h.call("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPassword, "padding": strings.Repeat("x", 64<<10)}, false)
	if response.Code != 400 || !strings.Contains(response.Body.String(), `"code":"invalid_json"`) {
		t.Fatalf("oversized login: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+adminPassword+`"}`+strings.Repeat(" ", 64<<10)))
	request.Host = "localhost"
	response = httptest.NewRecorder()
	h.h.ServeHTTP(response, request)
	if response.Code != 400 {
		t.Fatalf("oversized trailing whitespace: %d %s", response.Code, response.Body.String())
	}
}

func TestSecretRequestLimitsAndEmptyLogout(t *testing.T) {
	h := newHarness(t, false)
	for _, path := range []string{"/api/security/unlock", "/api/security/recovery-kit", "/api/accounts/export", "/api/auth/logout"} {
		response := h.call("POST", path, map[string]string{"password": recoveryPassword, "padding": strings.Repeat("x", 64<<10)}, true)
		code := "invalid_json"
		if path == "/api/auth/logout" {
			code = "invalid_body"
		}
		if response.Code != 400 || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) {
			t.Errorf("oversized %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	response := h.call("GET", "/api/status", nil, false)
	if !strings.Contains(response.Body.String(), `"authenticated":true`) {
		t.Fatal("rejected oversized logout deleted session")
	}
	response = h.call("POST", "/api/auth/logout", nil, true)
	if response.Code != 204 {
		t.Fatalf("empty logout lost compatibility: %d %s", response.Code, response.Body.String())
	}
}

func TestSetupRequestLimit(t *testing.T) {
	dir := t.TempDir()
	r, err := New(Config{StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	mux := http.NewServeMux()
	r.Register(mux)
	h := &testHarness{r: r, h: r.Handler(mux)}
	input := map[string]string{"username": "admin", "password": adminPassword, "recovery_password": recoveryPassword, "padding": strings.Repeat("x", 1<<20)}
	response := h.call("POST", "/api/setup", input, false)
	if response.Code != 400 || r.Initialized() {
		t.Fatalf("oversized setup: %d %s", response.Code, response.Body.String())
	}
	input["padding"] = strings.Repeat("x", 16<<10)
	response = h.call("POST", "/api/setup", input, false)
	if response.Code != 201 {
		t.Fatalf("setup larger than auth limit: %d %s", response.Code, response.Body.String())
	}
	request := httptest.NewRequest("POST", "/", strings.NewReader(`{"padding":"`+strings.Repeat("x", 64<<10)+`"}`))
	var value map[string]any
	if !decode(httptest.NewRecorder(), request, &value) {
		t.Fatal("default decoder inherited small authentication limit")
	}
}

func TestDeriveKeyWaitsForGlobalSlot(t *testing.T) {
	if cap(argonSlots) != 2 {
		t.Fatalf("Argon2 concurrency limit: %d", cap(argonSlots))
	}
	occupied := 0
	for occupied < cap(argonSlots) {
		argonSlots <- struct{}{}
		occupied++
	}
	defer func() {
		for occupied > 0 {
			<-argonSlots
			occupied--
		}
	}()
	password, salt := []byte("test-password"), []byte("0123456789abcdef")
	started := make(chan struct{})
	done := make(chan []byte, 1)
	go func() {
		close(started)
		done <- deriveKey(password, salt, 1, 8, 1)
	}()
	<-started
	select {
	case <-done:
		t.Fatal("Argon2 derivation bypassed occupied slots")
	case <-time.After(25 * time.Millisecond):
	}
	<-argonSlots
	occupied--
	select {
	case key := <-done:
		if !bytes.Equal(key, argon2.IDKey(password, salt, 1, 8, 1, 32)) {
			t.Fatal("derivation changed the Argon2 result")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Argon2 derivation did not resume after releasing a slot")
	}
}

func TestSensitiveExportsRequireAdminPassword(t *testing.T) {
	h := newHarness(t, false)
	account := transferFixture(t, h, "rclone", "reauth-drive")
	for _, path := range []string{"/api/security/recovery-kit", "/api/accounts/export"} {
		for _, password := range []string{"", "incorrect-login-password"} {
			response := h.call("POST", path, map[string]any{"password": transferPassword, "admin_password": password, "account_ids": []string{account.ID}}, true)
			if response.Code != 401 || !strings.Contains(response.Body.String(), `"code":"admin_password_invalid"`) {
				t.Fatalf("unauthorized %s: %d %s", path, response.Code, response.Body.String())
			}
		}
		var count int
		if err := h.r.Store.DB.QueryRow("SELECT count(*) FROM audit_events WHERE json_extract(data,'$.action') IN ('recovery-kit.export','accounts.export')").Scan(&count); err != nil || count != 0 {
			t.Fatalf("failed exports changed audit history: count=%d err=%v", count, err)
		}
	}
	for _, path := range []string{"/api/security/recovery-kit", "/api/accounts/export"} {
		response := h.call("POST", path, map[string]any{"password": transferPassword, "admin_password": adminPassword, "account_ids": []string{account.ID}}, true)
		if response.Code != 200 {
			t.Fatalf("authorized %s: %d %s", path, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), adminPassword) {
			t.Fatal("export exposed admin password")
		}
	}
}

func TestExportReauthenticationThrottleAndReset(t *testing.T) {
	h := newHarness(t, false)
	for i := 0; i < 9; i++ {
		response := h.call("POST", "/api/accounts/export", map[string]string{"password": transferPassword}, true)
		if response.Code != 401 {
			t.Fatalf("failed reauthentication %d: %d %s", i+1, response.Code, response.Body.String())
		}
	}
	response := h.call("POST", "/api/security/recovery-kit", map[string]string{"password": recoveryPassword, "admin_password": adminPassword}, true)
	if response.Code != 200 {
		t.Fatalf("successful reauthentication: %d %s", response.Code, response.Body.String())
	}
	for i := 0; i < 10; i++ {
		response = h.call("POST", "/api/accounts/export", map[string]string{"password": transferPassword}, true)
		if response.Code != 401 {
			t.Fatalf("reauthentication was not reset: %d %s", response.Code, response.Body.String())
		}
	}
	response = h.call("POST", "/api/security/recovery-kit", map[string]string{"password": recoveryPassword, "admin_password": adminPassword}, true)
	if response.Code != 429 || response.Header().Get("Retry-After") == "" {
		t.Fatalf("exports do not share reauthentication throttle: %d %s", response.Code, response.Body.String())
	}
	response = h.call("POST", "/api/auth/login", map[string]string{"username": "admin", "password": adminPassword}, false)
	if response.Code != 200 {
		t.Fatalf("reauthentication throttle leaked to login: %d %s", response.Code, response.Body.String())
	}
}
