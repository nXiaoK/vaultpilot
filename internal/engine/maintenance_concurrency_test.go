package engine

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"vaultpilot/internal/platform"
)

type maintenanceGateFixture struct {
	s              *Service
	root           string
	source, target Account
	plan           Plan
	pool           Pool
	repo           Repository
}

type maintenanceGateRun struct {
	id     string
	done   chan struct{}
	err    error
	cancel context.CancelFunc
}

func newMaintenanceGateFixture(t *testing.T, phase string) *maintenanceGateFixture {
	t.Helper()
	f := &maintenanceGateFixture{s: catalogRefreshNode(t), root: t.TempDir()}
	f.source = Account{ID: "source", Backend: "local", LocalPath: t.TempDir(), DriveID: "source-drive", Region: "global", TenantID: "consumers", Enabled: true, Verified: true, Budget: 64 << 20, MaintenanceReserve: 2 << 20}
	f.target = f.source
	f.target.ID, f.target.LocalPath, f.target.DriveID = "target", t.TempDir(), "target-drive"
	f.pool = Pool{ID: "pool", AccountIDs: []string{f.source.ID, f.target.ID}, CatalogReplicas: 1}
	f.plan = Plan{ID: "plan", PoolID: f.pool.ID}
	password, err := f.s.rt.Keys.Seal([]byte("disposable-maintenance-password"))
	if err != nil {
		t.Fatal(err)
	}
	f.repo = Repository{ID: "repository", PlanID: f.plan.ID, AccountID: f.source.ID, Path: "VaultPilot/" + f.s.rt.VaultID() + "/repositories/repository", Password: base64.StdEncoding.EncodeToString(password), Status: "ready"}
	f.repo.Locations = []Location{{AccountID: f.source.ID, DriveID: f.source.DriveID, Region: f.source.Region, TenantID: f.source.TenantID, Path: f.repo.Path, Verified: true}}
	for _, row := range []struct {
		table, id string
		value     any
	}{{"accounts", f.source.ID, f.source}, {"accounts", f.target.ID, f.target}, {"storage_pools", f.pool.ID, f.pool}, {"backup_plans", f.plan.ID, f.plan}, {"repositories", f.repo.ID, f.repo}} {
		if _, err := f.s.rt.Store.Save(t.Context(), row.table, row.id, 0, row.value); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(f.s.remoteFile(f.source, f.repo.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.s.remoteFile(f.source, f.repo.Path), "payload"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	gate := "gate() {\n if [ -f " + quote(f.root) + "/block.$1 ]; then\n  : > " + quote(f.root) + "/entered.$1\n  while [ ! -f " + quote(f.root) + "/release.$1 ]; do sleep 0.01; done\n fi\n}\n"
	rclone := "#!/bin/sh\nset -eu\n" + gate + "[ \"$1\" != --config ] || shift 2\ncommand=$1\nshift\ncase $command in\n size) printf '{\"bytes\":4}\\n';;\n copy) gate copy; mkdir -p \"$2\"; cp -R \"$1/.\" \"$2/\";;\n copyto) mkdir -p \"$(dirname \"$2\")\"; cp \"$1\" \"$2\";;\n purge) gate purge; [ -d \"$1\" ] || exit 3; rm -rf \"$1\";;\n *) exit 71;;\nesac\n"
	restic := "#!/bin/sh\nset -eu\n" + gate + "command=\nfor arg do\n case $arg in check|snapshots|forget|prune|cat) command=$arg;; esac\ndone\ncase $command in\n check) gate read-data;;\n snapshots) cat " + quote(filepath.Join(f.root, "snapshots.json")) + ";;\n prune) gate prune; [ ! -f " + quote(filepath.Join(f.root, "fail.prune")) + " ] || exit 73;;\n forget) printf '%s\\n' \"$@\" >> " + quote(filepath.Join(f.root, "forget.log")) + ";;\n cat) :;;\n *) exit 72;;\nesac\n"
	for name, program := range map[string]string{"rclone": rclone, "restic": restic} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(program), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.root, "snapshots.json"), []byte("[]"), 0600); err != nil {
		t.Fatal(err)
	}
	f.s.rt.Config.RcloneBin, f.s.rt.Config.ResticBin = filepath.Join(f.root, "rclone"), filepath.Join(f.root, "restic")
	if phase != "" {
		if err := os.WriteFile(filepath.Join(f.root, "block."+phase), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *maintenanceGateFixture) release(phase string) {
	_ = os.WriteFile(filepath.Join(f.root, "release."+phase), nil, 0600)
}

func (f *maintenanceGateFixture) wait(t *testing.T, phase string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(f.root, "entered."+phase)); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance never reached %s barrier", phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *maintenanceGateFixture) waitStage(t *testing.T, id, stage string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var task Task
		if err := f.s.rt.Store.Get(t.Context(), "backup_runs", id, &task); err == nil && task.Stage == stage {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("maintenance did not reach %s", stage)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (f *maintenanceGateFixture) start(t *testing.T, kind string, request any, work func(context.Context, *Task) error) *maintenanceGateRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	run := &maintenanceGateRun{id: uid(), done: make(chan struct{}), cancel: cancel}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		run.err = work(ctx, &Task{ID: run.id, Kind: kind, PlanID: f.plan.ID, Request: raw})
		close(run.done)
	}()
	t.Cleanup(func() {
		cancel()
		for _, phase := range []string{"copy", "read-data", "prune", "purge"} {
			f.release(phase)
		}
		select {
		case <-run.done:
		case <-time.After(5 * time.Second):
			t.Error("maintenance did not stop during fixture cleanup")
		}
	})
	return run
}

func (run *maintenanceGateRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-run.done:
		return run.err
	case <-time.After(5 * time.Second):
		t.Fatal("maintenance did not finish after releasing its barrier")
		return nil
	}
}

// Run the real catalog serializer, encryption, local upload/readback and
// completion certificate protocol for another plan while data work is blocked.
func (f *maintenanceGateFixture) publishOtherPlan(t *testing.T) Point {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := f.s.catalogMu.LockContext(ctx); err != nil {
		t.Fatalf("other plan could not acquire catalog while maintenance data work was blocked: %v", err)
	}
	defer f.s.catalogMu.Unlock()
	point := Point{ID: uid(), RunID: uid(), PlanID: "other-plan", CommitID: uid(), Status: "completed", FileCount: 1, LogicalBytes: 4}
	if _, err := f.s.rt.Store.Save(ctx, "backup_plans", point.PlanID, 0, Plan{ID: point.PlanID, PoolID: f.pool.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.rt.Store.DB.ExecContext(ctx, "INSERT INTO file_entries(run_id,source_id,path,kind,size) VALUES(?,'source','other.txt','file',4)", point.RunID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.commit(ctx, &Task{ID: uid(), Kind: "backup", PlanID: point.PlanID}, f.pool, &point, nil); err != nil {
		t.Fatalf("other plan metadata commit failed: %v", err)
	}
	if _, err := f.s.rt.Store.Save(ctx, "recovery_points", point.ID, 0, point); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.s.remoteFile(f.source, "VaultPilot/"+f.s.rt.VaultID()+"/catalog/"+point.CommitID+".ok")); err != nil {
		t.Fatalf("other plan did not publish a completion certificate: %v", err)
	}
	return point
}

func TestMaintenanceMigrationDataWorkAllowsOtherPlanCommit(t *testing.T) {
	for _, phase := range []string{"copy", "read-data"} {
		t.Run(phase, func(t *testing.T) {
			f := newMaintenanceGateFixture(t, phase)
			run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}, f.s.migrate)
			f.wait(t, phase)
			other := f.publishOtherPlan(t)
			f.release(phase)
			if err := run.wait(t); err != nil {
				t.Fatalf("migration failed after other plan committed: %v", err)
			}
			var saved Point
			if err := f.s.rt.Store.Get(t.Context(), "recovery_points", other.ID, &saved); err != nil || saved.RunID != other.RunID {
				t.Fatalf("migration lost concurrent point: %+v %v", saved, err)
			}
		})
	}
}

func TestMaintenanceMigrationReleasesResourcesBeforeCatalogWait(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	f.s.catalogMu.Lock()
	var once sync.Once
	releaseCatalog := func() { once.Do(f.s.catalogMu.Unlock) }
	defer releaseCatalog()
	run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}, f.s.migrate)
	f.waitStage(t, run.id, "committing_location")
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	releaseResources, err := f.s.lockResources(ctx, []string{f.source.ID, f.target.ID}, f.repo.ID)
	if err != nil {
		t.Fatalf("catalog owner could not acquire resources held by waiting migration: %v", err)
	}
	releaseResources()
	releaseCatalog()
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenanceMigrationRejectsChangesDuringCopy(t *testing.T) {
	for _, change := range []string{"repository", "target_disabled", "target_drive", "target_removed", "quota_only"} {
		t.Run(change, func(t *testing.T) {
			f := newMaintenanceGateFixture(t, "copy")
			run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}, f.s.migrate)
			f.wait(t, "copy")
			table, id, field, value := "accounts", f.target.ID, "enabled", any(false)
			switch change {
			case "repository":
				table, id, field, value = "repositories", f.repo.ID, "status", "changed_during_copy"
			case "target_drive":
				field, value = "drive_id", "replacement-drive"
			case "target_removed":
				table, id, field, value = "storage_pools", f.pool.ID, "account_ids", []string{f.source.ID}
			case "quota_only":
				field, value = "quota_at", now()
			}
			var raw map[string]any
			if err := f.s.rt.Store.Get(t.Context(), table, id, &raw); err != nil {
				t.Fatal(err)
			}
			raw[field] = value
			if _, err := f.s.rt.Store.Save(t.Context(), table, id, int(raw["version"].(float64)), raw); err != nil {
				t.Fatal(err)
			}
			f.release("copy")
			err := run.wait(t)
			var repo Repository
			if readErr := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); readErr != nil {
				t.Fatal(readErr)
			}
			if change == "quota_only" {
				if err != nil || repo.AccountID != f.target.ID {
					t.Fatalf("quota-only version change incorrectly invalidated physical identity: %+v %v", repo, err)
				}
			} else if err == nil || repo.AccountID != f.source.ID || repo.Path != f.repo.Path {
				t.Fatalf("changed state published a migration mapping: %+v %v", repo, err)
			}
			if content, err := os.ReadFile(filepath.Join(f.s.remoteFile(f.source, f.repo.Path), "payload")); err != nil || string(content) != "data" {
				t.Fatalf("migration changed source repository data: %q %v", content, err)
			}
		})
	}
}

