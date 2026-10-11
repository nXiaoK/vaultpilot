package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"vaultpilot/internal/platform"
)

func clearFixture(t *testing.T) (*Service, *Service, []Account) {
	t.Helper()
	s, reader := catalogRefreshNode(t), catalogRefreshNode(t)
	accounts := []Account{}
	for _, id := range []string{"one", "two", "three"} {
		a := Account{ID: id, Name: id, Backend: "local", DriveID: "drive-" + id, Enabled: true, Verified: true, LocalPath: t.TempDir()}
		accounts = append(accounts, a)
		for _, node := range []*Service{s, reader} {
			if _, err := node.rt.Store.Save(t.Context(), "accounts", id, 0, a); err != nil {
				t.Fatal(err)
			}
		}
		repo := Repository{ID: "repo-" + id, PlanID: "plan-" + id, AccountID: id, Status: "ready", Path: "VaultPilot/" + s.rt.VaultID() + "/repositories/repo-" + id}
		repo.Locations = []Location{{AccountID: id, DriveID: a.DriveID, Path: repo.Path, Verified: true}}
		if _, err := s.rt.Store.Save(t.Context(), "repositories", repo.ID, 0, repo); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{repo.Path + "/data/test.bin", "personal.txt", "VaultPilot/22222222222222222222222222222222/repositories/keep.bin", "VaultPilot/" + s.rt.VaultID() + "/connect.vpkey"} {
			target := filepath.Join(a.LocalPath, filepath.FromSlash(path))
			if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(target, []byte("keep or test data"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		pool := Pool{ID: "pool-" + id, AccountIDs: []string{id}, CatalogReplicas: 1}
		plan := Plan{ID: "plan-" + id, Name: id, PoolID: pool.ID, Enabled: true, Timezone: "Asia/Shanghai"}
		if _, err := s.rt.Store.Save(t.Context(), "storage_pools", pool.ID, 0, pool); err != nil {
			t.Fatal(err)
		}
		if _, err := s.rt.Store.Save(t.Context(), "backup_plans", plan.ID, 0, plan); err != nil {
			t.Fatal(err)
		}
	}
	for _, point := range []Point{
		{ID: "cross", RunID: "run-cross", Status: "completed", FileCount: 1, LogicalBytes: 10, Snapshots: []Snapshot{{RepositoryID: "repo-one", SnapshotID: "snap-one"}, {RepositoryID: "repo-two", SnapshotID: "snap-two"}}},
		{ID: "untouched", RunID: "run-untouched", Status: "completed", FileCount: 1, LogicalBytes: 10, Snapshots: []Snapshot{{RepositoryID: "repo-three", SnapshotID: "snap-three"}}},
	} {
		if _, err := s.rt.Store.Save(t.Context(), "recovery_points", point.ID, 0, point); err != nil {
			t.Fatal(err)
		}
		if _, err := s.rt.Store.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,repo_id) VALUES(?,?,?,?,?,?)", point.RunID, "source-one", point.ID+".bin", "file", 10, point.Snapshots[0].RepositoryID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.rt.Store.DB.Exec("INSERT INTO assignments(plan_id,source_id,path,repo_id) VALUES(?,?,?,?)", "plan-one", "source-one", "cross.bin", "repo-one"); err != nil {
		t.Fatal(err)
	}
	pool := Pool{CatalogReplicas: 3, AccountIDs: []string{"one", "two", "three"}}
	if err := s.commit(t.Context(), &Task{ID: uid()}, pool, nil, nil); err != nil {
		t.Fatal(err)
	}
	catalogRefreshDiscover(t, reader)
	return s, reader, accounts
}
func executeClear(t *testing.T, s *Service, ids []string) Task {
	t.Helper()
	p, err := s.buildClearPreview(t.Context(), ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Blocked {
		t.Fatal(p.Reasons)
	}
	req := AccountClearRequest{AccountIDs: ids, PreviewToken: p.PreviewToken, Confirmation: clearConfirmation}
	task, err := s.enqueue("account-clear", "", uid(), req)
	if err != nil {
		t.Fatal(err)
	}
	s.execute(task.ID)
	if err = s.rt.Store.Get(t.Context(), "backup_runs", task.ID, &task); err != nil {
		t.Fatal(err)
	}
	return task
}
func assertClearScope(t *testing.T, s *Service, accounts []Account, cleared map[string]bool) {
	t.Helper()
	for _, a := range accounts {
		base := filepath.Join(a.LocalPath, "VaultPilot", s.rt.VaultID())
		_, err := os.Stat(filepath.Join(base, "repositories", "repo-"+a.ID, "data", "test.bin"))
		if cleared[a.ID] && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("backup not removed on %s: %v", a.ID, err)
		}
		if !cleared[a.ID] && err != nil {
			t.Errorf("unselected account changed: %v", err)
		}
		for _, path := range []string{"personal.txt", "VaultPilot/22222222222222222222222222222222/repositories/keep.bin", "VaultPilot/" + s.rt.VaultID() + "/connect.vpkey"} {
			if _, err := os.Stat(filepath.Join(a.LocalPath, filepath.FromSlash(path))); err != nil {
				t.Errorf("removed file outside backup scope: %s %v", path, err)
			}
		}
	}
}
func TestAccountClearSelectedScopeAndCloudTombstones(t *testing.T) {
	s, reader, accounts := clearFixture(t)
	p, err := s.buildClearPreview(t.Context(), []string{"one", "two"}, "")
	if err != nil || p.Blocked {
		t.Fatalf("preview: %+v %v", p, err)
	}
	if len(p.PointIDs) != 1 || p.PointIDs[0] != "cross" || len(p.PausePlanIDs) != 2 || p.CatalogAccountCount != 3 {
		t.Fatalf("wrong impact: %+v", p)
	}
	task := executeClear(t, s, []string{"one", "two"})
	if task.Status != "completed" {
		t.Fatal(task.Error)
	}
	assertClearScope(t, s, accounts, map[string]bool{"one": true, "two": true})
	var assigned int
	s.rt.Store.DB.QueryRow("SELECT count(*) FROM assignments").Scan(&assigned)
	if assigned != 0 {
		t.Fatal("stale assignments retained")
	}
	for _, id := range []string{"one", "two", "three"} {
		var plan Plan
		s.rt.Store.Get(t.Context(), "backup_plans", "plan-"+id, &plan)
		if plan.Enabled != (id == "three") {
			t.Errorf("plan pause scope: %s", id)
		}
	}
	catalogRefreshDiscover(t, reader)
	for _, node := range []*Service{s, reader} {
		var point Point
		node.rt.Store.Get(t.Context(), "recovery_points", "cross", &point)
		if point.DeletedAt == "" {
			t.Fatal("cleared backup resurrected")
		}
		point = Point{}
		node.rt.Store.Get(t.Context(), "recovery_points", "untouched", &point)
		if point.DeletedAt != "" {
			t.Fatal("unselected backup retired")
		}
	}
	for _, id := range []string{"one", "two"} {
		var repo Repository
		reader.rt.Store.Get(t.Context(), "repositories", "repo-"+id, &repo)
		if repo.Status != "retired" {
			t.Fatal("deleted location was remapped")
		}
	}
	if pending, _ := s.rt.Store.Meta(t.Context(), "account_clear_pending"); pending != "" {
		t.Fatal("completed clear still blocks tasks")
	}
}
func TestAccountClearAllAndRepeat(t *testing.T) {
	s, reader, accounts := clearFixture(t)
	ids := []string{"one", "two", "three"}
	for i := 0; i < 2; i++ {
		task := executeClear(t, s, ids)
		if task.Status != "completed" {
			t.Fatal(task.Error)
		}
	}
	assertClearScope(t, s, accounts, map[string]bool{"one": true, "two": true, "three": true})
	catalogRefreshDiscover(t, reader)
	var count int
	reader.rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points WHERE coalesce(json_extract(data,'$.deleted_at'),'')=''").Scan(&count)
	if count != 0 {
		t.Fatal("clear-all left recoverable points")
	}
}
func TestAccountClearKeepsVerifiedAlternateLocation(t *testing.T) {
	s, reader, accounts := clearFixture(t)
	var repo Repository
	s.rt.Store.Get(t.Context(), "repositories", "repo-one", &repo)
	repo.Locations = append(repo.Locations, Location{AccountID: "three", DriveID: "drive-three", Path: repo.Path, Verified: true})
	s.rt.Store.Save(t.Context(), "repositories", repo.ID, repo.Version, repo)
	p, err := s.buildClearPreview(t.Context(), []string{"one"}, "")
	if err != nil || len(p.PointIDs) != 0 {
		t.Fatalf("alternate copy was discarded: %+v %v", p, err)
	}
	task := executeClear(t, s, []string{"one"})
	if task.Status != "completed" {
		t.Fatal(task.Error)
	}
	s.rt.Store.Get(t.Context(), "repositories", repo.ID, &repo)
	if repo.AccountID != "three" || repo.Locations[0].Verified {
		t.Fatal("repository did not move to remaining verified location")
	}
	assertClearScope(t, s, accounts, map[string]bool{"one": true})
	catalogRefreshDiscover(t, reader)
	var point Point
	reader.rt.Store.Get(t.Context(), "recovery_points", "cross", &point)
	if point.DeletedAt != "" {
		t.Fatal("recoverable alternate discarded")
	}
}
func TestAccountClearProtectedStaleAndConfirmationCannotDelete(t *testing.T) {
	s, _, accounts := clearFixture(t)
	var point Point
	s.rt.Store.Get(t.Context(), "recovery_points", "cross", &point)
	point.Protected = true
	s.rt.Store.Save(t.Context(), "recovery_points", point.ID, point.Version, point)
	p, err := s.buildClearPreview(t.Context(), []string{"one"}, "")
	if err != nil || !p.Blocked || len(p.ProtectedPointIDs) != 1 {
		t.Fatal("protected point not blocked")
	}
	task := Task{ID: uid(), Kind: "account-clear"}
	task.Request, _ = json.Marshal(AccountClearRequest{AccountIDs: []string{"one"}, Confirmation: clearConfirmation, PreviewToken: p.PreviewToken})
	if err = s.clearAccounts(t.Context(), &task); err == nil {
		t.Fatal("protected clear allowed")
	}
	point.Protected = false
	s.rt.Store.Save(t.Context(), "recovery_points", point.ID, -1, point)
	p, _ = s.buildClearPreview(t.Context(), []string{"one"}, "")
	var a platform.Account
	s.rt.Store.Get(t.Context(), "accounts", "one", &a)
	a.Name = "edited"
	s.rt.Store.Save(t.Context(), "accounts", a.ID, a.Version, a)
	task.Request, _ = json.Marshal(AccountClearRequest{AccountIDs: []string{"one"}, Confirmation: clearConfirmation, PreviewToken: p.PreviewToken})
	if err = s.clearAccounts(t.Context(), &task); err == nil || !strings.Contains(err.Error(), "预览") {
		t.Fatal("stale preview allowed")
	}
	task.Request, _ = json.Marshal(AccountClearRequest{AccountIDs: []string{"one"}, PreviewToken: p.PreviewToken})
	if err = s.clearAccounts(t.Context(), &task); err == nil {
		t.Fatal("missing confirmation accepted")
	}
	assertClearScope(t, s, accounts, map[string]bool{})
	if _, err = s.buildClearPreview(t.Context(), []string{"one", "one"}, ""); err == nil {
		t.Fatal("duplicate selection accepted")
	}
	if _, err = s.buildClearPreview(t.Context(), []string{"unknown"}, ""); err == nil {
		t.Fatal("unknown selection accepted")
	}
}
func TestAccountClearPartialRetryAndDurableTaskGate(t *testing.T) {
	s, reader, accounts := clearFixture(t)
	obstacle := filepath.Join(accounts[1].LocalPath, "VaultPilot", s.rt.VaultID(), "catalog", strings.Repeat("a", 32)+".ok", "child")
	os.MkdirAll(filepath.Dir(obstacle), 0700)
	os.WriteFile(obstacle, []byte("obstacle"), 0600)
	task := executeClear(t, s, []string{"one", "two"})
	if task.Status != "partial_failed" || task.Code != "ACCOUNT_CLEAR_PARTIAL" {
		t.Fatalf("partial: %+v", task)
	}
	if _, err := s.enqueue("backup", "plan-three", uid(), nil); err == nil {
		t.Fatal("backup allowed during unfinished destructive maintenance")
	}
	if _, err := s.buildClearPreview(t.Context(), []string{"two"}, ""); err == nil {
		t.Fatal("changed retry selection allowed")
	}
	// A fresh Service on the same persisted node must retain the gate.
	restarted, err := New(s.rt)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err = restarted.enqueue("discover", "", uid(), nil); err == nil {
		t.Fatal("restart lost pending clear state")
	}
	if err = os.RemoveAll(filepath.Dir(obstacle)); err != nil {
		t.Fatal(err)
	}
	p, err := restarted.buildClearPreview(t.Context(), []string{"two", "one"}, "")
	if err != nil || !p.Resume {
		t.Fatalf("retry: %+v %v", p, err)
	}
	task = executeClear(t, restarted, []string{"two", "one"})
	if task.Status != "completed" {
		t.Fatal(task.Error)
	}
	catalogRefreshDiscover(t, reader)
	assertClearScope(t, restarted, accounts, map[string]bool{"one": true, "two": true})
}
func TestAccountClearNoDeletionBeforeAllCatalogsCommit(t *testing.T) {
	s, _, accounts := clearFixture(t)
	bad := accounts[2]
	bad.Backend = "onedrive"
	s.rt.Store.Save(t.Context(), "accounts", bad.ID, -1, bad)
	task := executeClear(t, s, []string{"one"})
	if task.Status != "failed" {
		t.Fatal("catalog failure was hidden")
	}
	assertClearScope(t, s, accounts, map[string]bool{})
	if pending, _ := s.rt.Store.Meta(t.Context(), "account_clear_pending"); pending == "" {
		t.Fatal("retirement needs a persistent reconciliation gate")
	}
	s.rt.Store.Save(t.Context(), "accounts", accounts[2].ID, -1, accounts[2])
	task = executeClear(t, s, []string{"one"})
	if task.Status != "completed" {
		t.Fatal(task.Error)
	}
}
func TestAccountClearQueuedTaskExclusionAndIdempotency(t *testing.T) {
	s, _, _ := clearFixture(t)
	p, _ := s.buildClearPreview(t.Context(), []string{"one"}, "")
	req := AccountClearRequest{AccountIDs: []string{"one"}, Confirmation: clearConfirmation, PreviewToken: p.PreviewToken}
	first, err := s.enqueue("account-clear", "", "same", req)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.enqueue("account-clear", "", "same", req)
	if err != nil || second.ID != first.ID {
		t.Fatal("duplicate destructive task enqueued")
	}
	if _, err = s.enqueue("restore", "", uid(), nil); err == nil {
		t.Fatal("restore raced clear")
	}
	s.execute(first.ID)
	if _, err = s.enqueue("check", "", uid(), nil); err != nil {
		t.Fatal("completed clear did not release gate")
	}
	if _, err = s.enqueue("account-clear", "", uid(), req); err == nil {
		t.Fatal("clear raced queued check")
	}
}
func TestAccountClearRejectsSymlinkAndRecoveryNode(t *testing.T) {
	s, _, accounts := clearFixture(t)
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("safe"), 0600)
	path := filepath.Join(accounts[0].LocalPath, "VaultPilot", s.rt.VaultID(), "repositories")
	os.RemoveAll(path)
	os.Symlink(outside, path)
	task := executeClear(t, s, []string{"one"})
	if task.Status != "failed" {
		t.Fatal("symlink deletion allowed")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("escaped selected drive")
	}
	// Reopen in recovery mode; mutation endpoints must refuse before enqueue.
	s.rt.Store.SetMeta(t.Context(), "mode", "recovery")
	config := s.rt.Config
	s.rt.Store.Close()
	rt, err := platform.New(config)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	rt.Keys.SetMaster([]byte(strings.Repeat("*", 32)))
	reader, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err = reader.buildClearPreview(t.Context(), []string{"one"}, ""); err == nil {
		t.Fatal("recovery node clear allowed")
	}
	mux := http.NewServeMux()
	reader.Register(mux)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest("POST", "/api/accounts/clear", strings.NewReader(`{"account_ids":["one"],"confirmation":"清空所选备份","preview_token":"`+strings.Repeat("a", 64)+`"}`)))
	if res.Code == 202 {
		t.Fatal("recovery submit allowed")
	}
	// Authentication/CSRF middleware also applies to the new routes.
	res = httptest.NewRecorder()
	rt.Handler(mux).ServeHTTP(res, httptest.NewRequest("POST", "/api/accounts/clear/preview", strings.NewReader(`{"account_ids":["one"]}`)))
	if res.Code != 401 {
		t.Fatal("unauthenticated clear preview accepted")
	}
}
func TestRealResticBackupAfterAccountClear(t *testing.T) {
	s, plan, pool, a := realFixture(t)
	pool.CatalogReplicas = 1
	s.rt.Store.Save(t.Context(), "storage_pools", pool.ID, -1, pool)
	task := Task{ID: uid(), Kind: "backup", PlanID: plan.ID}
	if err := s.backup(context.Background(), &task); err != nil {
		t.Fatal(err)
	}
	task.Status = "completed"
	s.save(&task)
	cleared := executeClear(t, s, []string{a.ID})
	if cleared.Status != "completed" {
		t.Fatal(cleared.Error)
	}
	next := Task{ID: uid(), Kind: "backup", PlanID: plan.ID}
	if err := s.backup(t.Context(), &next); err != nil {
		t.Fatalf("next full backup could not start: %v", err)
	}
	var count int
	s.rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points WHERE coalesce(json_extract(data,'$.deleted_at'),'')=''").Scan(&count)
	if count != 1 {
		t.Fatalf("active points: %d", count)
	}
	var active Point
	raws, _ := s.rt.Store.All(t.Context(), "recovery_points")
	for _, raw := range raws {
		var point Point
		json.Unmarshal(raw, &point)
		if point.DeletedAt == "" {
			active = point
		}
	}
	target := s.rt.Config.RestoreRoots[0]
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	restore := Task{ID: uid(), Kind: "restore"}
	restore.Request, _ = json.Marshal(RestoreRequest{PointID: active.ID, Target: target, Layout: "direct"})
	if err := s.restore(t.Context(), &restore); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, "file.txt"))
	if err != nil || string(got) != "original business data" {
		t.Fatalf("restore after clear: %q %v", got, err)
	}

}

