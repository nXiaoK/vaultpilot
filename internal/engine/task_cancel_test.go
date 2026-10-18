package engine

import (
	"context"
	"path/filepath"
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
