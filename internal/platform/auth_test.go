package platform

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
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
