package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRealResticUnreadableSourceExplainsReadOnlyGrant(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	s, plan, _, _ := realFixture(t)
	var source Source
	if err := s.rt.Store.Get(context.Background(), "sources", plan.SourceIDs[0], &source); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(source.Path, "file.txt")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0600) })
	_, err := s.scan(context.Background(), &Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}, plan)
	var f *failure
	if !errors.As(err, &f) || f.Code != "SOURCE_READ_FAILED" || !strings.Contains(f.Message, "UID=") || !strings.Contains(f.Message, "不要用 chown/chmod") {
		t.Fatalf("unreadable source lacks read-only guidance: %v", err)
	}
	if info, err := os.Stat(locked); err != nil || info.Mode().Perm() != 0 {
		t.Fatalf("scan changed source permissions: %v %v", info, err)
	}
}

func TestRealResticManifestDirectoriesKeepPrivatePermissions(t *testing.T) {
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
	parent := ".@#local/trash"
	nested := parent + "/1Panel/database"
	if err := os.MkdirAll(filepath.Join(source.Path, nested, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	content := []byte("database backup contents")
	if err := os.WriteFile(filepath.Join(source.Path, nested, "backup.sql"), content, 0600); err != nil {
		t.Fatal(err)
	}
	backup := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	sources, err := s.scan(ctx, &backup, plan)
	if err != nil {
		t.Fatal(err)
	}
	// A source may be readable through an ACL or a different owner/group even
	// when its owner bits lack execute. Inject the scanned metadata so the test
	// is portable and does not require root, chown or platform-specific ACLs.
	dirTime := time.Unix(1_700_000_000, 0)
	if _, err := s.rt.Store.DB.ExecContext(ctx, "UPDATE file_entries SET mode=?,mtime_ns=? WHERE run_id=? AND source_id=? AND path=?", 0400, dirTime.UnixNano(), backup.ID, source.ID, parent); err != nil {
		t.Fatal(err)
	}
	accounts, err := s.accounts(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	allocation, repos, err := s.allocate(ctx, &backup, plan, pool, accounts)
	if err != nil || len(allocation.Shards) != 1 {
		t.Fatalf("allocation: %+v %v", allocation, err)
	}
	stage := t.TempDir()
	repo := repos[allocation.Shards[0].RepositoryID]
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(stage, repo.ID, source.ID, parent), 0700) })
	snapshot, err := s.backupShard(ctx, &backup, plan, repo, account, sources, stage)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(stage, repo.ID)
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err == nil && (!info.IsDir() || info.Mode().Perm() != 0700) {
			t.Errorf("unsafe staging permissions: %s %v", path, info.Mode())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The workaround must not chmod the user's source directory.
	info, err := os.Stat(filepath.Join(source.Path, parent))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("source permissions changed: %v %v", info, err)
	}
	repo.Status = "ready"
	if _, err := s.rt.Store.Save(ctx, "repositories", repo.ID, -1, repo); err != nil {
		t.Fatal(err)
	}
	point := Point{ID: uid(), PlanID: plan.ID, RunID: backup.ID, Status: "completed", CommitID: uid(), StartedAt: backup.StartedAt, FinishedAt: now(), FileCount: allocation.FileCount, LogicalBytes: allocation.LogicalBytes, Snapshots: []Snapshot{{RepositoryID: repo.ID, SnapshotID: snapshot}}}
	if err := s.commit(ctx, &backup, pool, &point, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.rt.Store.Save(ctx, "recovery_points", point.ID, 0, point); err != nil {
		t.Fatal(err)
	}
	target := s.rt.Config.RestoreRoots[0]
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	target, err = filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	restoredParent := filepath.Join(target, parent)
	t.Cleanup(func() { _ = os.Chmod(restoredParent, 0700) })
	request, _ := json.Marshal(RestoreRequest{PointID: point.ID, Target: target, Layout: restoreLayoutDirect})
	restore := Task{ID: uid(), Kind: "restore", Request: request}
	if err := s.restore(ctx, &restore); err != nil {
		t.Fatal(err)
	}
	info, err = os.Stat(restoredParent)
	if err != nil || info.Mode().Perm() != 0400 || !info.ModTime().Equal(dirTime) {
		t.Fatalf("lost original directory metadata: %v %v", info, err)
	}
	// Reopen the intentionally unsearchable directory for content assertions.
	if err := os.Chmod(restoredParent, 0700); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(target, nested, "backup.sql"))
	if err != nil || string(got) != string(content) {
		t.Fatalf("restored contents: %q %v", got, err)
	}
	if info, err := os.Stat(filepath.Join(target, nested, "empty")); err != nil || !info.IsDir() {
		t.Fatalf("missing empty directory: %v %v", info, err)
	}
	if err := os.RemoveAll(stage); err != nil {
		t.Fatalf("staging cleanup: %v", err)
	}
}
