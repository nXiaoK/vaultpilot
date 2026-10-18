package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

func TestUnicodeSubtreeAndGlobstar(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, path := range []string{"目录", "目录/子目录/a.txt", "目录旁/a.txt", "back\\slash\n.txt"} {
		_, err = db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind) VALUES(?,?,?,?)", "r", "s", path, "file")
		if err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{rt: &platform.Runtime{Store: db}}
	where, args, err := selectionWhere(RestoreRequest{Paths: []PathSelection{{SourceID: "s", Path: "目录"}}}, "r")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := s.entries(context.Background(), where, args, 10, 0)
	if err != nil || len(entries) != 2 {
		t.Fatalf("Unicode subtree selection: entries=%v err=%v", entries, err)
	}
	for _, path := range []string{".cache/a.txt", "a/b/.cache/c.txt", "a/b/file.tmp"} {
		if !excluded(path, []string{"**/.cache/**", "**/*.tmp"}) {
			t.Fatalf("globstar did not exclude %q", path)
		}
	}
	if !safeLogical("back\\slash\n.txt") {
		t.Fatal("valid Unix filename rejected")
	}
	if safeLogical("目录/../escape") {
		t.Fatal("path traversal accepted")
	}
}
func TestRestoreFailurePreservesExistingFile(t *testing.T) {
	outDir, privateDir := t.TempDir(), t.TempDir()
	_ = os.Mkdir(filepath.Join(outDir, "s"), 0700)
	original := filepath.Join(outDir, "s", "file")
	_ = os.WriteFile(original, []byte("original"), 0600)
	_ = os.WriteFile(filepath.Join(privateDir, "file"), []byte("wrong-size"), 0600)
	out, _ := os.OpenRoot(outDir)
	defer out.Close()
	private, _ := os.OpenRoot(privateDir)
	defer private.Close()
	s := &Service{}
	entry := Entry{SourceID: "s", Path: "file", Kind: "file", Size: 1, Mode: 0600, SnapshotPath: "/file"}
	if err := s.copyRestored(out, private, entry, true, restoreLayoutBySource); err == nil {
		t.Fatal("expected size mismatch")
	}
	got, _ := os.ReadFile(original)
	if string(got) != "original" {
		t.Fatal("failed restore modified pre-existing target")
	}
	entry.Size = int64(len("wrong-size"))
	if err := s.copyRestored(out, private, entry, false, restoreLayoutBySource); err == nil {
		t.Fatal("expected conflict")
	}
	got, _ = os.ReadFile(original)
	if string(got) != "original" {
		t.Fatal("non-overwrite restore modified target")
	}
	if err := s.copyRestored(out, private, entry, true, restoreLayoutBySource); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(original)
	if string(got) != "wrong-size" {
		t.Fatal("approved overwrite failed")
	}
}
func TestRestoreLinkParentCannotEscape(t *testing.T) {
	outside, outDir, privateDir := t.TempDir(), t.TempDir(), t.TempDir()
	_ = os.Symlink(outside, filepath.Join(outDir, "s"))
	_ = os.WriteFile(filepath.Join(privateDir, "file"), []byte("safe"), 0600)
	out, _ := os.OpenRoot(outDir)
	defer out.Close()
	private, _ := os.OpenRoot(privateDir)
	defer private.Close()
	s := &Service{}
	e := Entry{SourceID: "s", Path: "file", Kind: "file", Size: 4, Mode: 0600, SnapshotPath: "/file"}
	if err := s.copyRestored(out, private, e, true, restoreLayoutBySource); err == nil {
		t.Fatal("symlink parent accepted")
	}
	if _, err := os.Stat(filepath.Join(outside, "file")); !os.IsNotExist(err) {
		t.Fatal("wrote outside authorized target")
	}
	if safeLink(Entry{Path: "dir/link", LinkTarget: "../../outside"}) {
		t.Fatal("unsafe link accepted")
	}
}
func TestScheduleUsesExplicitTimezone(t *testing.T) {
	schedule, err := parseSchedule(Plan{Schedule: "0 2 * * *", Timezone: "Asia/Shanghai"})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 9, 19, 0, 0, 0, time.UTC)
	next := schedule.Next(at)
	want := time.Date(2026, 10, 10, 18, 0, 0, 0, time.UTC)
	if !next.Equal(want) {
		t.Fatalf("next=%v want=%v", next, want)
	}
	if _, err = parseSchedule(Plan{Schedule: "0 2 * * *", Timezone: "invalid/timezone"}); err == nil {
		t.Fatal("invalid timezone accepted")
	}
}
func TestTransactionalReservationsPreventOverselling(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	keys := &platform.Keyring{}
	if err = keys.SetMaster(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	s := &Service{rt: &platform.Runtime{Store: db, Keys: keys}}
	account := Account{ID: "a", Budget: 20 << 20, Free: 100 << 20, Verified: true, Enabled: true}
	if _, err = db.Save(context.Background(), "accounts", account.ID, 0, account); err != nil {
		t.Fatal(err)
	}
	pool := Pool{AccountIDs: []string{"a"}, CatalogReplicas: 1, ShardTarget: 100 << 20}
	for _, id := range []string{"r1", "r2"} {
		_, err = db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,mtime_ns) VALUES(?,?,?,?,?,?)", id, "s", "big.bin", "file", 8<<20, 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	first, _, err := s.allocate(context.Background(), &Task{ID: "r1", Kind: "backup"}, Plan{ID: "p1"}, pool, map[string]Account{"a": account})
	if err != nil || len(first.Shards) != 1 {
		t.Fatalf("first reservation: %v", err)
	}
	if _, _, err = s.allocate(context.Background(), &Task{ID: "r2", Kind: "backup"}, Plan{ID: "p2"}, pool, map[string]Account{"a": account}); err == nil {
		t.Fatal("second task oversold reserved capacity")
	}
	var count int
	if err = db.DB.QueryRow("SELECT count(*) FROM capacity_reservations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("reservations=%d error=%v", count, err)
	}
}
func TestRestoreSelectionStringCompatibility(t *testing.T) {
	var r RestoreRequest
	if err := json.Unmarshal([]byte(`{"point_id":"p","layout":"by_source","paths":["s/中文/a.txt"]}`), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Paths) != 1 || r.Paths[0].Path != "中文/a.txt" || r.Layout != restoreLayoutBySource {
		t.Fatalf("unexpected selection %#v", r)
	}
}

func TestMergedTriggerSurvivesRestartOnce(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rt := &platform.Runtime{Store: db, Keys: &platform.Keyring{}, Config: platform.Config{GlobalConcurrency: 1}}
	plan := Plan{ID: "p", Name: "pending test", SourceIDs: []string{"s"}, PoolID: "pool"}
	if _, err = db.Save(context.Background(), "backup_plans", plan.ID, 0, plan); err != nil {
		t.Fatal(err)
	}
	first, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := first.enqueue("backup", plan.ID, "initial", nil)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := first.enqueue("backup", plan.ID, "merged", nil)
	if err != nil || merged.ID != initial.ID {
		t.Fatalf("trigger did not merge: %+v %v", merged, err)
	}
	value, err := db.Meta(context.Background(), "pending:"+plan.ID)
	if err != nil || value != "1" {
		t.Fatalf("pending trigger was not persisted: %q %v", value, err)
	}
	first.Close()
	restarted, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.replayPending()
	if len(restarted.queue) != 1 || restarted.active[plan.ID] == initial.ID {
		t.Fatal("pending intent did not replay as exactly one fresh task")
	}
	duplicate, err := restarted.enqueue("backup", plan.ID, "merged", nil)
	if err != nil || duplicate.ID != initial.ID {
		t.Fatal("merged trigger idempotency did not survive restart")
	}
}

func TestRestorePreflightRejectsSingleFileLargerThanCache(t *testing.T) {
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	restore, cache := filepath.Join(root, "restore"), filepath.Join(root, "cache")
	_ = os.Mkdir(restore, 0700)
	_ = os.Mkdir(cache, 0700)
	db, err := store.Open(filepath.Join(root, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	account := Account{ID: "a", Backend: "local", LocalPath: root, Verified: true}
	repo := Repository{ID: "repo", AccountID: account.ID}
	point := Point{ID: "p", RunID: "run", Status: "completed", FileCount: 1, LogicalBytes: 4096}
	for table, value := range map[string]any{"accounts": account, "repositories": repo, "recovery_points": point} {
		raw, _ := json.Marshal(value)
		var object map[string]any
		_ = json.Unmarshal(raw, &object)
		if _, err = db.Save(ctx, table, object["id"].(string), 0, value); err != nil {
			t.Fatal(err)
		}
	}
	_, err = db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,repo_id,snapshot_path) VALUES(?,?,?,?,?,?,?)", "run", "s", "large-file", "file", 4096, "repo", "/large-file")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{rt: &platform.Runtime{Store: db, Config: platform.Config{CacheDir: cache, RestoreRoots: []string{restore}, CacheMaxBytes: 2048}}}
	preview, err := s.restorePreview(ctx, RestoreRequest{PointID: point.ID, Target: filepath.Join(restore, "new-target")})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Blocked || preview.Code != "CACHE_LIMIT_EXCEEDED" {
		t.Fatalf("oversized file accepted: %+v", preview)
	}
}

func TestNewFileReusesRepositoryWithoutSecondInitializationReservation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	keys := &platform.Keyring{}
	_ = keys.SetMaster(make([]byte, 32))
	s := &Service{rt: &platform.Runtime{Store: db, Keys: keys}}
	account := Account{ID: "a", Budget: 34 << 20, MaintenanceReserve: 1 << 20, Free: 100 << 20, Verified: true, Enabled: true}
	if _, err = db.Save(ctx, "accounts", account.ID, 0, account); err != nil {
		t.Fatal(err)
	}
	pool := Pool{AccountIDs: []string{account.ID}, ShardTarget: 24 << 20}
	for _, entry := range []Entry{{SourceID: "s", Path: "", Kind: "dir"}, {SourceID: "s", Path: "large", Kind: "file", Size: 20 << 20}} {
		_, err = db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size) VALUES(?,?,?,?,?)", "r", entry.SourceID, entry.Path, entry.Kind, entry.Size)
		if err != nil {
			t.Fatal(err)
		}
	}
	allocation, _, err := s.allocate(ctx, &Task{ID: "r", Kind: "backup"}, Plan{ID: "p"}, pool, map[string]Account{account.ID: account})
	if err != nil {
		t.Fatalf("reusable shard rejected affordable file: %v", err)
	}
	if len(allocation.Shards) != 1 {
		t.Fatalf("expected single reused repository: %+v", allocation)
	}
}

