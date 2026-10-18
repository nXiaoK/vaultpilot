package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

func allocationFixture(t *testing.T) (*Service, Account) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "allocation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	keys := &platform.Keyring{}
	if err = keys.SetMaster(make([]byte, 32)); err != nil {
		t.Fatal(err)
	}
	a := Account{ID: "account", Free: 1 << 30, Budget: 1 << 30, Enabled: true, Verified: true}
	if _, err = db.Save(context.Background(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	return &Service{rt: &platform.Runtime{Store: db, Keys: keys}}, a
}

func TestAllocationPreservesAssignmentsAcrossPages(t *testing.T) {
	s, a := allocationFixture(t)
	ctx := context.Background()
	repo := Repository{ID: "stable", PlanID: "plan", AccountID: a.ID, Status: "ready"}
	if _, err := s.rt.Store.Save(ctx, "repositories", repo.ID, 0, repo); err != nil {
		t.Fatal(err)
	}
	tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 1025; i++ {
		path := fmt.Sprintf("%04d", i)
		if _, err = tx.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,size,mtime_ns) VALUES('run','source',?,'file',123,45)", path); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("INSERT INTO assignments(plan_id,source_id,path,repo_id,size,mtime_ns) VALUES('plan','source',?,'stable',123,45)", path); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	planning, _, err := s.allocate(ctx, &Task{ID: "run", Kind: "backup"}, Plan{ID: "plan"}, Pool{AccountIDs: []string{a.ID}}, map[string]Account{a.ID: a})
	if err != nil {
		t.Fatal(err)
	}
	if planning.FileCount != 1025 || len(planning.Shards) != 1 || planning.Shards[0].EstimatedGrowth != 1025*1024 {
		t.Fatalf("分页规划失准：%+v", planning)
	}
	var assigned int
	if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries WHERE run_id='run' AND repo_id='stable'").Scan(&assigned); err != nil || assigned != 1025 {
		t.Fatalf("归属遗漏：%d %v", assigned, err)
	}
}

func TestEntryPageRollsBackOnWriteFailure(t *testing.T) {
	s, _ := allocationFixture(t)
	ctx := context.Background()
	for _, path := range []string{"a", "b"} {
		if _, err := s.rt.Store.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind) VALUES('run','source',?,'file')", path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.rt.Store.DB.Exec("CREATE TRIGGER reject_second BEFORE UPDATE ON file_entries WHEN NEW.path='b' BEGIN SELECT RAISE(ABORT,'failed page'); END"); err != nil {
		t.Fatal(err)
	}
	err := s.writeEntryPage(ctx, "UPDATE file_entries SET repo_id='assigned' WHERE run_id='run' AND path=?", []Entry{{Path: "a"}, {Path: "b"}}, func(e Entry) []any { return []any{e.Path} })
	if err == nil {
		t.Fatal("页写入失败未返回错误")
	}
	var count int
	if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM file_entries WHERE repo_id<>''").Scan(&count); err != nil || count != 0 {
		t.Fatalf("失败页未回滚：%d %v", count, err)
	}
}

func TestReservationRechecksCurrentAccountAndPlanningSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name          string
		change        func(*Account)
		planningSmall bool
	}{
		{name: "quota", change: func(a *Account) { a.Free = 9 }},
		{name: "budget", change: func(a *Account) { a.Budget = 9 }},
		{name: "completed task usage", change: func(a *Account) { a.ProjectUsed = a.Budget - 9 }},
		{name: "disabled", change: func(a *Account) { a.Enabled = false }},
		{name: "unverified", change: func(a *Account) { a.Verified = false }},
		{name: "planning snapshot", change: func(a *Account) {}, planningSmall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, a := allocationFixture(t)
			snapshot := a
			if tc.planningSmall {
				snapshot.Free = 9
			}
			tc.change(&a)
			if _, err := s.rt.Store.Save(context.Background(), "accounts", a.ID, -1, a); err != nil {
				t.Fatal(err)
			}
			err := s.reserveCapacity(context.Background(), &Task{ID: "run", Kind: "backup"}, map[string]Account{a.ID: snapshot}, map[string]int64{a.ID: 10})
			if err == nil {
				t.Fatal("使用了规划后失效的账号容量")
			}
			var count int
			if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM capacity_reservations").Scan(&count); err != nil || count != 0 {
				t.Fatalf("失败后仍预留容量：%d %v", count, err)
			}
		})
	}
}

func TestConcurrentReservationsDoNotOversell(t *testing.T) {
	s, a := allocationFixture(t)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, run := range []string{"first", "second"} {
		wg.Go(func() {
			<-start
			results <- s.reserveCapacity(context.Background(), &Task{ID: run, Kind: "backup"}, map[string]Account{a.ID: a}, map[string]int64{a.ID: 700 << 20})
		})
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("并发预留成功数=%d，预期仅一次", successes)
	}
	var reserved int64
	if err := s.rt.Store.DB.QueryRow("SELECT sum(bytes) FROM capacity_reservations").Scan(&reserved); err != nil || reserved != 700<<20 {
		t.Fatalf("容量超售：%d %v", reserved, err)
	}
}

func TestReservationValidatesWholeAccountBatchBeforeWriting(t *testing.T) {
	s, a := allocationFixture(t)
	ctx := context.Background()
	b := a
	b.ID = "second"
	b.Enabled = false
	if _, err := s.rt.Store.Save(ctx, "accounts", b.ID, 0, b); err != nil {
		t.Fatal(err)
	}
	if err := s.reserveCapacity(ctx, &Task{ID: "run", Kind: "backup"}, map[string]Account{a.ID: a, b.ID: a}, map[string]int64{a.ID: 10, b.ID: 10}); err == nil {
		t.Fatal("接受了失效账号")
	}
	var count int
	if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM capacity_reservations").Scan(&count); err != nil || count != 0 {
		t.Fatalf("账号批校验失败仍写入预留：%d %v", count, err)
	}
	if err := s.reserveCapacity(ctx, &Task{ID: "empty"}, nil, nil); err != nil {
		t.Fatalf("空集合错误：%v", err)
	}
	if err := s.reserveCapacity(ctx, &Task{ID: "missing"}, map[string]Account{a.ID: a}, map[string]int64{"missing": 10}); err == nil {
		t.Fatal("接受了已删除账号")
	}
}
