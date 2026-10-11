package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type notificationTransport func(*http.Request) (*http.Response, error)

func (f notificationTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRealResticBackupTelegramFinalResults(t *testing.T) {
	// No parallel tests in this package. Intercept the standard HTTP transport
	// before constructing the runtime; no real Telegram credentials or requests.
	original := http.DefaultTransport
	http.DefaultTransport = notificationTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.telegram.org" {
			t.Errorf("unexpected outbound request: %s", req.URL.Host)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	s, plan, pool, _ := realFixture(t)
	pool.CatalogReplicas = 1
	s.rt.Store.Save(t.Context(), "storage_pools", pool.ID, -1, pool)
	mux := http.NewServeMux()
	s.rt.Register(mux)
	body := `{"version":0,"enabled":true,"bot_token":"12345:fake_token_abcdefghijklmnop","chat_id":"123456789","node_name":"A","backup_success":true,"backup_failure":true,"account_error":true}`
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest("PUT", "/api/notifications/telegram", strings.NewReader(body)))
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	for _, success := range []bool{true, false} {
		if !success {
			s.rt.Keys.Lock()
		}
		task, err := s.enqueue("backup", plan.ID, uid(), nil)
		if err != nil {
			t.Fatal(err)
		}
		s.execute(task.ID)
		var final Task
		if err = s.rt.Store.Get(t.Context(), "backup_runs", task.ID, &final); err != nil {
			t.Fatal(err)
		}
		expected := "backup_success"
		if !success {
			expected = "backup_failure"
		}
		if success && final.Status != "completed" {
			raw, _ := json.Marshal(final)
			t.Fatalf("backup did not finish: %s", raw)
		}
		var kind string
		if err = s.rt.Store.DB.QueryRow("SELECT kind FROM notification_outbox WHERE id LIKE ?", "%:backup:"+task.ID).Scan(&kind); err != nil || kind != expected {
			t.Fatalf("final notification missing: %s %v", kind, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var sent int
		s.rt.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox WHERE status='sent'").Scan(&sent)
		if sent == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background sender did not complete both notifications")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