func TestMigrateRejectsTargetOutsidePlanPool(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	plan := Plan{ID: "plan", PoolID: "pool"}
	pool := Pool{ID: "pool", AccountIDs: []string{"a", "b"}}
	repo := Repository{ID: "repo", PlanID: plan.ID, AccountID: "a", Path: "original", Locations: []Location{{AccountID: "a", Path: "original", Verified: true}}}
	for _, item := range []struct {
		table, id string
		value     any
	}{
		{"backup_plans", plan.ID, plan}, {"storage_pools", pool.ID, pool}, {"repositories", repo.ID, repo},
		{"accounts", "a", Account{ID: "a", Verified: true, Enabled: true}},
		{"accounts", "c", Account{ID: "c", Verified: true, Enabled: true}},
	} {
		if _, err = db.Save(ctx, item.table, item.id, 0, item.value); err != nil {
			t.Fatal(err)
		}
	}
	s := &Service{rt: &platform.Runtime{Store: db, Config: platform.Config{RcloneBin: filepath.Join(t.TempDir(), "missing-rclone")}}}
	request, _ := json.Marshal(MigrateRequest{RepositoryID: repo.ID, TargetAccountID: "c", Confirm: true})
	err = s.migrate(ctx, &Task{ID: "migration", Request: request})
	failure, ok := err.(*failure)
	if !ok || failure.Code != "MIGRATION_TARGET_OUTSIDE_POOL" {
		t.Fatalf("expected pool validation before engine call: %v", err)
	}
	var got Repository
	if err = db.Get(ctx, "repositories", repo.ID, &got); err != nil {
		t.Fatal(err)
	}
	if got.AccountID != repo.AccountID || got.Path != repo.Path || len(got.Locations) != 1 || got.Locations[0] != repo.Locations[0] {
		t.Fatalf("migration changed repository: %+v", got)
	}
}

