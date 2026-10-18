package engine

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

func cancellationService(t *testing.T) *Service {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "tasks.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(&platform.Runtime{Store: db})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(); db.Close() })
	return s
}

func TestCancelDoesNotOverwriteTerminalStatus(t *testing.T) {
	s := cancellationService(t)
	for _, status := range terminalStatuses {
		t.Run(status, func(t *testing.T) {
			stale := Task{ID: uid(), Kind: "backup", Status: "running"}
			if err := s.save(&stale); err != nil {
				t.Fatal(err)
			}
			completed := stale
			completed.Status = status
			completed.FinishedAt = now()
			completed.Result = map[string]any{"kept": true}
			if err := s.save(&completed); err != nil {
				t.Fatal(err)
			}
			// The handler read the running snapshot before the executor finished.
			current, err := s.cancelTask(context.Background(), stale)
			result, _ := current.Result.(map[string]any)
			if err != nil || current.Status != status || current.Version != completed.Version || current.CancelRequested || current.FinishedAt != completed.FinishedAt || result["kept"] != true {
				t.Fatalf("terminal task overwritten: %+v, %v", current, err)
			}
		})
	}
}

func TestCancelFlagSurvivesExecutorSave(t *testing.T) {
	s := cancellationService(t)
	for _, kind := range []string{"backup", "restore", "retention"} {
		t.Run(kind, func(t *testing.T) {
			task := Task{ID: uid(), Kind: kind, Status: "running"}
			if err := s.save(&task); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.mu.Lock()
			s.cancels[task.ID] = cancel
			s.mu.Unlock()
			current, err := s.cancelTask(context.Background(), task)
			if err != nil || !current.CancelRequested || current.Version <= task.Version || ctx.Err() == nil {
				t.Fatalf("cancel was not recorded: %+v %v", current, err)
			}
			task.Stage = "uploading"
			if err := s.save(&task); err != nil {
				t.Fatal(err)
			}
			tables := []string{"backup_runs"}
			if kind == "restore" {
				tables = append(tables, "restore_jobs")
			} else if kind == "retention" {
				tables = append(tables, "maintenance_jobs")
			}
			for _, table := range tables {
				if err := s.rt.Store.Get(context.Background(), table, task.ID, &current); err != nil || !current.CancelRequested || current.Stage != "uploading" {
					t.Fatalf("cancel marker lost in %s: %+v %v", table, current, err)
				}
			}
		})
	}
}

func TestCancelOldTaskPreservesNewTaskPending(t *testing.T) {
	for _, status := range terminalStatuses {
		t.Run(status, func(t *testing.T) {
			s := cancellationService(t)
			stale := Task{ID: uid(), Kind: "backup", PlanID: "plan", Status: "running"}
			if err := s.save(&stale); err != nil {
				t.Fatal(err)
			}
			completed := stale
			completed.Status = status
			completed.FinishedAt = now()
			completed.Result = map[string]any{"kept": true}
			if err := s.save(&completed); err != nil {
				t.Fatal(err)
			}
			next, err := s.enqueue("backup", stale.PlanID, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			next.Status = "running"
			if err := s.save(&next); err != nil {
				t.Fatal(err)
			}
			merged, err := s.enqueue("backup", next.PlanID, "", nil)
			if err != nil || merged.ID != next.ID {
				t.Fatalf("new task trigger did not merge: %+v %v", merged, err)
			}
			// The cancel handler captured A before A finished and B became active.
			current, err := s.cancelTask(context.Background(), stale)
			if err != nil || !reflect.DeepEqual(completed, current) {
				t.Fatalf("old terminal task changed: %+v %v", current, err)
			}
			s.mu.Lock()
			active, pending := s.active[next.PlanID], s.pending[next.PlanID]
			s.mu.Unlock()
			persisted, err := s.rt.Store.Meta(t.Context(), "pending:"+next.PlanID)
			if active != next.ID || !pending || err != nil || persisted != "1" {
				t.Fatalf("old cancellation removed new task state: active=%q pending=%v persisted=%q err=%v", active, pending, persisted, err)
			}
			var savedNext Task
			if err := s.rt.Store.Get(t.Context(), "backup_runs", next.ID, &savedNext); err != nil || !reflect.DeepEqual(next, savedNext) {
				t.Fatalf("new task changed: %+v %v", savedNext, err)
			}
			// Cancelling the current task still discards its merged trigger.
			if _, err := s.cancelTask(t.Context(), next); err != nil {
				t.Fatal(err)
			}
			s.mu.Lock()
			active, pending = s.active[next.PlanID], s.pending[next.PlanID]
			s.mu.Unlock()
			persisted, err = s.rt.Store.Meta(t.Context(), "pending:"+next.PlanID)
			if active != next.ID || pending || err != nil || persisted != "0" {
				t.Fatalf("current cancellation did not clear pending: active=%q pending=%v persisted=%q err=%v", active, pending, persisted, err)
			}
		})
	}
}