func TestRealRcloneClearCatalogFilterPreservesCurrentCertificate(t *testing.T) {
	binary := os.Getenv("VP_TEST_RCLONE")
	if binary == "" {
		t.Skip("VP_TEST_RCLONE required")
	}
	root := t.TempDir()
	keep, old := strings.Repeat("a", 32), strings.Repeat("b", 32)
	for _, name := range []string{keep + ".vpc", keep + ".ok", keep + ".prepare", old + ".vpc", old + ".ok", old + ".prepare", "personal.txt", "nested/" + old + ".vpc"} {
		target := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(target), 0700)
		os.WriteFile(target, []byte("fixture"), 0600)
	}
	args := append([]string{"--config", filepath.Join(t.TempDir(), "empty.conf")}, clearCatalogDeleteArguments(root, keep)...)
	if output, err := exec.CommandContext(t.Context(), binary, args...).CombinedOutput(); err != nil {
		t.Fatalf("rclone filter: %s %v", output, err)
	}
	for _, name := range []string{keep + ".vpc", keep + ".ok", keep + ".prepare", "personal.txt", "nested/" + old + ".vpc"} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Errorf("protected file removed: %s", name)
		}
	}
	for _, name := range []string{old + ".vpc", old + ".ok", old + ".prepare"} {
		if _, err := os.Stat(filepath.Join(root, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("old backup index retained: %s", name)
		}
	}
}

