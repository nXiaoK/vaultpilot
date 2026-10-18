package main

import "testing"

func TestHealthcheckURLUsesListenAddress(t *testing.T) {
	for addr, want := range map[string]string{
		"": "http://127.0.0.1:8080/api/status", ":9090": "http://127.0.0.1:9090/api/status",
		"0.0.0.0:9090": "http://127.0.0.1:9090/api/status", "[::]:9090": "http://127.0.0.1:9090/api/status",
		"127.0.0.1:8091": "http://127.0.0.1:8091/api/status", "[::1]:8092": "http://[::1]:8092/api/status",
		"192.0.2.10:8093": "http://192.0.2.10:8093/api/status",
	} {
		got, err := healthcheckURL(addr)
		if err != nil || got != want {
			t.Fatalf("healthcheckURL(%q) = %q, %v; want %q", addr, got, err, want)
		}
	}
	if _, err := healthcheckURL("invalid"); err == nil {
		t.Fatal("invalid listen address accepted")
	}
}