func (f *maintenanceGateFixture) migrateBeforeCleanup(t *testing.T) {
	t.Helper()
	run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}, f.s.migrate)
	if err := run.wait(t); err != nil {
		t.Fatalf("fixture migration: %v", err)
	}
}

func TestMaintenanceOldCopyDataWorkAllowsOtherPlanCommit(t *testing.T) {
	for _, phase := range []string{"read-data", "purge"} {
		t.Run(phase, func(t *testing.T) {
			f := newMaintenanceGateFixture(t, "")
			f.migrateBeforeCleanup(t)
			if err := os.WriteFile(filepath.Join(f.root, "block."+phase), nil, 0600); err != nil {
				t.Fatal(err)
			}
			run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, CleanupOld: true, OldLocationIndex: 0, OldLocationPath: f.repo.Path, Confirm: true}, f.s.migrate)
			f.wait(t, phase)
			f.publishOtherPlan(t)
			f.release(phase)
			if err := run.wait(t); err != nil {
				t.Fatalf("old-copy cleanup failed after concurrent commit: %v", err)
			}
			var repo Repository
			if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || repo.Locations[0].Status != "deleted" || repo.Locations[0].Verified {
				t.Fatalf("cleanup did not retire old location: %+v %v", repo, err)
			}
		})
	}
}