func TestAccountClearDisabledAccountAndRetiredRepositoryAPIs(t *testing.T) {
	s, _, accounts := clearFixture(t)
	var a platform.Account
	s.rt.Store.Get(t.Context(), "accounts", "one", &a)
	a.Enabled = false
	s.rt.Store.Save(t.Context(), "accounts", a.ID, a.Version, a)
	task := executeClear(t, s, []string{"one", "two", "three"})
	if task.Status != "completed" {
		t.Fatal(task.Error)
	}
	assertClearScope(t, s, accounts, map[string]bool{"one": true, "two": true, "three": true})
	s.rt.Store.Get(t.Context(), "accounts", "one", &a)
	if a.Enabled {
		t.Fatal("clear enabled disabled account")
	}
	mux := http.NewServeMux()
	s.Register(mux)
	res := httptest.NewRecorder()
	mux.ServeHTTP(res, httptest.NewRequest("GET", "/api/repositories?limit=1&offset=0", nil))
	var listed struct {
		Total int
		Items []Repository
	}
	json.Unmarshal(res.Body.Bytes(), &listed)
	if res.Code != 200 || listed.Total != 0 || len(listed.Items) != 0 {
		t.Fatal("retired repositories shown as usable")
	}
	check := Task{ID: uid(), Kind: "check", Request: json.RawMessage(`{}`)}
	if err := s.check(t.Context(), &check); err != nil {
		t.Fatal("check-all used retired repositories")
	}
	check.Request = json.RawMessage(`{"repository_ids":["repo-one"]}`)
	if err := s.check(t.Context(), &check); err == nil {
		t.Fatal("explicit retired check allowed")
	}
}
