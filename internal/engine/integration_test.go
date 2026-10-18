package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vaultpilot/internal/platform"
)

// These tests use an explicitly configured real Restic binary and never mock a successful engine response.
func realFixture(t *testing.T) (*Service, Plan, Pool, Account) {
	t.Helper()
	binary := os.Getenv("VP_TEST_RESTIC")
	if binary == "" {
		t.Skip("VP_TEST_RESTIC is required for real-engine fault tests")
	}
	binary, _ = filepath.Abs(binary)
	root := t.TempDir()
	source, drive := filepath.Join(root, "source"), filepath.Join(root, "drive")
	for _, dir := range []string{source, drive} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	rt, err := platform.New(platform.Config{StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache"), SourceRoots: []string{source}, RestoreRoots: []string{filepath.Join(root, "restore")}, AllowLocal: true, ResticBin: binary})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	mux := http.NewServeMux()
	rt.Register(mux)
	request := httptest.NewRequest("POST", "/api/setup", bytes.NewBufferString(`{"username":"test-admin","password":"Disposable-login-2026!","recovery_password":"Disposable-recovery-2026!","mode":"create"}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("setup: %d %s", response.Code, response.Body.String())
	}
	service, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(service.Close)
	identity, err := platform.MountIdentity(source)
	if err != nil {
		t.Fatal(err)
	}
	src := Source{ID: "test-source", Name: "test source", Path: source, MountIdentity: identity, IncludeHidden: true}
	account := Account{ID: "test-account", Name: "local", Backend: "local", DriveID: "stable-local-drive", Verified: true, Enabled: true, LocalPath: drive, Budget: 64 << 20, MaintenanceReserve: 2 << 20}
	pool := Pool{ID: "test-pool", AccountIDs: []string{account.ID}, CatalogReplicas: 2, ShardTarget: 8 << 20}
	plan := Plan{ID: "test-plan", Name: "fault-test", SourceIDs: []string{src.ID}, PoolID: pool.ID, Timezone: "Asia/Shanghai"}
	for table, value := range map[string]any{"sources": src, "accounts": account, "storage_pools": pool, "backup_plans": plan} {
		raw, _ := json.Marshal(value)
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		if _, err = rt.Store.Save(context.Background(), table, object["id"].(string), 0, value); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(source, "file.txt"), []byte("original business data"), 0600); err != nil {
		t.Fatal(err)
	}
	return service, plan, pool, account
}
func TestRealResticCatalogInsufficientNeverPublishesPoint(t *testing.T) {
	s, plan, _, _ := realFixture(t)
	task := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	err := s.backup(context.Background(), &task)
	var failure *failure
	if !errors.As(err, &failure) || failure.Code != "CATALOG_REPLICAS_INSUFFICIENT" {
		t.Fatalf("expected replica failure; got %v", err)
	}
	var points, shards int
	_ = s.rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points").Scan(&points)
	_ = s.rt.Store.DB.QueryRow("SELECT count(*) FROM shard_runs").Scan(&shards)
	if points != 0 || shards < 1 {
		t.Fatalf("data shard must exist but recovery point must not publish: points=%d shards=%d", points, shards)
	}
}
func TestRealResticSameTypeSourceChangeRejectsSnapshot(t *testing.T) {
	s, plan, pool, account := realFixture(t)
	ctx := context.Background()
	task := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	sources, err := s.scan(ctx, &task, plan)
	if err != nil {
		t.Fatal(err)
	}
	accounts, err := s.accounts(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	allocation, repos, err := s.allocate(ctx, &task, plan, pool, accounts)
	if err != nil {
		t.Fatal(err)
	}
	if len(allocation.Shards) != 1 {
		t.Fatalf("unexpected shards: %+v", allocation)
	}
	if err = os.WriteFile(filepath.Join(sources["test-source"].Path, "file.txt"), []byte("source changed after scan: longer contents"), 0600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	repo := repos[allocation.Shards[0].RepositoryID]
	if _, err = s.backupShard(ctx, &task, plan, repo, account, sources, stage); err == nil {
		t.Fatal("same-type change published inaccurate file manifest")
	}
	var points int
	_ = s.rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points").Scan(&points)
	if points != 0 {
		t.Fatal("inaccurate restore point was published")
	}
	var verificationRows int
	if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM engine_snapshot_entries_v2 WHERE task_id=?", task.ID).Scan(&verificationRows); err != nil || verificationRows != 0 {
		t.Fatalf("failed verification leaked rows: %d %v", verificationRows, err)
	}
}

func TestRealResticMidRunRestoreReportsPartialOutput(t *testing.T) {
	s, plan, pool, _ := realFixture(t)
	ctx := context.Background()
	pool.CatalogReplicas = 1
	if _, err := s.rt.Store.Save(ctx, "storage_pools", pool.ID, -1, pool); err != nil {
		t.Fatal(err)
	}
	var source Source
	if err := s.rt.Store.Get(ctx, "sources", plan.SourceIDs[0], &source); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(source.Path, "file.txt"), filepath.Join(source.Path, "a-first.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(source.Path, "b-unsafe-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source.Path, "z-unattempted.txt"), []byte("must stay unattempted"), 0600); err != nil {
		t.Fatal(err)
	}
	backup := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	if err := s.backup(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	result, ok := backup.Result.(map[string]any)
	if !ok {
		t.Fatalf("backup result: %#v", backup.Result)
	}
	pointID := result["point_id"].(string)
	if err := os.MkdirAll(s.rt.Config.RestoreRoots[0], 0700); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(s.rt.Config.RestoreRoots[0])
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(canonicalRoot, "partial-target")
	request := RestoreRequest{PointID: pointID, Target: target}
	preview, err := s.restorePreview(ctx, request)
	if err != nil || preview.Blocked {
		t.Fatalf("preflight: %+v %v", preview, err)
	}
	task, err := s.enqueue("restore", "", "mid-run-test", request)
	if err != nil {
		t.Fatal(err)
	}
	s.execute(task.ID)
	var final Task
	if err = s.rt.Store.Get(ctx, "backup_runs", task.ID, &final); err != nil {
		t.Fatal(err)
	}
	if final.Status != "partial_failed" || final.Code != "RESTORE_UNSAFE_LINK" {
		t.Fatalf("partial result lost original failure code: %#v", final)
	}
	output, err := os.ReadFile(filepath.Join(target, "a-first.txt"))
	if err != nil || string(output) != "original business data" {
		t.Fatalf("completed output was not preserved: %q %v", output, err)
	}
	if _, err = os.Lstat(filepath.Join(target, "b-unsafe-link")); !os.IsNotExist(err) {
		t.Fatalf("unsafe link was published: %v", err)
	}
	if _, err = os.Lstat(filepath.Join(target, "z-unattempted.txt")); !os.IsNotExist(err) {
		t.Fatalf("later entry was written despite prior failure: %v", err)
	}
	stats := final.Result.(map[string]any)
	if stats["restored"].(float64) != 2 || stats["failed"].(float64) != 1 || stats["unattempted"].(float64) != 1 || stats["conflicts"].(float64) != 0 {
		t.Fatalf("inaccurate partial counts: %#v", stats)
	}
	failures := stats["failure_entries"].([]any)
	if len(failures) != 1 || failures[0].(map[string]any)["path"] != "b-unsafe-link" {
		t.Fatalf("missing failure path: %#v", stats)
	}
	if stats["verification_status"] != "partial" {
		t.Fatalf("unattempted entries reported fully verified: %#v", stats)
	}
}

func TestRealResticRetentionPruneRetriesAfterFailure(t *testing.T) {
	s, plan, pool, account := realFixture(t)
	ctx := context.Background()
	pool.CatalogReplicas = 1
	if _, err := s.rt.Store.Save(ctx, "storage_pools", pool.ID, -1, pool); err != nil {
		t.Fatal(err)
	}
	var source Source
	if err := s.rt.Store.Get(ctx, "sources", plan.SourceIDs[0], &source); err != nil {
		t.Fatal(err)
	}
	points := []Point{}
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(source.Path, "file.txt"), []byte(fmt.Sprintf("backup content %d", i)), 0600); err != nil {
			t.Fatal(err)
		}
		task := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
		if err := s.backup(ctx, &task); err != nil {
			t.Fatal(err)
		}
		task.Status, task.Stage, task.FinishedAt = "completed", "completed", now()
		if err := s.save(&task); err != nil {
			t.Fatal(err)
		}
		var point Point
		if err := s.rt.Store.Get(ctx, "recovery_points", task.Result.(map[string]any)["point_id"].(string), &point); err != nil {
			t.Fatal(err)
		}
		points = append(points, point)
	}
	binary := s.rt.Config.ResticBin
	wrapper := filepath.Join(t.TempDir(), "restic-prune-failure")
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = prune ]; then echo injected-prune-failure >&2; exit 73; fi\ndone\nexec '" + strings.ReplaceAll(binary, "'", "'\\''") + "' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.rt.Config.ResticBin = wrapper
	request, _ := json.Marshal(RetentionRequest{PlanID: plan.ID, PointIDs: []string{points[0].ID}, Confirm: true})
	task := Task{ID: uid(), Kind: "retention", PlanID: plan.ID, Request: request}
	err := s.retain(ctx, &task)
	failure, ok := err.(*failure)
	if !ok || failure.Code != "RETENTION_ENGINE_FAILED" {
		t.Fatalf("expected injected prune failure: %v", err)
	}
	var repo Repository
	if err = s.rt.Store.Get(ctx, "repositories", points[0].Snapshots[0].RepositoryID, &repo); err != nil {
		t.Fatal(err)
	}
	if len(repo.PendingForget) == 0 || task.Result.(map[string]any)["cleanup_pending"] != true {
		t.Fatalf("retry intent lost: %+v %#v", repo, task.Result)
	}
	var deleted Point
	if err = s.rt.Store.Get(ctx, "recovery_points", points[0].ID, &deleted); err != nil || deleted.DeletedAt == "" {
		t.Fatalf("delete decision missing: %+v %v", deleted, err)
	}
	if got := cleanupEntryCount(t, s, points[0].RunID); got != 0 {
		t.Fatalf("deleted point kept its file list after prune failure: %d", got)
	}
	if got := cleanupEntryCount(t, s, points[1].RunID); int64(got) != points[1].FileCount {
		t.Fatalf("live point lost its file list: %d want=%d", got, points[1].FileCount)
	}
	// The deletion catalog must carry the retry intent even before the local registration is saved.
	if err = s.discover(ctx, &Task{ID: uid(), Kind: "discover"}); err != nil {
		t.Fatal(err)
	}
	if err = s.rt.Store.Get(ctx, "repositories", repo.ID, &repo); err != nil || len(repo.PendingForget) == 0 {
		t.Fatalf("catalog lost pending cleanup: %+v %v", repo, err)
	}
	s.rt.Config.ResticBin = binary
	out, err := s.restic(ctx, &task, repo, account, []string{"snapshots", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var snapshots []struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(out, &snapshots); err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if snapshot.ID == points[0].Snapshots[0].SnapshotID {
			t.Fatal("fault occurred before forget; expected prune-only retry")
		}
	}
	request, _ = json.Marshal(RetentionRequest{PlanID: plan.ID, PointIDs: []string{}, Confirm: true})
	catalogPath := filepath.Join(account.LocalPath, "VaultPilot", s.rt.VaultID(), "catalog")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }
	wrapper = filepath.Join(t.TempDir(), "restic-catalog-failure")
	script = "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = prune ]; then\n    " + quote(binary) + " \"$@\" || exit $?\n    mv " + quote(catalogPath) + " " + quote(catalogPath+".saved") + " || exit $?\n    : > " + quote(catalogPath) + "\n    exit 0\n  fi\ndone\nexec " + quote(binary) + " \"$@\"\n"
	if err = os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.rt.Config.ResticBin = wrapper
	task = Task{ID: uid(), Kind: "retention", PlanID: plan.ID, Request: request}
	if err = s.retain(ctx, &task); err == nil || task.Result.(map[string]any)["cleanup_pending"] != true {
		t.Fatalf("failed cleanup catalog lost retry intent: %v %#v", err, task.Result)
	}
	if err = os.Remove(catalogPath); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(catalogPath+".saved", catalogPath); err != nil {
		t.Fatal(err)
	}
	s.rt.Config.ResticBin = binary
	for i := 0; i < 2; i++ {
		task = Task{ID: uid(), Kind: "retention", PlanID: plan.ID, Request: request}
		if err = s.retain(ctx, &task); err != nil {
			t.Fatalf("retry %d: %v", i, err)
		}
		if task.Result.(map[string]any)["cleanup_pending"] != false {
			t.Fatalf("cleanup remains pending: %#v", task.Result)
		}
	}
	repoID := repo.ID
	repo = Repository{}
	if err = s.rt.Store.Get(ctx, "repositories", repoID, &repo); err != nil || len(repo.PendingForget) != 0 {
		t.Fatalf("pending cleanup not cleared: %+v %v", repo, err)
	}
	if err = s.rt.Store.SetMeta(ctx, "catalog_discovered", ""); err != nil {
		t.Fatal(err)
	}
	if err = s.discover(ctx, &Task{ID: uid(), Kind: "discover"}); err != nil {
		t.Fatal(err)
	}
	if err = s.rt.Store.Get(ctx, "repositories", repoID, &repo); err != nil || len(repo.PendingForget) != 0 {
		t.Fatalf("discovery resurrected completed cleanup: %+v %v", repo, err)
	}
	if _, err = s.restic(ctx, &task, repo, account, []string{"check", "--read-data"}, nil); err != nil {
		t.Fatal(err)
	}
	out, err = s.restic(ctx, &task, repo, account, []string{"snapshots", "--json"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(out, &snapshots); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 1 || snapshots[0].ID != points[1].Snapshots[0].SnapshotID {
		t.Fatalf("live snapshot lost or deleted snapshot returned: %s", out)
	}
	if err = s.recordRetentionDeletion(ctx, nil, map[string][]string{repo.ID: {snapshots[0].ID}}); err != nil {
		t.Fatal(err)
	}
	task = Task{ID: uid(), Kind: "retention", PlanID: plan.ID, Request: request}
	if err = s.retain(ctx, &task); err != nil {
		t.Fatalf("new live reference must remain protected: %v", err)
	}
	if _, err = s.restic(ctx, &task, repo, account, []string{"cat", "snapshot", snapshots[0].ID}, nil); err != nil {
		t.Fatal(err)
	}
	pending, err := s.pendingPruneRepositories(ctx, plan.ID)
	if err != nil || len(pending) != 0 {
		t.Fatalf("protected snapshot left a pending cleanup: %v %v", pending, err)
	}
}