func TestMaintenanceOldCopyRejectsMappingChangeBeforeRetirement(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	f.migrateBeforeCleanup(t)
	f.s.catalogMu.Lock()
	var once sync.Once
	releaseCatalog := func() { once.Do(f.s.catalogMu.Unlock) }
	defer releaseCatalog()
	run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, CleanupOld: true, OldLocationIndex: 0, OldLocationPath: f.repo.Path, Confirm: true}, f.s.migrate)
	f.waitStage(t, run.id, "committing_old_copy_retirement")
	var changed Repository
	if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &changed); err != nil {
		t.Fatal(err)
	}
	changed.AccountID, changed.Path = f.source.ID, f.repo.Path
	if _, err := f.s.rt.Store.Save(t.Context(), "repositories", changed.ID, changed.Version, changed); err != nil {
		t.Fatal(err)
	}
	releaseCatalog()
	if err := run.wait(t); err == nil {
		t.Fatal("changed mapping was not rejected before deleting the old location")
	}
	if content, err := os.ReadFile(filepath.Join(f.s.remoteFile(f.source, f.repo.Path), "payload")); err != nil || string(content) != "data" {
		t.Fatalf("cleanup purged a location promoted to current: %q %v", content, err)
	}
}

func TestMaintenanceOldCopyPublicationFailureRemainsRetryable(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	f.migrateBeforeCleanup(t)
	if err := os.WriteFile(filepath.Join(f.root, "block.purge"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	req := MigrateRequest{RepositoryID: f.repo.ID, CleanupOld: true, OldLocationIndex: 0, OldLocationPath: f.repo.Path, Confirm: true}
	run := f.start(t, "migrate", req, f.s.migrate)
	f.wait(t, "purge")
	// Changing the pool at this boundary makes only the completion publication
	// fail: the retirement certificate already exists before purge starts.
	f.pool.CatalogReplicas = 3
	if _, err := f.s.rt.Store.Save(t.Context(), "storage_pools", f.pool.ID, -1, f.pool); err != nil {
		t.Fatal(err)
	}
	f.release("purge")
	if err := run.wait(t); err == nil {
		t.Fatal("unavailable completion replicas did not fail publication")
	}
	var repo Repository
	if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || repo.Locations[0].Status != "cleanup_pending" || repo.Locations[0].Verified {
		t.Fatalf("publication failure lost durable retry intent: %+v %v", repo, err)
	}
	if _, err := os.Stat(f.s.remoteFile(f.source, f.repo.Path)); !os.IsNotExist(err) {
		t.Fatalf("fixture never physically purged old copy: %v", err)
	}
	f.pool.CatalogReplicas = 1
	if _, err := f.s.rt.Store.Save(t.Context(), "storage_pools", f.pool.ID, -1, f.pool); err != nil {
		t.Fatal(err)
	}
	retry := f.start(t, "migrate", req, f.s.migrate)
	if err := retry.wait(t); err != nil {
		t.Fatalf("missing old directory could not finish retry: %v", err)
	}
	if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || repo.Locations[0].Status != "deleted" || repo.Locations[0].Verified {
		t.Fatalf("retry did not publish deleted location: %+v %v", repo, err)
	}
}

func TestMaintenanceOldCopyRejectsUnusableCurrentAccountAtHandoff(t *testing.T) {
	for _, state := range []string{"unverified", "error"} {
		t.Run(state, func(t *testing.T) {
			f := newMaintenanceGateFixture(t, "")
			f.migrateBeforeCleanup(t)
			f.s.catalogMu.Lock()
			var once sync.Once
			releaseCatalog := func() { once.Do(f.s.catalogMu.Unlock) }
			defer releaseCatalog()
			run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, CleanupOld: true, OldLocationIndex: 0, OldLocationPath: f.repo.Path, Confirm: true}, f.s.migrate)
			f.waitStage(t, run.id, "committing_old_copy_retirement")
			var raw map[string]any
			if err := f.s.rt.Store.Get(t.Context(), "accounts", f.target.ID, &raw); err != nil {
				t.Fatal(err)
			}
			if state == "unverified" {
				raw["verified"] = false
			} else {
				raw["status"] = "error"
			}
			if _, err := f.s.rt.Store.Save(t.Context(), "accounts", f.target.ID, int(raw["version"].(float64)), raw); err != nil {
				t.Fatal(err)
			}
			releaseCatalog()
			if err := run.wait(t); err == nil {
				t.Fatal("cleanup accepted an unusable current account at retirement handoff")
			}
			if content, err := os.ReadFile(filepath.Join(f.s.remoteFile(f.source, f.repo.Path), "payload")); err != nil || string(content) != "data" {
				t.Fatalf("cleanup deleted old data after current account became unusable: %q %v", content, err)
			}
			var repo Repository
			if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || !repo.Locations[0].Verified || repo.Locations[0].Status == "cleanup_pending" {
				t.Fatalf("rejected handoff retired the old copy: %+v %v", repo, err)
			}
		})
	}
}

