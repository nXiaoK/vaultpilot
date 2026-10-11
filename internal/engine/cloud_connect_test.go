package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"vaultpilot/internal/platform"
)

type cloudHTTP struct {
	handler http.Handler
	cookie  *http.Cookie
	csrf    string
}

func cloudTestHTTP(t *testing.T, s *Service, setup string) *cloudHTTP {
	t.Helper()
	mux := http.NewServeMux()
	s.rt.Register(mux)
	s.Register(mux)
	h := &cloudHTTP{handler: s.rt.Handler(mux)}
	path := "/api/setup"
	if s.rt.Initialized() {
		path = "/api/auth/login"
		setup = `{"username":"test-admin","password":"Disposable-login-2026!"}`
	}
	res := h.call("POST", path, setup)
	if res.Code != 200 && res.Code != 201 {
		t.Fatalf("authenticate: %d %s", res.Code, res.Body.String())
	}
	var session struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	h.csrf = session.CSRF
	for _, cookie := range res.Result().Cookies() {
		if cookie.Name == "vaultpilot_session" {
			h.cookie = cookie
		}
	}
	return h
}

func (h *cloudHTTP) call(method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if h.cookie != nil {
		r.AddCookie(h.cookie)
		r.Header.Set("X-CSRF-Token", h.csrf)
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func TestRealResticCloudPasswordConnectAndAutomaticDiscovery(t *testing.T) {
	writer, plan, pool, account := realFixture(t)
	ctx := context.Background()
	pool.CatalogReplicas = 1
	if _, err := writer.rt.Store.Save(ctx, "storage_pools", pool.ID, -1, pool); err != nil {
		t.Fatal(err)
	}
	first := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	if err := writer.backup(ctx, &first); err != nil {
		t.Fatal(err)
	}
	a := cloudTestHTTP(t, writer, "")
	if res := a.call("POST", "/api/security/cloud-connection", `{"password":"incorrect-backup-password"}`); res.Code != 400 {
		t.Fatalf("wrong password accepted: %d", res.Code)
	}
	if _, err := os.Stat(writer.remoteFile(account, "VaultPilot/"+writer.rt.VaultID()+"/connect.vpkey")); !os.IsNotExist(err) {
		t.Fatal("wrong password published an entry")
	}
	res := a.call("POST", "/api/security/cloud-connection", `{"password":"Disposable-recovery-2026!"}`)
	if res.Code != 200 {
		t.Fatalf("publish: %d %s", res.Code, res.Body.String())
	}
	raw, err := os.ReadFile(writer.remoteFile(account, "VaultPilot/"+writer.rt.VaultID()+"/connect.vpkey"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("master_key")) || bytes.Contains(raw, []byte("Disposable-recovery")) {
		t.Fatal("cloud bootstrap leaks secrets")
	}
	root := t.TempDir()
	target := filepath.Join(root, "restore")
	if err = os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	rt, err := platform.New(platform.Config{StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache"), RestoreRoots: []string{target}, AllowLocal: true, ResticBin: writer.rt.Config.ResticBin})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	reader, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(reader.Close)
	b := cloudTestHTTP(t, reader, `{"mode":"cloud","username":"reader","password":"Disposable-reader-login!"}`)
	account.ID = "reader-local-account"
	if _, err = rt.Store.Save(ctx, "accounts", account.ID, 0, account); err != nil {
		t.Fatal(err)
	}
	res = b.call("GET", "/api/security/cloud-vaults", "")
	if res.Code != 200 || !strings.Contains(res.Body.String(), writer.rt.VaultID()) || strings.Contains(res.Body.String(), "ciphertext") {
		t.Fatalf("list: %d %s", res.Code, res.Body.String())
	}
	join := func(password string) *httptest.ResponseRecorder {
		payload, _ := json.Marshal(map[string]string{"vault_id": writer.rt.VaultID(), "password": password})
		return b.call("POST", "/api/security/cloud-connect", string(payload))
	}
	if res = join("incorrect-backup-password"); res.Code != 400 || !rt.CloudPending() || rt.VaultID() != "" {
		t.Fatal("failed connection changed reader identity")
	}
	res = join("Disposable-recovery-2026!")
	if res.Code != 200 || rt.VaultID() != writer.rt.VaultID() || !rt.RecoveryMode() || rt.CloudPending() {
		t.Fatalf("join: %d %s", res.Code, res.Body.String())
	}
	var joined struct {
		Task Task `json:"task"`
	}
	_ = json.Unmarshal(res.Body.Bytes(), &joined)
	reader.execute(joined.Task.ID)
	var task Task
	_ = rt.Store.Get(ctx, "backup_runs", joined.Task.ID, &task)
	if task.Status != "completed" {
		t.Fatalf("discovery: %+v", task)
	}
	if res = join("Disposable-recovery-2026!"); res.Code != 400 {
		t.Fatal("connected node can switch identity")
	}
	if res = b.call("POST", "/api/security/cloud-connection", `{"password":"Disposable-recovery-2026!"}`); res.Code != 400 {
		t.Fatal("reader can publish writer bootstrap")
	}
	for _, kind := range []string{"backup", "retention", "migrate"} {
		queued, e := reader.enqueue(kind, plan.ID, "", nil)
		if e != nil {
			t.Fatal(e)
		}
		reader.execute(queued.ID)
		_ = rt.Store.Get(ctx, "backup_runs", queued.ID, &task)
		if task.Code != "RECOVERY_READ_ONLY" {
			t.Fatalf("reader %s allowed: %+v", kind, task)
		}
	}
	// Direct execution leaves task IDs queued; drain only nonterminal discovery tasks.
	nextDiscovery := func() string {
		for {
			select {
			case id := <-reader.queue:
				var queued Task
				if err := rt.Store.Get(ctx, "backup_runs", id, &queued); err != nil {
					t.Fatal(err)
				}
				if queued.Kind == "discover" && !terminal(queued.Status) {
					return id
				}
			default:
				t.Fatal("automatic discovery was not enqueued")
				return ""
			}
		}
	}
	// Establish a successful listing signature, then unchanged checks enqueue nothing.
	reader.pollCloud()
	refresh := nextDiscovery()
	reader.execute(refresh)
	var before, after int
	_ = rt.Store.DB.QueryRow("SELECT count(*) FROM backup_runs").Scan(&before)
	reader.pollCloud()
	_ = rt.Store.DB.QueryRow("SELECT count(*) FROM backup_runs").Scan(&after)
	if before != after {
		t.Fatal("unchanged automatic poll creates tasks")
	}
	if err = os.WriteFile(filepath.Join(writer.rt.Config.SourceRoots[0], "file.txt"), []byte("new content from node A"), 0600); err != nil {
		t.Fatal(err)
	}
	second := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	if err = writer.backup(ctx, &second); err != nil {
		t.Fatal(err)
	}
	reader.pollCloud()
	refresh = nextDiscovery()
	reader.execute(refresh)
	_ = rt.Store.Get(ctx, "backup_runs", refresh, &task)
	if task.Status != "completed" {
		t.Fatalf("automatic refresh failed: %+v", task)
	}
	var points int
	_ = rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points").Scan(&points)
	if points != 2 {
		t.Fatalf("new point not discovered: %d", points)
	}
	var point Point
	pointID := second.Result.(map[string]any)["point_id"].(string)
	if err := rt.Store.Get(ctx, "recovery_points", pointID, &point); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(RestoreRequest{PointID: point.ID, Target: filepath.Join(target, "from-a")})
	restore, err := reader.enqueue("restore", "", "", json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	reader.execute(restore.ID)
	_ = rt.Store.Get(ctx, "backup_runs", restore.ID, &task)
	if task.Status != "completed" {
		t.Fatalf("restore failed: %+v", task)
	}
	content, err := os.ReadFile(filepath.Join(target, "from-a", "file.txt"))
	if err != nil || string(content) != "new content from node A" {
		t.Fatalf("restored content: %q %v", content, err)
	}
	var storedSecrets int
	_ = rt.Store.DB.QueryRow("SELECT count(*) FROM backup_runs WHERE data LIKE '%Disposable-recovery%'").Scan(&storedSecrets)
	if storedSecrets != 0 {
		t.Fatal("backup password persisted in task requests")
	}
}

func TestCloudConnectionEndpointsRequireSessionAndCSRF(t *testing.T) {
	rt, err := platform.New(platform.Config{StateDir: t.TempDir(), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	s, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mux := http.NewServeMux()
	rt.Register(mux)
	s.Register(mux)
	h := &cloudHTTP{handler: rt.Handler(mux)}
	for _, path := range []string{"/api/security/cloud-connection", "/api/security/cloud-vaults"} {
		if res := h.call("GET", path, ""); res.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, res.Code)
		}
	}
	h = cloudTestHTTP(t, s, `{"mode":"cloud","username":"reader","password":"Disposable-reader-login!"}`)
	h.csrf = "incorrect"
	if res := h.call("POST", "/api/security/cloud-connect", `{}`); res.Code != 403 {
		t.Fatalf("missing csrf: %d", res.Code)
	}
}

func TestCloudBootstrapRejectsSpecialFilesAndOversizedMetadata(t *testing.T) {
	rt, err := platform.New(platform.Config{StateDir: t.TempDir(), CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	s, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	account := Account{Backend: "local", LocalPath: t.TempDir()}
	id := "11111111111111111111111111111111"
	path := s.remoteFile(account, "VaultPilot/"+id+"/connect.vpkey")
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readCloudBootstrap(context.Background(), account, id); err == nil {
		t.Fatal("FIFO accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readCloudBootstrap(context.Background(), account, id); err == nil {
		t.Fatal("directory accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, bytes.Repeat([]byte{'x'}, platform.CloudBootstrapMaxBytes+1), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readCloudBootstrap(context.Background(), account, id); err == nil {
		t.Fatal("oversize accepted")
	}
	if _, err = s.readCloudBootstrap(context.Background(), account, "../../outside"); err == nil {
		t.Fatal("invalid ID accepted")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.vpkey")
	if err = os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err = s.readCloudBootstrap(context.Background(), account, id); err == nil {
		t.Fatal("outside symlink accepted")
	}
}
