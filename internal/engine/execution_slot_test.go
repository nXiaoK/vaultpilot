package engine

import (
	"context"
	"testing"
)

func TestExecuteLoadFailureReleasesActivePlan(t *testing.T) {
	for _, kind := range []string{"backup", "account-clear", "discover"} {
		t.Run(kind, func(t *testing.T) {
			s := cancellationService(t)
			plan := "plan"
			if kind != "backup" {
				plan = ""
			}
			task, err := s.enqueue(kind, plan, "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.rt.Store.Delete(context.Background(), "backup_runs", task.ID); err != nil {
				t.Fatal(err)
			}
			s.active["other-plan"] = "other-task"
			s.cancelled[task.ID] = true
			s.execute(task.ID)
			key := plan
			if kind != "backup" {
				key = kind
			}
			if s.active[key] != "" || s.cancelled[task.ID] || s.active["other-plan"] != "other-task" {
				t.Fatalf("incorrect slot cleanup: %+v", s.active)
			}
			delete(s.active, "other-plan")
			next, err := s.enqueue(kind, plan, "", nil)
			if err != nil || next.ID == task.ID || next.ID == "" {
				t.Fatalf("new task could not use released slot: %+v %v", next, err)
			}
			var logs int
			if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM task_logs WHERE task_id=?", task.ID).Scan(&logs); err != nil || logs != 1 {
				t.Fatalf("load failure not logged: %d %v", logs, err)
			}
		})
	}
}

func TestExecuteLoadFailureMarksExistingRowFailed(t *testing.T) {
	s := cancellationService(t)
	task, err := s.enqueue("backup", "plan", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.rt.Store.DB.Exec("UPDATE backup_runs SET data=json_set(data,'$.progress','invalid') WHERE id=?", task.ID); err != nil {
		t.Fatal(err)
	}
	s.execute(task.ID)
	var raw map[string]any
	if err = s.rt.Store.Get(context.Background(), "backup_runs", task.ID, &raw); err != nil || raw["status"] != "failed" || raw["code"] != "TASK_LOAD_FAILED" || raw["plan_id"] != "plan" || raw["finished_at"] == "" {
		t.Fatalf("existing task did not record the load failure: %+v %v", raw, err)
	}
}

func TestExecuteLoadFailurePreservesNewerActiveSlot(t *testing.T) {
	s := cancellationService(t)
	s.active["plan"] = "newer-task"
	s.execute("missing-task")
	if s.active["plan"] != "newer-task" {
		t.Fatal("load failure removed another task's slot")
	}
}