func (f *maintenanceGateFixture) retentionPoints(t *testing.T) (Point, Point) {
	t.Helper()
	points := []Point{
		{ID: "old-point", RunID: "old-run", PlanID: f.plan.ID, Status: "completed", FinishedAt: "2026-10-01T00:00:00Z", FileCount: 1, LogicalBytes: 4, Snapshots: []Snapshot{{RepositoryID: f.repo.ID, SnapshotID: strings.Repeat("a", 64)}}},
		{ID: "kept-point", RunID: "kept-run", PlanID: f.plan.ID, Status: "completed", FinishedAt: "2026-10-02T00:00:00Z", FileCount: 1, LogicalBytes: 4, Snapshots: []Snapshot{{RepositoryID: f.repo.ID, SnapshotID: strings.Repeat("b", 64)}}},
	}
	for _, point := range points {
		if _, err := f.s.rt.Store.Save(t.Context(), "recovery_points", point.ID, 0, point); err != nil {
			t.Fatal(err)
		}
		if _, err := f.s.rt.Store.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,repo_id,snapshot_path) VALUES(?,'source','payload','file',4,?,'/payload')", point.RunID, f.repo.ID); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal([]map[string]string{{"id": points[0].Snapshots[0].SnapshotID}, {"id": points[1].Snapshots[0].SnapshotID}})
	if err := os.WriteFile(filepath.Join(f.root, "snapshots.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	return points[0], points[1]
}

func (f *maintenanceGateFixture) latestCatalog(t *testing.T) catalogControl {
	t.Helper()
	id, err := f.s.rt.Store.Meta(t.Context(), "catalog_last")
	if err != nil || id == "" {
		t.Fatalf("missing completed catalog: %q %v", id, err)
	}
	plain := filepath.Join(t.TempDir(), "catalog.zip")
	if err := openCatalog(f.s.rt.Keys, f.s.remoteFile(f.source, "VaultPilot/"+f.s.rt.VaultID()+"/catalog/"+id+".vpc"), plain); err != nil {
		t.Fatal(err)
	}
	archive, err := zip.OpenReader(plain)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	for _, file := range archive.File {
		if file.Name != "control.json" {
			continue
		}
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		var control catalogControl
		if err := json.NewDecoder(reader).Decode(&control); err != nil {
			t.Fatal(err)
		}
		return control
	}
	t.Fatal("catalog has no control document")
	return catalogControl{}
}

func TestMaintenancePruneAllowsConcurrentCommitAndPreservesNewPending(t *testing.T) {
	f := newMaintenanceGateFixture(t, "prune")
	old, kept := f.retentionPoints(t)
	run := f.start(t, "retention", RetentionRequest{PlanID: f.plan.ID, PointIDs: []string{old.ID}, Confirm: true}, f.s.retain)
	f.wait(t, "prune")
	other := f.publishOtherPlan(t)
	newPending := strings.Repeat("c", 64)
	f.s.catalogMu.Lock()
	err := f.s.recordRetentionDeletion(t.Context(), nil, map[string][]string{f.repo.ID: {newPending}})
	f.s.catalogMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	f.release("prune")
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
	var repo Repository
	if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || !slices.Equal(repo.PendingForget, []string{newPending}) {
		t.Fatalf("completion removed another deletion's pending ID: %+v %v", repo, err)
	}
	control := f.latestCatalog(t)
	if !slices.ContainsFunc(control.Points, func(point Point) bool { return point.ID == other.ID && point.DeletedAt == "" }) {
		t.Fatal("completion catalog lost the point committed while prune was running")
	}
	if !slices.ContainsFunc(control.Repositories, func(repo Repository) bool {
		return repo.ID == f.repo.ID && slices.Equal(repo.PendingForget, []string{newPending})
	}) {
		t.Fatal("completion catalog lost the newly appended pending ID")
	}
	if got := cleanupEntryCount(t, f.s, old.RunID); got != 0 {
		t.Fatalf("deleted point retained its file list: %d", got)
	}
	if got := cleanupEntryCount(t, f.s, kept.RunID); got != 1 {
		t.Fatalf("cleanup removed a live point's file list: %d", got)
	}
}

func TestMaintenancePruneFailureKeepsDecisionForRetry(t *testing.T) {
	for _, fault := range []string{"prune", "completion", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			f := newMaintenanceGateFixture(t, "prune")
			old, kept := f.retentionPoints(t)
			run := f.start(t, "retention", RetentionRequest{PlanID: f.plan.ID, PointIDs: []string{old.ID}, Confirm: true}, f.s.retain)
			f.wait(t, "prune")
			switch fault {
			case "prune":
				if err := os.WriteFile(filepath.Join(f.root, "fail.prune"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "completion":
				f.pool.CatalogReplicas = 3
				if _, err := f.s.rt.Store.Save(t.Context(), "storage_pools", f.pool.ID, -1, f.pool); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				run.cancel()
			}
			f.release("prune")
			if err := run.wait(t); err == nil {
				t.Fatal("injected prune/completion failure unexpectedly succeeded")
			}
			var deleted Point
			if err := f.s.rt.Store.Get(t.Context(), "recovery_points", old.ID, &deleted); err != nil || deleted.DeletedAt == "" {
				t.Fatalf("failure lost the durable tombstone: %+v %v", deleted, err)
			}
			var repo Repository
			if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || !slices.Contains(repo.PendingForget, old.Snapshots[0].SnapshotID) {
				t.Fatalf("failure lost pending retry intent: %+v %v", repo, err)
			}
			_ = os.Remove(filepath.Join(f.root, "fail.prune"))
			f.pool.CatalogReplicas = 1
			if _, err := f.s.rt.Store.Save(t.Context(), "storage_pools", f.pool.ID, -1, f.pool); err != nil {
				t.Fatal(err)
			}
			// Forget may already have succeeded before prune/publication failed.
			raw, _ := json.Marshal([]map[string]string{{"id": kept.Snapshots[0].SnapshotID}})
			if err := os.WriteFile(filepath.Join(f.root, "snapshots.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			retry := f.start(t, "retention", RetentionRequest{PlanID: f.plan.ID, Confirm: true}, f.s.retain)
			if err := retry.wait(t); err != nil {
				t.Fatalf("durable pending decision could not retry: %v", err)
			}
			repo = Repository{}
			if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || len(repo.PendingForget) != 0 {
				t.Fatalf("successful retry did not clear processed IDs: %+v %v", repo, err)
			}
			if !slices.ContainsFunc(f.latestCatalog(t).Repositories, func(repo Repository) bool { return repo.ID == f.repo.ID && len(repo.PendingForget) == 0 }) {
				t.Fatal("retry catalog did not converge to completed cleanup")
			}
		})
	}
}

func TestMaintenanceRetentionWaitsForRestoreReaders(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	old, _ := f.retentionPoints(t)
	// D's restore integration test proves the restore lifetime holds this reader.
	f.s.recoveryIndex.RLock()
	var once sync.Once
	releaseReader := func() { once.Do(f.s.recoveryIndex.RUnlock) }
	defer releaseReader()
	run := f.start(t, "retention", RetentionRequest{PlanID: f.plan.ID, PointIDs: []string{old.ID}, Confirm: true}, f.s.retain)
	deadline := time.Now().Add(3 * time.Second)
	for {
		probe, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		err := f.s.recoveryIndex.RLockContext(probe)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			break // The retaining writer has admission and is draining our reader.
		}
		if err != nil {
			t.Fatal(err)
		}
		f.s.recoveryIndex.RUnlock()
		if time.Now().After(deadline) {
			t.Fatal("retention did not wait for existing restore readers")
		}
	}
	var before Point
	if err := f.s.rt.Store.Get(t.Context(), "recovery_points", old.ID, &before); err != nil || before.DeletedAt != "" {
		t.Fatalf("retention deleted a point before its readers drained: %+v %v", before, err)
	}
	releaseReader()
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
	_, _, _, _, _, err := f.s.restoreContext(t.Context(), RestoreRequest{PointID: old.ID})
	var failure *failure
	if !errors.As(err, &failure) || failure.Code != "POINT_UNAVAILABLE" {
		t.Fatalf("new restore accepted a deleted point: %v", err)
	}
}

func TestMaintenanceRetentionDoesNotForgetSharedSnapshot(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	old, _ := f.retentionPoints(t)
	shared := old
	shared.ID, shared.RunID, shared.PlanID = "shared-point", "shared-run", "other-plan"
	if _, err := f.s.rt.Store.Save(t.Context(), "recovery_points", shared.ID, 0, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.rt.Store.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,repo_id,snapshot_path) VALUES(?,'source','payload','file',4,?,'/payload')", shared.RunID, f.repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.s.recordRetentionDeletion(t.Context(), nil, map[string][]string{f.repo.ID: {old.Snapshots[0].SnapshotID}}); err != nil {
		t.Fatal(err)
	}
	run := f.start(t, "retention", RetentionRequest{PlanID: f.plan.ID, PointIDs: []string{old.ID}, Confirm: true}, f.s.retain)
	if err := run.wait(t); err != nil {
		t.Fatal(err)
	}
	forgotten, err := os.ReadFile(filepath.Join(f.root, "forget.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if strings.Contains(string(forgotten), old.Snapshots[0].SnapshotID) {
		t.Fatal("retention forgot a snapshot still referenced by another plan")
	}
	var point Point
	if err := f.s.rt.Store.Get(t.Context(), "recovery_points", shared.ID, &point); err != nil || point.DeletedAt != "" {
		t.Fatalf("retention deleted the other plan's live point: %+v %v", point, err)
	}
	if got := cleanupEntryCount(t, f.s, shared.RunID); got != 1 {
		t.Fatalf("retention removed the shared live point's file list: %d", got)
	}
}

func TestMaintenanceMigrationPublicationFailureRestoresOldMapping(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	f.pool.CatalogReplicas = 3
	if _, err := f.s.rt.Store.Save(t.Context(), "storage_pools", f.pool.ID, -1, f.pool); err != nil {
		t.Fatal(err)
	}
	run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}, f.s.migrate)
	if err := run.wait(t); err == nil {
		t.Fatal("insufficient catalog replicas unexpectedly published migration")
	}
	var repo Repository
	if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || repo.AccountID != f.source.ID || repo.Path != f.repo.Path || len(repo.Locations) != 1 {
		t.Fatalf("failed publication did not restore old mapping: %+v %v", repo, err)
	}
	if content, err := os.ReadFile(filepath.Join(f.s.remoteFile(f.source, f.repo.Path), "payload")); err != nil || string(content) != "data" {
		t.Fatalf("failed publication changed source data: %q %v", content, err)
	}
}

func TestMaintenanceDegradedMigrationKeepsCommittedMapping(t *testing.T) {
	f := newMaintenanceGateFixture(t, "")
	cloud := platform.Account{ID: "cloud", Name: "Metadata replica", Backend: "onedrive", Remote: "cloud", DriveID: "cloud-drive", DriveType: "personal", Region: "global", TenantID: "consumers", Enabled: true, Verified: true}
	version, err := f.s.rt.Store.Save(t.Context(), "accounts", cloud.ID, 0, cloud)
	if err != nil {
		t.Fatal(err)
	}
	token, _ := json.Marshal(platform.OAuthToken{AccessToken: "disposable-metadata-access", RefreshToken: "disposable-metadata-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)})
	raw, _ := json.Marshal(map[string]any{"config": "[cloud]\ntype = onedrive\ntoken = " + string(token) + "\n", "remote": "cloud", "version": version})
	mux := http.NewServeMux()
	f.s.rt.Register(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest("POST", "/api/accounts/"+cloud.ID+"/rclone/import", bytes.NewReader(raw)))
	if response.Code != http.StatusOK {
		t.Fatalf("disposable credential import: %d %s", response.Code, response.Body.String())
	}
	// Import installs encrypted credentials through the actual endpoint. This
	// metadata-only fixture then restores the verified physical Drive identity.
	if _, err := f.s.rt.Store.Save(t.Context(), "accounts", cloud.ID, -1, cloud); err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	mirror := filepath.Join(f.root, "cloud-mirror")
	wrapper := filepath.Join(f.root, "metadata-rclone")
	script := "#!/bin/sh\nset -eu\n[ \"$1\" != --config ] || shift 2\nif [ \"$1\" != copyto ]; then exec " + quote(f.s.rt.Config.RcloneBin) + " \"$@\"; fi\nshift\nfrom=$1\nto=$2\ncase $from in cloud:*) from=" + quote(mirror) + "/${from#cloud:};; esac\ncase $to in cloud:*.ok) exit 73;; cloud:*) to=" + quote(mirror) + "/${to#cloud:};; esac\nmkdir -p \"$(dirname \"$to\")\"\ncp \"$from\" \"$to\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	f.s.rt.Config.RcloneBin = wrapper
	f.pool.AccountIDs, f.pool.CatalogReplicas = []string{f.source.ID, cloud.ID, f.target.ID}, 2
	if _, err := f.s.rt.Store.Save(t.Context(), "storage_pools", f.pool.ID, -1, f.pool); err != nil {
		t.Fatal(err)
	}
	run := f.start(t, "migrate", MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}, f.s.migrate)
	err = run.wait(t)
	var failure *failure
	if !errors.As(err, &failure) || failure.Code != "PROTECTION_DEGRADED" {
		t.Fatalf("expected one surviving completion certificate: %v", err)
	}
	var repo Repository
	if err := f.s.rt.Store.Get(t.Context(), "repositories", f.repo.ID, &repo); err != nil || repo.AccountID != f.target.ID || len(repo.Locations) != 2 {
		t.Fatalf("degraded logical commit was rolled back: %+v %v", repo, err)
	}
	if content, err := os.ReadFile(filepath.Join(f.s.remoteFile(f.source, f.repo.Path), "payload")); err != nil || string(content) != "data" {
		t.Fatalf("degraded migration changed source data: %q %v", content, err)
	}
}
