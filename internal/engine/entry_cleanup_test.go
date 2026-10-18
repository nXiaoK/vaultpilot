package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func insertCleanupEntries(t *testing.T, s *Service, run string, count int) {
	t.Helper()
	tx, err := s.rt.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare("INSERT INTO file_entries(run_id,source_id,path,kind) VALUES(?,'source',?,'file')")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	for i := 0; i < count; i++ {
		if _, err = stmt.Exec(run, fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func cleanupEntryCount(t *testing.T, s *Service, run string) int {
	t.Helper()
	var count int
	if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries WHERE run_id=?", run).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestFailedRunEntriesAreRemoved(t *testing.T) {
	s := cancellationService(t)
	for _, task := range []Task{
		{ID: "failed", Kind: "backup", Status: "failed"},
		{ID: "cancelled", Kind: "backup", Status: "cancelled"},
		{ID: "preview", Kind: "preview", Status: "completed"},
		{ID: "successful", Kind: "backup", Status: "completed"},
		{ID: "degraded", Kind: "backup", Status: "partial_failed"},
	} {
		if err := s.save(&task); err != nil {
			t.Fatal(err)
		}
		insertCleanupEntries(t, s, task.ID, 1)
	}
	for _, run := range []string{"successful", "degraded"} {
		if _, err := s.rt.Store.Save(t.Context(), "recovery_points", run, 0, Point{ID: run, RunID: run, Status: "completed"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, task := range []Task{{ID: "failed", Status: "failed"}, {ID: "cancelled", Status: "cancelled"}, {ID: "preview", Kind: "preview", Status: "completed"}, {ID: "degraded", Status: "partial_failed"}} {
		s.cleanupTaskEntries(&task)
	}
	for run, want := range map[string]int{"failed": 0, "cancelled": 0, "preview": 0, "successful": 1, "degraded": 1} {
		if got := cleanupEntryCount(t, s, run); got != want {
			t.Fatalf("run=%s count=%d want=%d", run, got, want)
		}
	}
}

func TestStartupSweepPreservesLiveImportedAndActiveEntries(t *testing.T) {
	s := cancellationService(t)
	for _, task := range []Task{{ID: "running", Status: "running"}, {ID: "queued", Status: "queued"}, {ID: "failed", Status: "failed"}, {ID: "completed-orphan", Status: "completed"}} {
		if err := s.save(&task); err != nil {
			t.Fatal(err)
		}
	}
	for _, point := range []Point{{ID: "live", RunID: "live-run", Status: "completed"}, {ID: "imported", RunID: "import-example", Status: "completed"}, {ID: "deleted", RunID: "deleted-run", DeletedAt: now(), Status: "deleted"}, {ID: "deleted-shared", RunID: "live-run", DeletedAt: now(), Status: "deleted"}} {
		if _, err := s.rt.Store.Save(t.Context(), "recovery_points", point.ID, 0, point); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{"running", "queued", "failed", "completed-orphan", "live-run", "import-example", "deleted-run", "unknown"} {
		insertCleanupEntries(t, s, run, 1)
	}
	insertCleanupEntries(t, s, "large-orphan", 10001)
	if _, err := s.rt.Store.DB.Exec("CREATE TABLE engine_snapshot_entries_v2(task_id TEXT,repo_id TEXT,path TEXT,PRIMARY KEY(task_id,repo_id,path)); INSERT INTO engine_snapshot_entries_v2 VALUES('running','repo','active'),('failed','repo','stale'),('unknown','repo','stale')"); err != nil {
		t.Fatal(err)
	}
	if err := s.cleanupOrphanEntries(t.Context()); err != nil {
		t.Fatal(err)
	}
	for run, want := range map[string]int{"running": 1, "queued": 1, "live-run": 1, "import-example": 1, "failed": 0, "completed-orphan": 0, "deleted-run": 0, "unknown": 0, "large-orphan": 0} {
		if got := cleanupEntryCount(t, s, run); got != want {
			t.Fatalf("run=%s count=%d want=%d", run, got, want)
		}
	}
	var kept int
	if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM engine_snapshot_entries_v2 WHERE task_id='running'").Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("活动校验被删除：%d %v", kept, err)
	}
	var stale int
	if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM engine_snapshot_entries_v2 WHERE task_id<>'running'").Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("旧校验未删除：%d %v", stale, err)
	}
}

func TestStartSweepsInterruptedEntriesAsynchronously(t *testing.T) {
	original := cancellationService(t)
	task := Task{ID: "restart-run", Status: "running"}
	if err := original.save(&task); err != nil {
		t.Fatal(err)
	}
	insertCleanupEntries(t, original, task.ID, 1)
	s, err := New(original.rt)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.Start(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for cleanupEntryCount(t, s, task.ID) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("启动清扫未删除中断运行")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var interrupted Task
	if err = s.rt.Store.Get(t.Context(), "backup_runs", task.ID, &interrupted); err != nil || interrupted.Status != "interrupted" {
		t.Fatalf("重启状态错误：%+v %v", interrupted, err)
	}
}

func TestCleanupWaitsForImportedIndexPublication(t *testing.T) {
	s := cancellationService(t)
	unlock := s.lock("recovery-index")
	insertCleanupEntries(t, s, "import-publishing", 1)
	done := make(chan error, 1)
	go func() { done <- s.cleanupOrphanEntries(context.Background()) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("导入发布锁尚未释放即完成清扫：%v", err)
	case <-time.After(20 * time.Millisecond):
	}
	_, err := s.rt.Store.Save(t.Context(), "recovery_points", "published", 0, Point{ID: "published", RunID: "import-publishing", Status: "completed"})
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if got := cleanupEntryCount(t, s, "import-publishing"); got != 1 {
		t.Fatalf("未保留新发布的导入清单：%d", got)
	}
}

func TestDeletedPointCleanupPreservesOtherLiveReferences(t *testing.T) {
	s := cancellationService(t)
	for _, p := range []Point{{ID: "deleted", PlanID: "plan", RunID: "deleted-run", DeletedAt: now()}, {ID: "deleted-alias", PlanID: "plan", RunID: "shared-run", DeletedAt: now()}, {ID: "live-alias", PlanID: "other", RunID: "shared-run"}, {ID: "other-deleted", PlanID: "other", RunID: "other-run", DeletedAt: now()}} {
		if _, err := s.rt.Store.Save(t.Context(), "recovery_points", p.ID, 0, p); err != nil {
			t.Fatal(err)
		}
	}
	for _, run := range []string{"deleted-run", "shared-run", "other-run"} {
		insertCleanupEntries(t, s, run, 1)
	}
	s.cleanupDeletedPointEntries(&Task{ID: "retention"}, "plan")
	for run, want := range map[string]int{"deleted-run": 0, "shared-run": 1, "other-run": 1} {
		if got := cleanupEntryCount(t, s, run); got != want {
			t.Fatalf("run=%s count=%d want=%d", run, got, want)
		}
	}
}

func TestSnapshotVerificationCleansUpOnEarlyReturn(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%t", cancelled), func(t *testing.T) {
			s, _ := allocationFixture(t)
			root := t.TempDir()
			program := filepath.Join(root, "failed-restic")
			script := "#!/bin/sh\ni=0\nwhile [ \"$i\" -lt 513 ]; do\n printf '{\"struct_type\":\"node\",\"path\":\"/fault-%s\",\"type\":\"file\"}\\n' \"$i\"\n i=$((i+1))\ndone\n"
			if cancelled {
				script += "sleep 30\n"
			} else {
				script += "exit 73\n"
			}
			if err := os.WriteFile(program, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			s.rt.Config.CacheDir = root
			s.rt.Config.ResticBin = program
			password, err := s.rt.Keys.Seal([]byte("disposable-fault-test"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- s.verifySnapshot(ctx, &Task{ID: "verify"}, Repository{ID: "repo", Password: base64.StdEncoding.EncodeToString(password)}, Account{Backend: "local"}, "snapshot", root)
			}()
			if cancelled {
				deadline := time.Now().Add(5 * time.Second)
				for {
					var count int
					err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM engine_snapshot_entries_v2 WHERE task_id='verify'").Scan(&count)
					if err == nil && count >= 512 {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("故障引擎没有写入完整校验页")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			if err = <-done; err == nil {
				t.Fatal("故障引擎未返回错误")
			}
			var remaining int
			if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM engine_snapshot_entries_v2 WHERE task_id='verify'").Scan(&remaining); err != nil || remaining != 0 {
				t.Fatalf("提前返回残留校验清单：%d %v", remaining, err)
			}
		})
	}
}
