package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"vaultpilot/internal/platform"
)

func catalogRefreshNode(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	config := platform.Config{StateDir: filepath.Join(root, "state"), CacheDir: filepath.Join(root, "cache"), AllowLocal: true}
	rt, err := platform.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err = rt.Store.SetMeta(context.Background(), "vault_id", "11111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if err = rt.Close(); err != nil {
		t.Fatal(err)
	}
	rt, err = platform.New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rt.Close() })
	if err = rt.Keys.SetMaster(bytes.Repeat([]byte{42}, 32)); err != nil {
		t.Fatal(err)
	}
	s, err := New(rt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func catalogRefreshFixture(t *testing.T) (*Service, *Service, Pool, []Account) {
	t.Helper()
	writer, reader := catalogRefreshNode(t), catalogRefreshNode(t)
	accounts := []Account{
		{ID: "account-one", Backend: "local", DriveID: "drive-one", Enabled: true, Verified: true, LocalPath: t.TempDir()},
		{ID: "account-two", Backend: "local", DriveID: "drive-two", Enabled: true, Verified: true, LocalPath: t.TempDir()},
	}
	ctx := context.Background()
	for _, node := range []*Service{writer, reader} {
		for _, account := range accounts {
			if _, err := node.rt.Store.Save(ctx, "accounts", account.ID, 0, account); err != nil {
				t.Fatal(err)
			}
		}
	}
	repo := Repository{ID: "repository-one", AccountID: accounts[1].ID, Path: "repo-path", Status: "ready", Locations: []Location{{DriveID: accounts[1].DriveID, AccountID: accounts[1].ID, Path: "repo-path", Verified: true}}}
	if _, err := writer.rt.Store.Save(ctx, "repositories", repo.ID, 0, repo); err != nil {
		t.Fatal(err)
	}
	pool := Pool{CatalogReplicas: 2, AccountIDs: []string{accounts[0].ID, accounts[1].ID}}
	catalogRefreshPublish(t, writer, pool, "point-one", "first.txt")
	return writer, reader, pool, accounts
}

func catalogRefreshPublish(t *testing.T, s *Service, pool Pool, id, path string) Point {
	t.Helper()
	ctx := context.Background()
	point := Point{ID: id, RunID: "writer-" + uid(), CommitID: uid(), Status: "complete", FileCount: 1, LogicalBytes: 10, Snapshots: []Snapshot{{RepositoryID: "repository-one", SnapshotID: "snapshot-" + id}}}
	if _, err := s.rt.Store.DB.ExecContext(ctx, "INSERT INTO file_entries(run_id,source_id,path,kind,size,repo_id,snapshot_path) VALUES(?,?,?,?,?,?,?)", point.RunID, "source-one", path, "file", 10, "repository-one", "/"+path); err != nil {
		t.Fatal(err)
	}
	task := Task{ID: uid(), Kind: "backup"}
	if err := s.commit(ctx, &task, pool, &point, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.rt.Store.Save(ctx, "recovery_points", point.ID, -1, point); err != nil {
		t.Fatal(err)
	}
	return point
}

func catalogRefreshDiscover(t *testing.T, s *Service) Task {
	t.Helper()
	task := Task{ID: uid(), Kind: "discover"}
	if err := s.discover(context.Background(), &task); err != nil {
		t.Fatal(err)
	}
	return task
}

func catalogRefreshEntryCount(t *testing.T, s *Service) int {
	t.Helper()
	var count int
	if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestCatalogRepeatedDiscoveryReusesAuthenticatedCertificate(t *testing.T) {
	_, reader, _, accounts := catalogRefreshFixture(t)
	first := catalogRefreshDiscover(t, reader)
	commit := first.Result.(map[string]any)["commit_id"].(string)
	var original Point
	if err := reader.rt.Store.Get(context.Background(), "recovery_points", "point-one", &original); err != nil {
		t.Fatal(err)
	}
	// Removing payloads proves repeated discovery only reads certificates.
	for _, account := range accounts {
		if err := os.Remove(reader.remoteFile(account, "VaultPilot/"+reader.rt.VaultID()+"/catalog/"+commit+".vpc")); err != nil {
			t.Fatal(err)
		}
	}
	// An authenticated-looking newer marker with no valid catalog must fall back.
	id := uid()
	marker := catalogMarker{Format: 1, VaultID: reader.rt.VaultID(), CommitID: id, SHA256: "invalid-digest", State: "committed", PreparedReplicas: 2, Drives: []string{"drive-one", "drive-two"}, CreatedAt: "9999-01-01T00:00:00Z"}
	raw, _ := json.Marshal(marker)
	sealed, err := reader.rt.Keys.Seal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(reader.remoteFile(accounts[0], "VaultPilot/"+reader.rt.VaultID()+"/catalog/"+id+".ok"), sealed, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		task := catalogRefreshDiscover(t, reader)
		if task.Result.(map[string]any)["unchanged"] != true {
			t.Fatalf("unchanged commit rebuilt: %#v", task.Result)
		}
	}
	var current Point
	if err = reader.rt.Store.Get(context.Background(), "recovery_points", original.ID, &current); err != nil {
		t.Fatal(err)
	}
	if current.RunID != original.RunID || current.Version != original.Version || catalogRefreshEntryCount(t, reader) != 1 {
		t.Fatalf("repeated discovery changed or leaked entries: old=%+v new=%+v", original, current)
	}
}

func TestCatalogNewCommitReusesOldPointManifests(t *testing.T) {
	writer, reader, pool, _ := catalogRefreshFixture(t)
	catalogRefreshDiscover(t, reader)
	var original Point
	if err := reader.rt.Store.Get(context.Background(), "recovery_points", "point-one", &original); err != nil {
		t.Fatal(err)
	}
	catalogRefreshPublish(t, writer, pool, "point-two", "second.txt")
	task := catalogRefreshDiscover(t, reader)
	if task.Result.(map[string]any)["unchanged"] != false {
		t.Fatalf("new commit skipped: %#v", task.Result)
	}
	var current Point
	if err := reader.rt.Store.Get(context.Background(), "recovery_points", original.ID, &current); err != nil {
		t.Fatal(err)
	}
	if current.RunID != original.RunID || catalogRefreshEntryCount(t, reader) != 2 {
		t.Fatalf("new commit recopied old manifest: old=%+v current=%+v count=%d", original, current, catalogRefreshEntryCount(t, reader))
	}
	catalogRefreshDiscover(t, reader)
	if catalogRefreshEntryCount(t, reader) != 2 {
		t.Fatal("repeat new commit leaked entries")
	}
}

func TestCatalogAccountChangesRemapWithoutReimport(t *testing.T) {
	_, reader, _, accounts := catalogRefreshFixture(t)
	ctx := context.Background()
	catalogRefreshDiscover(t, reader)
	var original Point
	if err := reader.rt.Store.Get(ctx, "recovery_points", "point-one", &original); err != nil {
		t.Fatal(err)
	}
	account := accounts[1]
	account.Enabled = false
	if _, err := reader.rt.Store.Save(ctx, "accounts", account.ID, -1, account); err != nil {
		t.Fatal(err)
	}
	catalogRefreshDiscover(t, reader)
	var missing Repository
	if err := reader.rt.Store.Get(ctx, "repositories", "repository-one", &missing); err != nil {
		t.Fatal(err)
	}
	var degraded Point
	if err := reader.rt.Store.Get(ctx, "recovery_points", original.ID, &degraded); err != nil {
		t.Fatal(err)
	}
	if missing.AccountID != "" || missing.Status != "account_missing" || degraded.ProtectionStatus != "degraded" {
		t.Fatalf("account removal left stale availability: %+v %+v", missing, degraded)
	}
	if err := reader.rt.Store.Delete(ctx, "accounts", account.ID); err != nil {
		t.Fatal(err)
	}
	account.ID, account.Enabled = "reauthorized-account", true
	if _, err := reader.rt.Store.Save(ctx, "accounts", account.ID, 0, account); err != nil {
		t.Fatal(err)
	}
	catalogRefreshDiscover(t, reader)
	var remapped Repository
	if err := reader.rt.Store.Get(ctx, "repositories", missing.ID, &remapped); err != nil {
		t.Fatal(err)
	}
	var current Point
	if err := reader.rt.Store.Get(ctx, "recovery_points", original.ID, &current); err != nil {
		t.Fatal(err)
	}
	if remapped.AccountID != account.ID || remapped.Status != "ready" || current.ProtectionStatus != "complete" || current.RunID != original.RunID || catalogRefreshEntryCount(t, reader) != 1 {
		t.Fatalf("reauthorization did not safely remap: %+v %+v", remapped, current)
	}
}

func TestCatalogFailedImportKeepsPublishedIndex(t *testing.T) {
	writer, reader, pool, _ := catalogRefreshFixture(t)
	ctx := context.Background()
	catalogRefreshDiscover(t, reader)
	var original Point
	if err := reader.rt.Store.Get(ctx, "recovery_points", "point-one", &original); err != nil {
		t.Fatal(err)
	}
	// The latest catalog first stages a valid new point, then fails on another
	// manifest. Neither that staged point nor its entries may become visible.
	catalogRefreshPublish(t, writer, pool, "point-two", "second.txt")
	catalogRefreshPublish(t, writer, pool, "bad-point", "../escape")
	task := Task{ID: uid(), Kind: "discover"}
	if err := reader.discover(ctx, &task); err == nil {
		t.Fatal("invalid manifest published")
	}
	var current Point
	if err := reader.rt.Store.Get(ctx, "recovery_points", original.ID, &current); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := reader.rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if current.RunID != original.RunID || current.Version != original.Version || count != 1 || catalogRefreshEntryCount(t, reader) != 1 {
		t.Fatalf("failed import disturbed published index: %+v %+v count=%d", original, current, count)
	}
}
