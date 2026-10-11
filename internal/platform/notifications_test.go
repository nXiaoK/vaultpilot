package platform

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const fakeTelegramToken = "123456:FAKE_test_token_abcdefghijklmnop"

func notificationHarness(t *testing.T) *testHarness {
	t.Helper()
	h := newHarness(t, false)
	// The production worker is exercised separately. Most tests use a controlled
	// sender so they never contact Telegram, even with intentionally fake tokens.
	h.r.notificationCancel()
	h.r.notificationWG.Wait()
	return h
}
func notificationSettings() map[string]any {
	return map[string]any{"version": 0, "enabled": true, "bot_token": fakeTelegramToken, "chat_id": "123456789", "node_name": "A · 测试备份机", "backup_success": true, "backup_failure": true, "account_error": true}
}
func enableNotifications(t *testing.T, h *testHarness) {
	t.Helper()
	res := h.call("PUT", "/api/notifications/telegram", notificationSettings(), true)
	if res.Code != 200 {
		t.Fatalf("enable: %d %s", res.Code, res.Body.String())
	}
}
func notificationCount(t *testing.T, r *Runtime, status string) int {
	t.Helper()
	var count int
	query := "SELECT count(*) FROM notification_outbox"
	args := []any{}
	if status != "" {
		query += " WHERE status=?"
		args = append(args, status)
	}
	if err := r.Store.DB.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}
func notificationResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}
func TestTelegramSettingsEncryptionAuthenticationAndRotation(t *testing.T) {
	h := notificationHarness(t)
	res := h.call("PUT", "/api/notifications/telegram", notificationSettings(), false)
	if res.Code != 403 {
		t.Fatal("missing CSRF accepted")
	}
	unauth := httptest.NewRecorder()
	h.h.ServeHTTP(unauth, httptest.NewRequest("GET", "/api/notifications/telegram", nil))
	if unauth.Code != 401 {
		t.Fatal("unauthenticated configuration read")
	}
	enableNotifications(t, h)
	res = h.call("GET", "/api/notifications/telegram", nil, false)
	if strings.Contains(res.Body.String(), fakeTelegramToken) || strings.Contains(res.Body.String(), "bot_token") {
		t.Fatal("token exposed")
	}
	raw, _ := h.r.meta(telegramConfigKey)
	if strings.Contains(raw, fakeTelegramToken) || strings.Contains(raw, "123456789") {
		t.Fatal("config stored unencrypted")
	}
	if h.call("POST", "/api/notifications/telegram/test", map[string]any{}, true).Code != 202 {
		t.Fatal("test enqueue failed")
	}
	v := notificationSettings()
	v["version"] = 1
	v["bot_token"] = ""
	v["node_name"] = "B"
	res = h.call("PUT", "/api/notifications/telegram", v, true)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	c, _ := h.r.telegramConfig()
	if c.BotToken != fakeTelegramToken {
		t.Fatal("blank token discarded existing token")
	}
	res = h.call("PUT", "/api/notifications/telegram", v, true)
	if res.Code != 409 {
		t.Fatal("stale version accepted")
	}
	v["version"] = 2
	v["chat_id"] = "@username"
	if h.call("PUT", "/api/notifications/telegram", v, true).Code != 400 {
		t.Fatal("username accepted as private chat ID")
	}
	v["chat_id"] = "987654321"
	if h.call("PUT", "/api/notifications/telegram", v, true).Code != 200 {
		t.Fatal("recipient rotation failed")
	}
	if notificationCount(t, h.r, "cancelled") != 1 {
		t.Fatal("old recipient pending message survived")
	}
	v["version"] = 3
	v["enabled"] = false
	v["clear_token"] = true
	if h.call("PUT", "/api/notifications/telegram", v, true).Code != 200 {
		t.Fatal("disable failed")
	}
	c, _ = h.r.telegramConfig()
	if c.BotToken != "" {
		t.Fatal("clear token ignored")
	}
	if h.call("POST", "/api/notifications/telegram/test", map[string]any{}, true).Code != 409 {
		t.Fatal("disabled test allowed")
	}
}

