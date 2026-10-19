package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// wrapResticStep runs the real engine and only adds a shell action before the
// named subcommand, e.g. an application rotating its own files mid-run.
func wrapResticStep(t *testing.T, s *Service, step, action string) {
	t.Helper()
	wrapper := filepath.Join(t.TempDir(), "restic-"+step)
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = " + step + " ]; then\n" + action + "\n  fi\ndone\nexec " + shellQuote(s.rt.Config.ResticBin) + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s.rt.Config.ResticBin = wrapper
}

func TestRealResticBackupLeavesOutFilesDeletedAfterScan(t *testing.T) {
	cases := []struct {
		name, step string
		action     func(binary, target string) string
	}{
		// Repository initialization runs after the scan and before the shard precheck.
		{"before shard precheck", "init", func(_, target string) string { return "rm -f " + shellQuote(target) }},
		// Restic skips a listed file that no longer exists and exits 0.
		{"before engine reads", "backup", func(_, target string) string { return "rm -f " + shellQuote(target) }},
		// Restic cannot read the file, exits 3, and the file is then deleted.
		{"while engine reads", "backup", func(binary, target string) string {
			return "chmod 000 " + shellQuote(target) + "\n" + shellQuote(binary) + " \"$@\"\ncode=$?\nrm -f " + shellQuote(target) + "\nexit $code"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.step == "backup" && strings.Contains(tc.name, "while") && os.Geteuid() == 0 {
				t.Skip("root can read mode 0000 files")
			}
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
			rotated := filepath.Join(source.Path, "database", "db_halo_20261017083000.sql.gz")
			if err := os.MkdirAll(filepath.Dir(rotated), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(rotated, []byte("old database dump"), 0600); err != nil {
				t.Fatal(err)
			}
			canonical, err := filepath.EvalSymlinks(rotated)
			if err != nil {
				t.Fatal(err)
			}
			binary := s.rt.Config.ResticBin
			wrapResticStep(t, s, tc.step, tc.action(binary, canonical))
			task := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
			if err := s.backup(ctx, &task); err != nil {
				t.Fatalf("backup failed after a source file was deleted: %v", err)
			}
			if _, err := os.Lstat(rotated); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("wrapper did not delete the file: %v", err)
			}
			result := task.Result.(map[string]any)
			var point Point
			if err := s.rt.Store.Get(ctx, "recovery_points", result["point_id"].(string), &point); err != nil {
				t.Fatal(err)
			}
			var count, size, kept int64
			if err := s.rt.Store.DB.QueryRow("SELECT count(*),coalesce(sum(size),0) FROM file_entries WHERE run_id=?", point.RunID).Scan(&count, &size); err != nil {
				t.Fatal(err)
			}
			if count != point.FileCount || size != point.LogicalBytes {
				t.Fatalf("point totals do not match its manifest: point=%d/%d manifest=%d/%d", point.FileCount, point.LogicalBytes, count, size)
			}
			if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries WHERE run_id=? AND path=?", point.RunID, "database/db_halo_20261017083000.sql.gz").Scan(&kept); err != nil || kept != 0 {
				t.Fatalf("deleted file stayed in the manifest: %d %v", kept, err)
			}
			if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries WHERE run_id=? AND path='file.txt'", point.RunID).Scan(&kept); err != nil || kept != 1 {
				t.Fatalf("remaining file missing from the manifest: %d %v", kept, err)
			}
			warnings := result["planning"].(Planning).Warnings
			if len(warnings) != 1 || !strings.Contains(warnings[0], "1 个条目") {
				t.Fatalf("missing deletion warning: %#v", warnings)
			}
			// Discovery checks the published manifest against the point totals.
			s.rt.Config.ResticBin = binary
			if err := s.discover(ctx, &Task{ID: uid(), Kind: "discover"}); err != nil {
				t.Fatalf("catalog does not match the published point: %v", err)
			}
		})
	}
}

func TestRealResticUnreadableSourceFileStillFailsBackup(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode 0000 files")
	}
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
	target, err := filepath.EvalSymlinks(filepath.Join(source.Path, "file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0600) })
	wrapResticStep(t, s, "backup", "chmod 000 "+shellQuote(target))
	task := Task{ID: uid(), Kind: "backup", PlanID: plan.ID, StartedAt: now()}
	err = s.backup(ctx, &task)
	var f *failure
	if !errors.As(err, &f) || f.Code != "SOURCE_READ_FAILED" {
		t.Fatalf("an existing unreadable file must still fail the backup: %v", err)
	}
	var points int
	_ = s.rt.Store.DB.QueryRow("SELECT count(*) FROM recovery_points").Scan(&points)
	if points != 0 {
		t.Fatal("incomplete recovery point was published")
	}
}

func TestRealResticShardPrecheckRejectsUnsafeSourceChanges(t *testing.T) {
	for _, name := range []string{"mount changed", "type changed"} {
		t.Run(name, func(t *testing.T) {
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
			if err != nil || len(allocation.Shards) != 1 {
				t.Fatalf("allocation: %+v %v", allocation, err)
			}
			source := sources[plan.SourceIDs[0]]
			path := filepath.Join(source.Path, "file.txt")
			if err = os.Remove(path); err != nil {
				t.Fatal(err)
			}
			want := "SOURCE_CHANGED"
			if name == "mount changed" {
				// An unmounted volume also makes files disappear; it must not
				// publish an emptied point.
				source.MountIdentity = "different-mount"
				sources[source.ID] = source
				want = "SOURCE_MOUNT_CHANGED"
			} else if err = os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			repo := repos[allocation.Shards[0].RepositoryID]
			_, err = s.backupShard(ctx, &task, plan, repo, account, sources, t.TempDir())
			var f *failure
			if !errors.As(err, &f) || f.Code != want {
				t.Fatalf("expected %s, got %v", want, err)
			}
			var entries int
			_ = s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries WHERE run_id=? AND path='file.txt'", task.ID).Scan(&entries)
			if entries != 1 {
				t.Fatal("rejected change must not remove planned entries")
			}
		})
	}
}
