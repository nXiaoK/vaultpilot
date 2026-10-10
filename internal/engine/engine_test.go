package engine

import (
	"context"
	"encoding/json"
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
	if err := s.copyRestored(out, private, entry, true); err == nil {
		t.Fatal("expected size mismatch")
	}
	got, _ := os.ReadFile(original)
	if string(got) != "original" {
		t.Fatal("failed restore modified pre-existing target")
	}
	entry.Size = int64(len("wrong-size"))
	if err := s.copyRestored(out, private, entry, false); err == nil {
		t.Fatal("expected conflict")
	}
	got, _ = os.ReadFile(original)
	if string(got) != "original" {
		t.Fatal("non-overwrite restore modified target")
	}
	if err := s.copyRestored(out, private, entry, true); err != nil {
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
	if err := s.copyRestored(out, private, e, true); err == nil {
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
	if err := json.Unmarshal([]byte(`{"point_id":"p","paths":["s/中文/a.txt"]}`), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Paths) != 1 || r.Paths[0].Path != "中文/a.txt" {
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
	account := Account{ID: "a", Budget: 34 << 20, MaintenanceReserve: 1 << 20, Free: 100 << 20}
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