func TestTelegramBackupFinalResultsDedupAndCatchup(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	ctx := t.Context()
	h.r.Store.Save(ctx, "backup_plans", "p", 0, map[string]any{"name": "每日测试备份"})
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, status := range []string{"completed", "failed", "partial_failed", "cancelled", "interrupted", "running"} {
		id := status
		_, err := h.r.Store.Save(ctx, "backup_runs", id, 0, map[string]any{"kind": "backup", "status": status, "plan_id": "p", "finished_at": stamp, "started_at": time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano), "error": "secret-token-DO-NOT-SEND", "scanned_bytes": 123, "uploaded_bytes": 45})
		if err != nil {
			t.Fatal(err)
		}
		if err = h.r.NotifyBackup(id); err != nil {
			t.Fatal(err)
		}
		if err = h.r.NotifyBackup(id); err != nil {
			t.Fatal(err)
		}
	}
	if notificationCount(t, h.r, "") != 5 {
		t.Fatal("wrong final-result count")
	}
	h.r.Store.Save(ctx, "backup_runs", "preview", 0, map[string]any{"kind": "preview", "status": "completed", "finished_at": stamp})
	h.r.NotifyBackup("preview")
	h.r.Store.Save(ctx, "backup_runs", "historic", 0, map[string]any{"kind": "backup", "status": "completed", "finished_at": "2020-01-01T00:00:00Z"})
	h.r.NotifyBackup("historic")
	h.r.Store.Save(ctx, "backup_runs", "missed-at-crash", 0, map[string]any{"kind": "backup", "status": "failed", "finished_at": stamp})
	h.r.reconcileBackupNotifications(ctx)
	if notificationCount(t, h.r, "") != 6 {
		t.Fatal("durable completion catchup failed")
	}
	var sent int
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "api.telegram.org" || req.URL.Path != "/bot"+fakeTelegramToken+"/sendMessage" || req.Method != "POST" {
			t.Error("unexpected destination")
		}
		data, _ := io.ReadAll(req.Body)
		if strings.Contains(string(data), "secret-token-DO-NOT-SEND") {
			t.Error("raw task error leaked")
		}
		var body map[string]any
		json.Unmarshal(data, &body)
		if body["chat_id"] != "123456789" || body["parse_mode"] != nil {
			t.Error("unexpected recipient or formatting")
		}
		if !strings.Contains(body["text"].(string), "A · 测试备份机") {
			t.Error("missing node identity")
		}
		sent++
		return notificationResponse(200, `{"ok":true}`), nil
	})}
	h.r.deliverNotifications(ctx)
	if sent != 6 || notificationCount(t, h.r, "sent") != 6 {
		t.Fatal("not all final notifications delivered")
	}
	var retained int
	h.r.Store.DB.QueryRow("SELECT count(*) FROM notification_outbox WHERE body<>''").Scan(&retained)
	if retained != 0 {
		t.Fatal("sent bodies retained")
	}
}

func TestTelegramRetryLimitsSanitizedFailuresAndRateLimit(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	for i := 0; i < 3; i++ {
		if h.call("POST", "/api/notifications/telegram/test", map[string]any{}, true).Code != 202 {
			t.Fatal("test queue")
		}
	}
	if h.call("POST", "/api/notifications/telegram/test", map[string]any{}, true).Code != 429 {
		t.Fatal("test spam allowed")
	}
	calls := 0
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		return notificationResponse(429, `{"ok":false,"error_code":429,"description":"secret `+fakeTelegramToken+`","parameters":{"retry_after":120}}`), nil
	})}
	h.r.deliverNotifications(t.Context())
	h.r.deliverNotifications(t.Context())
	if calls != 1 {
		t.Fatal("did not honor bot-wide Retry-After")
	}
	var attempts int
	h.r.Store.DB.QueryRow("SELECT sum(attempts) FROM notification_outbox").Scan(&attempts)
	if attempts != 1 {
		t.Fatal(attempts)
	}
	h.r.setMeta("telegram_retry_after", "")
	h.r.Store.DB.Exec("UPDATE notification_outbox SET next_at=0")
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return nil, errors.New("network error containing " + fakeTelegramToken)
	})}
	h.r.deliverNotifications(t.Context())
	res := h.call("GET", "/api/notifications/telegram", nil, false)
	if strings.Contains(res.Body.String(), fakeTelegramToken) || strings.Contains(res.Body.String(), "network error containing") {
		t.Fatal("HTTP error leaked token URL")
	}
	h.r.Store.DB.Exec("UPDATE notification_outbox SET next_at=0,attempts=7")
	h.r.deliverNotifications(t.Context())
	if notificationCount(t, h.r, "failed") != 3 {
		t.Fatal("retry cap ignored")
	}
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return notificationResponse(403, `{"ok":false,"description":"private server echo"}`), nil
	})}
	if h.call("POST", "/api/notifications/telegram/retry", map[string]any{}, true).Code != 202 {
		t.Fatal("manual retry failed")
	}
	h.r.deliverNotifications(t.Context())
	if notificationCount(t, h.r, "failed") != 3 {
		t.Fatal("permanent 403 retried automatically")
	}
	res = h.call("GET", "/api/notifications/telegram", nil, false)
	if !strings.Contains(res.Body.String(), "/start") || strings.Contains(res.Body.String(), "private server echo") {
		t.Fatal("unsafe or missing actionable failure")
	}
}