func TestRetentionPrecheckFailsBeforeCommit(t *testing.T) {
	for _, scenario := range []string{"reserve", "missing", "disabled", "unverified"} {
		t.Run(scenario, func(t *testing.T) {
			db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			drive := t.TempDir()
			plan := Plan{ID: "plan", PoolID: "pool", Timezone: "UTC"}
			account := Account{ID: "account", Backend: "local", LocalPath: drive, Verified: true, Enabled: true, MaintenanceReserve: 1}
			if scenario == "reserve" {
				account.MaintenanceReserve = 0
			}
			if scenario == "disabled" {
				account.Enabled = false
			}
			if scenario == "unverified" {
				account.Verified = false
			}
			pool := Pool{ID: "pool", AccountIDs: []string{account.ID}, CatalogReplicas: 1}
			repo := Repository{ID: "repo", PlanID: plan.ID, AccountID: account.ID}
			older := Point{ID: "older", PlanID: plan.ID, Status: "completed", FinishedAt: "2026-10-01T00:00:00Z", FileCount: 1, Snapshots: []Snapshot{{RepositoryID: repo.ID, SnapshotID: "old-snapshot"}}}
			newer := Point{ID: "newer", PlanID: plan.ID, Status: "completed", FinishedAt: "2026-10-02T00:00:00Z", FileCount: 1, Snapshots: []Snapshot{{RepositoryID: repo.ID, SnapshotID: "new-snapshot"}}}
			for _, item := range []struct {
				table, id string
				value     any
			}{{"backup_plans", plan.ID, plan}, {"storage_pools", pool.ID, pool}, {"repositories", repo.ID, repo}, {"recovery_points", older.ID, older}, {"recovery_points", newer.ID, newer}} {
				if _, err = db.Save(ctx, item.table, item.id, 0, item.value); err != nil {
					t.Fatal(err)
				}
			}
			if scenario != "missing" {
				if _, err = db.Save(ctx, "accounts", account.ID, 0, account); err != nil {
					t.Fatal(err)
				}
			}
			s := &Service{rt: &platform.Runtime{Store: db, Config: platform.Config{CacheDir: t.TempDir()}}}
			request, _ := json.Marshal(RetentionRequest{PlanID: plan.ID, PointIDs: []string{older.ID}, Confirm: true})
			err = s.retain(ctx, &Task{ID: "retention", Request: request})
			failure, ok := err.(*failure)
			code := "RETENTION_ACCOUNT_UNAVAILABLE"
			if scenario == "reserve" {
				code = "MAINTENANCE_RESERVE_REQUIRED"
			}
			if !ok || failure.Code != code {
				t.Fatalf("expected precheck %s: %v", code, err)
			}
			if err = db.Get(ctx, "recovery_points", older.ID, &older); err != nil {
				t.Fatal(err)
			}
			if older.DeletedAt != "" || older.Status != "completed" {
				t.Fatalf("point deleted before precheck: %+v", older)
			}
			if err = db.Get(ctx, "repositories", repo.ID, &repo); err != nil {
				t.Fatal(err)
			}
			if len(repo.PendingForget) != 0 {
				t.Fatalf("pending cleanup changed: %+v", repo)
			}
			if err = filepath.WalkDir(drive, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if entry.Name() == "catalog" {
					t.Fatal("catalog published before precheck")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRetentionRetrySelectionAndReferenceProtection(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	for _, planID := range []string{"cleaned", "fresh", "blocked"} {
		if _, err = db.Save(ctx, "backup_plans", planID, 0, Plan{ID: planID, PoolID: "pool", Timezone: "UTC"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Save(ctx, "storage_pools", "pool", 0, Pool{ID: "pool"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Save(ctx, "recovery_points", "deleted", 0, Point{ID: "deleted", PlanID: "cleaned", Status: "deleted", DeletedAt: now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Save(ctx, "recovery_points", "incomplete", 0, Point{ID: "incomplete", PlanID: "blocked", Status: "pending"}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Save(ctx, "repositories", "pending", 0, Repository{ID: "pending", PlanID: "blocked", PendingForget: []string{"snap"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Save(ctx, "repositories", "retired", 0, Repository{ID: "retired", PlanID: "fresh", Status: "retired", PendingForget: []string{"deleted-repo-snapshot"}}); err != nil {
		t.Fatal(err)
	}
	s := &Service{rt: &platform.Runtime{Store: db}}
	for _, scenario := range []struct{ plan, code string }{{"cleaned", ""}, {"fresh", "EMPTY_RETENTION_SELECTION"}, {"blocked", "RETENTION_PAUSED"}} {
		request, _ := json.Marshal(RetentionRequest{PlanID: scenario.plan, PointIDs: []string{}, Confirm: true})
		err = s.retain(ctx, &Task{ID: uid(), Request: request})
		if scenario.code == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		failure, ok := err.(*failure)
		if !ok || failure.Code != scenario.code {
			t.Fatalf("%s expected %s: %v", scenario.plan, scenario.code, err)
		}
	}
	raws := []json.RawMessage{}
	for _, p := range []Point{{ID: "selected", Snapshots: []Snapshot{{RepositoryID: "repo", SnapshotID: "shared"}, {RepositoryID: "repo", SnapshotID: "unique"}, {RepositoryID: "repo", SnapshotID: "unique"}}}, {ID: "live", Snapshots: []Snapshot{{RepositoryID: "repo", SnapshotID: "shared"}}}} {
		raw, _ := json.Marshal(p)
		raws = append(raws, raw)
	}
	snapshots, err := retentionSnapshots(raws, map[string]bool{"selected": true})
	if err != nil || len(snapshots["repo"]) != 1 || snapshots["repo"][0] != "unique" {
		t.Fatalf("live reference or deduplication failed: %v %v", snapshots, err)
	}
}

func TestRetentionRegistrationIsAtomic(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	repo := Repository{ID: "repo", PlanID: "plan", PendingForget: []string{"existing"}}
	point := Point{ID: "point", PlanID: "plan", Status: "completed"}
	if _, err = db.Save(ctx, "repositories", repo.ID, 0, repo); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Save(ctx, "recovery_points", point.ID, 0, point); err != nil {
		t.Fatal(err)
	}
	s := &Service{rt: &platform.Runtime{Store: db}}
	selected, snapshots := map[string]bool{point.ID: true}, map[string][]string{repo.ID: {"new", "new"}}
	for _, trigger := range []string{"RAISE(ABORT, 'injected-write-failure')", "RAISE(IGNORE)"} {
		if _, err = db.DB.Exec("CREATE TRIGGER reject_retention BEFORE UPDATE ON recovery_points BEGIN SELECT " + trigger + "; END"); err != nil {
			t.Fatal(err)
		}
		err = s.recordRetentionDeletion(ctx, selected, snapshots)
		if err == nil {
			t.Fatal("injected write failure was ignored")
		}
		if trigger == "RAISE(IGNORE)" && !errors.Is(err, store.ErrConflict) {
			t.Fatalf("expected version conflict: %v", err)
		}
		var gotRepo Repository
		var gotPoint Point
		if err = db.Get(ctx, "repositories", repo.ID, &gotRepo); err != nil {
			t.Fatal(err)
		}
		if err = db.Get(ctx, "recovery_points", point.ID, &gotPoint); err != nil {
			t.Fatal(err)
		}
		if gotRepo.Version != 1 || len(gotRepo.PendingForget) != 1 || gotRepo.PendingForget[0] != "existing" || gotPoint.DeletedAt != "" || gotPoint.Status != "completed" {
			t.Fatalf("partial local registration: %+v %+v", gotRepo, gotPoint)
		}
		if _, err = db.DB.Exec("DROP TRIGGER reject_retention"); err != nil {
			t.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err = s.recordRetentionDeletion(canceled, selected, snapshots); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled registration: %v", err)
	}
	if err = s.recordRetentionDeletion(ctx, selected, snapshots); err != nil {
		t.Fatal(err)
	}
	if err = db.Get(ctx, "repositories", repo.ID, &repo); err != nil {
		t.Fatal(err)
	}
	if err = db.Get(ctx, "recovery_points", point.ID, &point); err != nil {
		t.Fatal(err)
	}
	if len(repo.PendingForget) != 2 || repo.PendingForget[0] != "existing" || repo.PendingForget[1] != "new" || point.DeletedAt == "" || point.Status != "deleted" {
		t.Fatalf("registration not durable: %+v %+v", repo, point)
	}
}
