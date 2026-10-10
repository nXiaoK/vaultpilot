package engine

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestRealResticRetryAfterRepositoryInitFailure(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		name := "uninitialized"
		if initialized {
			name = "already-initialized"
		}
		t.Run(name, func(t *testing.T) {
			s, plan, pool, account := realFixture(t)
			ctx := context.Background()
			pool.CatalogReplicas = 1
			if _, err := s.rt.Store.Save(ctx, "storage_pools", pool.ID, -1, pool); err != nil {
				t.Fatal(err)
			}
			restic := s.rt.Config.ResticBin
			s.rt.Config.ResticBin = filepath.Join(t.TempDir(), "missing-restic")
			first, err := s.enqueue("backup", plan.ID, "first-attempt", nil)
			if err != nil {
				t.Fatal(err)
			}
			s.execute(first.ID)
			if err := s.rt.Store.Get(ctx, "backup_runs", first.ID, &first); err != nil {
				t.Fatal(err)
			}
			if first.Status != "failed" || first.Code != "REPOSITORY_INIT_FAILED" {
				t.Fatalf("expected initialization failure: status=%s code=%s error=%s", first.Status, first.Code, first.Error)
			}
			rows, err := s.rt.Store.All(ctx, "repositories")
			if err != nil || len(rows) != 1 {
				t.Fatalf("expected one persisted pending repository: count=%d err=%v", len(rows), err)
			}
			var original Repository
			if err := json.Unmarshal(rows[0], &original); err != nil {
				t.Fatal(err)
			}
			if original.Status != "new" || original.Version < 1 {
				t.Fatalf("unexpected pending repository: status=%s version=%d", original.Status, original.Version)
			}
			s.rt.Config.ResticBin = restic
			if initialized {
				// Simulate a previous run that created the cloud repository before failing.
				if output, err := s.restic(ctx, &first, original, account, []string{"init"}, nil); err != nil {
					t.Fatalf("prepare partially initialized repository: %v\n%s", err, output)
				}
			}
			for _, key := range []string{"retry", "next-backup"} {
				task, err := s.enqueue("backup", plan.ID, key, nil)
				if err != nil {
					t.Fatal(err)
				}
				s.execute(task.ID)
				if err := s.rt.Store.Get(ctx, "backup_runs", task.ID, &task); err != nil {
					t.Fatal(err)
				}
				if task.Status != "completed" {
					t.Fatalf("%s failed: status=%s code=%s error=%s", key, task.Status, task.Code, task.Error)
				}
			}
			rows, err = s.rt.Store.All(ctx, "repositories")
			if err != nil || len(rows) != 1 {
				t.Fatalf("retry created extra repositories: count=%d err=%v", len(rows), err)
			}
			var final Repository
			if err := json.Unmarshal(rows[0], &final); err != nil {
				t.Fatal(err)
			}
			if final.ID != original.ID || final.Password != original.Password || final.Path != original.Path || final.Status != "ready" {
				t.Fatal("retry must reuse the repository ID, path and password, then mark it ready")
			}
			points, err := s.rt.Store.All(ctx, "recovery_points")
			if err != nil || len(points) != 2 {
				t.Fatalf("successful retries must publish recovery points: count=%d err=%v", len(points), err)
			}
		})
	}
}