func TestTelegramAuthorizationIncidentsAndNormalRefresh(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	a := Account{ID: "one", Name: "OneDrive 测试账号", Backend: "onedrive", DriveID: "drive-one", Verified: true, Enabled: true}
	h.r.Store.Save(t.Context(), "accounts", a.ID, 0, a)
	h.r.saveCredential(a.ID, h.r.customCredential(OAuthToken{AccessToken: "expired-access-secret", RefreshToken: "refresh-secret", Expiry: time.Now().Add(-time.Hour)}))
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "login.microsoftonline.com" {
			return notificationResponse(200, `{"access_token":"fresh-secret","expires_in":3600}`), nil
		}
		return notificationResponse(200, `{"id":"drive-one"}`), nil
	})}
	h.r.checkAccountAuthorizations(t.Context())
	if notificationCount(t, h.r, "") != 0 {
		t.Fatal("normal token expiry/refresh raised alert")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.r.ObserveAccountAccess(a.ID, cloudError{Status: 401}) }()
	}
	wg.Wait()
	if notificationCount(t, h.r, "") != 1 {
		t.Fatal("repeated failure spam")
	}
	h.r.AccountAuthorizationHealthy(a.ID, false)
	h.r.ObserveAccountAccess(a.ID, cloudError{Status: 403})
	h.r.AccountAuthorizationHealthy(a.ID, false)
	h.r.ObserveAccountAccess(a.ID, cloudError{Status: 403})
	if notificationCount(t, h.r, "") != 2 {
		t.Fatal("read success incorrectly cleared write-permission incident")
	}
	h.r.AccountAuthorizationHealthy(a.ID, true)
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return notificationResponse(403, `{}`), nil })}
	_, _ = h.r.graph(t.Context(), a.ID, "GET", "/me?$select=mail", nil, "")
	if notificationCount(t, h.r, "") != 2 {
		t.Fatal("optional profile permission raised Drive alarm")
	}
	_, _ = h.r.graph(t.Context(), a.ID, "GET", "/drives/drive-one", nil, "")
	if notificationCount(t, h.r, "") != 3 {
		t.Fatal("Drive access failure missed")
	}
	h.r.AccountAuthorizationHealthy(a.ID, true)
	h.r.ObserveRcloneFailure(a.ID, []byte(`oauth2: "invalid_grant" token=refresh-secret`), errors.New("exit 1"))
	if notificationCount(t, h.r, "") != 4 {
		t.Fatal("rclone revoked grant missed")
	}
	h.r.AccountAuthorizationHealthy(a.ID, true)
	h.r.ObserveRcloneFailure(a.ID, []byte("network timeout"), errors.New("exit 1"))
	if notificationCount(t, h.r, "") != 4 {
		t.Fatal("ordinary transport error misclassified")
	}
	h.r.saveCredential(a.ID, h.r.customCredential(OAuthToken{AccessToken: "expired", Expiry: time.Now().Add(-time.Hour)}))
	_, _ = h.r.accessToken(t.Context(), a.ID)
	if notificationCount(t, h.r, "") != 5 {
		t.Fatal("expired grant without refresh token missed")
	}
}

