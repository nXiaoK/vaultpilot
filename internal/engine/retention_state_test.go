package engine

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"vaultpilot/internal/store"
)

func TestMaintenancePendingRemovalIsAtomicAndPreservesJSON(t *testing.T) {
	s, _ := allocationFixture(t)
	for _, id := range []string{"first", "second"} {
		raw := map[string]json.RawMessage{"pending_forget": json.RawMessage(`["remove","new"]`), "extra_null": json.RawMessage(`null`), "precise_number": json.RawMessage(`9007199254740993`)}
		if _, err := s.rt.Store.Save(t.Context(), "repositories", id, 0, raw); err != nil {
			t.Fatal(err)
		}
		// Seed an existing JSON integer beyond float64's exact range.
		if _, err := s.rt.Store.DB.Exec("UPDATE repositories SET data=json_set(data,'$.precise_number',9007199254740993) WHERE id=?", id); err != nil {
			t.Fatal(err)
		}
	}
	removals := map[string][]string{"first": {"remove"}, "second": {"remove"}}
	if _, err := s.rt.Store.DB.Exec("CREATE TRIGGER reject_second BEFORE UPDATE ON repositories WHEN OLD.id='second' BEGIN SELECT RAISE(IGNORE); END"); err != nil {
		t.Fatal(err)
	}
	if err := s.removePendingForget(t.Context(), removals); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("批更新行数不足未失败：%v", err)
	}
	for _, id := range []string{"first", "second"} {
		var repository Repository
		if err := s.rt.Store.Get(t.Context(), "repositories", id, &repository); err != nil || repository.Version != 1 || !slices.Equal(repository.PendingForget, []string{"remove", "new"}) {
			t.Fatalf("失败批更新未完整回滚：%+v %v", repository, err)
		}
	}
	if _, err := s.rt.Store.DB.Exec("DROP TRIGGER reject_second"); err != nil {
		t.Fatal(err)
	}
	if err := s.removePendingForget(t.Context(), removals); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"first", "second"} {
		var raw map[string]json.RawMessage
		if err := s.rt.Store.Get(t.Context(), "repositories", id, &raw); err != nil {
			t.Fatal(err)
		}
		if string(raw["extra_null"]) != "null" || string(raw["precise_number"]) != "9007199254740993" || string(raw["version"]) != "2" || string(raw["pending_forget"]) != `["new"]` {
			t.Fatalf("增量移除改变无关JSON或数字：%s", raw)
		}
	}
	if err := s.removePendingForget(t.Context(), map[string][]string{"first": {"new"}, "second": {"new"}}); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := s.rt.Store.Get(t.Context(), "repositories", "first", &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["pending_forget"]; present {
		t.Fatal("空pending改变了原omitempty契约")
	}
}

func TestMaintenanceExecuteCancellationDoesNotWaitForEntryCleanup(t *testing.T) {
	for _, kind := range []string{"retention", "migrate", "restore"} {
		t.Run(kind, func(t *testing.T) {
			f := newMaintenanceGateFixture(t, "")
			old, _ := f.retentionPoints(t)
			if err := f.s.recoveryIndex.RLockContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			defer f.s.recoveryIndex.RUnlock()
			var request any
			switch kind {
			case "retention":
				request = RetentionRequest{PlanID: f.plan.ID, PointIDs: []string{old.ID}, Confirm: true}
			case "migrate":
				request = MigrateRequest{RepositoryID: f.repo.ID, TargetAccountID: f.target.ID, Confirm: true}
			default:
				request = RestoreRequest{PointID: old.ID}
			}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			task := Task{ID: uid(), Kind: kind, PlanID: f.plan.ID, Request: raw, Status: "queued", CancelRequested: kind != "retention"}
			if err = f.s.save(&task); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { f.s.execute(task.ID); close(done) }()
			if kind == "retention" {
				// A queued writer holds the admission gate, so a later reader times out.
				deadline := time.Now().Add(3 * time.Second)
				for {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
					err = f.s.recoveryIndex.RLockContext(ctx)
					cancel()
					if err != nil {
						break
					}
					f.s.recoveryIndex.RUnlock()
					if time.Now().After(deadline) {
						t.Fatal("清理未等待现有恢复读者")
					}
					time.Sleep(time.Millisecond)
				}
				if _, err = f.s.cancelTask(t.Context(), task); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("无自有文件清单的取消任务仍等待索引写锁清扫")
			}
			var saved Task
			if err = f.s.rt.Store.Get(t.Context(), "backup_runs", task.ID, &saved); err != nil || saved.Status != "cancelled" {
				t.Fatalf("取消未落终态：%+v %v", saved, err)
			}
			if kind == "retention" {
				var unchanged Point
				if err = f.s.rt.Store.Get(t.Context(), "recovery_points", old.ID, &unchanged); err != nil || unchanged.DeletedAt != "" {
					t.Fatalf("取消的删除决策仍修改了恢复点：%+v %v", unchanged, err)
				}
			}
		})
	}
}