func TestTelegramQueueAndIncidentsSurviveRuntimeRestart(t *testing.T) {
	dir := t.TempDir()
	r, err := New(Config{StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	r.notificationCancel()
	r.notificationWG.Wait()
	c := telegramConfig{Version: 1, Enabled: true, BotToken: fakeTelegramToken, ChatID: "123456789", AccountError: true, Generation: "restart-generation", Since: time.Now().UTC().Format(time.RFC3339Nano)}
	raw, _ := json.Marshal(c)
	sealed, _ := r.sealNotification("config", raw)
	r.setMeta(telegramConfigKey, sealed)
	r.Store.Save(t.Context(), "accounts", "one", 0, Account{ID: "one", Backend: "onedrive", Name: "test"})
	r.AccountAuthorizationFailed("one", "authorization_required")
	r.Store.DB.Exec("UPDATE notification_outbox SET attempts=2,next_at=?", time.Now().Add(time.Minute).Unix())
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = New(Config{StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	r.notificationCancel()
	r.notificationWG.Wait()
	r.AccountAuthorizationFailed("one", "authorization_required")
	if notificationCount(t, r, "") != 1 {
		t.Fatal("restart duplicated incident")
	}
	c, err = r.telegramConfig()
	if err != nil || c.BotToken != fakeTelegramToken {
		t.Fatal("encrypted config not recovered")
	}
	var attempts int
	r.Store.DB.QueryRow("SELECT attempts FROM notification_outbox").Scan(&attempts)
	if attempts != 2 {
		t.Fatal("retry state lost")
	}
	r.Store.DB.Exec("UPDATE notification_outbox SET next_at=0")
	r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) { return notificationResponse(200, `{"ok":true}`), nil })}
	r.deliverNotifications(t.Context())
	if notificationCount(t, r, "sent") != 1 {
		t.Fatal("pending message lost after restart")
	}
}

func TestTelegramWorkerDeliveryAndDisabledEvents(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	v := notificationSettings()
	v["version"] = 1
	v["bot_token"] = ""
	v["backup_success"] = false
	v["account_error"] = false
	if h.call("PUT", "/api/notifications/telegram", v, true).Code != 200 {
		t.Fatal("toggle events failed")
	}
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	h.r.Store.Save(t.Context(), "backup_runs", "success", 0, map[string]any{"kind": "backup", "status": "completed", "finished_at": stamp})
	h.r.NotifyBackup("success")
	h.r.Store.Save(t.Context(), "accounts", "one", 0, Account{Backend: "onedrive"})
	h.r.AccountAuthorizationFailed("one", "authorization_required")
	if notificationCount(t, h.r, "") != 0 {
		t.Fatal("disabled event queued")
	}
	delivered := make(chan struct{}, 1)
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case delivered <- struct{}{}:
		default:
		}
		return notificationResponse(200, `{"ok":true}`), nil
	})}
	h.r.startNotifications()
	if h.call("POST", "/api/notifications/telegram/test", map[string]any{}, true).Code != 202 {
		t.Fatal("test queue failed")
	}
	select {
	case <-delivered:
	case <-time.After(3 * time.Second):
		t.Fatal("background worker did not send")
	}
	deadline := time.Now().Add(2 * time.Second)
	for notificationCount(t, h.r, "sent") != 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	h.r.notificationCancel()
	h.r.notificationWG.Wait()
	if notificationCount(t, h.r, "sent") != 1 {
		t.Fatal("worker delivery not persisted")
	}
}

func TestTelegramRedirectAndMalformedResponseNeverLeakToken(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	c, _ := h.r.telegramConfig()
	for _, code := range []int{302, 200, 401, 400, 500} {
		calls := 0
		h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			res := notificationResponse(code, "not-json "+fakeTelegramToken)
			res.Header.Set("Location", "https://untrusted.example/token")
			return res, nil
		})}
		err := h.r.sendTelegram(context.Background(), c, "test")
		if err == nil || calls != 1 || strings.Contains(err.Error(), fakeTelegramToken) {
			t.Fatalf("unsafe response handling HTTP %s: %v", strconv.Itoa(code), err)
		}
	}
}
