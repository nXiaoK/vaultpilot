package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

type concurrentQuotaStore struct {
	*store.Store
	saves          int
	alwaysConflict bool
}

func (db *concurrentQuotaStore) Save(ctx context.Context, table, id string, expected int, value any) (int, error) {
	db.saves++
	if db.alwaysConflict {
		return 0, store.ErrConflict
	}
	if db.saves == 1 {
		var edited map[string]any
		if err := db.Store.Get(ctx, table, id, &edited); err != nil {
			return 0, err
		}
		edited["name"] = "用户编辑后的账号"
		edited["budget_bytes"] = 2048
		edited["verified"] = false
		edited["remote"] = "vp_reauthorized"
		if _, err := db.Store.Save(ctx, table, id, expected, edited); err != nil {
			return 0, err
		}
	}
	return db.Store.Save(ctx, table, id, expected, value)
}

func TestRefreshAccountDoesNotClobberConcurrentEdit(t *testing.T) {
	s := catalogRefreshNode(t)
	ctx := context.Background()
	a := Account{ID: "account", Name: "旧账号名", Backend: "local", LocalPath: t.TempDir(), Verified: true, Enabled: true, Budget: 1024}
	if _, err := s.rt.Store.Save(ctx, "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	a.Free, a.ProjectUsed, a.QuotaAt = 700, 23, now()
	db := &concurrentQuotaStore{Store: s.rt.Store}
	if err := saveAccountQuota(ctx, db, a, 1000, 300); err != nil {
		t.Fatal(err)
	}
	var current platform.Account
	if err := s.rt.Store.Get(ctx, "accounts", a.ID, &current); err != nil {
		t.Fatal(err)
	}
	if current.Name != "用户编辑后的账号" || current.BudgetBytes != 2048 || current.Verified || current.Remote != "vp_reauthorized" {
		t.Fatalf("配额刷新覆盖了并发编辑：%+v", current)
	}
	if current.FreeBytes != 700 || current.TotalBytes != 1000 || current.UsedBytes != 300 || current.ProjectUsedBytes != 23 || current.QuotaAt != a.QuotaAt {
		t.Fatalf("配额未完整保存：%+v", current)
	}
	if db.saves != 2 || current.Version != 3 {
		t.Fatalf("未通过版本冲突重试：saves=%d version=%d", db.saves, current.Version)
	}
}

func TestAccountQuotaConflictsAreBounded(t *testing.T) {
	s := catalogRefreshNode(t)
	a := Account{ID: "account", Backend: "local"}
	if _, err := s.rt.Store.Save(context.Background(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	db := &concurrentQuotaStore{Store: s.rt.Store, alwaysConflict: true}
	if err := saveAccountQuota(context.Background(), db, a, 10, 3); !errors.Is(err, store.ErrConflict) || db.saves != 3 {
		t.Fatalf("冲突重试未限制为三次：saves=%d err=%v", db.saves, err)
	}
}

func TestPostTaskQuotaRefreshStopsOnShutdown(t *testing.T) {
	s := catalogRefreshNode(t)
	a := Account{ID: "account", Backend: "local", Verified: true, LocalPath: t.TempDir()}
	if _, err := s.rt.Store.Save(context.Background(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(t.TempDir(), "rclone")
	if err := os.WriteFile(program, []byte("#!/bin/sh\n: > \"$0.started\"\nsleep 30\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s.rt.Config.RcloneBin = program
	// Use a local transport in the task while retaining a credential-free local
	// account in the store, so only the subprocess cancellation is under test.
	a.Backend, a.Remote = "onedrive", ":local"
	done := make(chan error, 1)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		done <- s.refreshAccountAfterTask(&Task{ID: "quota-refresh"}, &a)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(program + ".started"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("未启动收尾刷新")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closed := make(chan struct{})
	go func() {
		s.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("收尾刷新阻止了服务关闭")
	}
	if err := <-done; err == nil {
		t.Fatal("关闭时刷新仍然成功")
	}
	var logs int
	if err := s.rt.Store.DB.QueryRow("SELECT count(*) FROM task_logs WHERE task_id=?", "quota-refresh").Scan(&logs); err != nil || logs != 1 {
		t.Fatalf("收尾刷新失败未记日志：logs=%d err=%v", logs, err)
	}
	paths, err := filepath.Glob(filepath.Join(s.rt.Config.CacheDir, "rclone-*.conf"))
	if err != nil || len(paths) != 0 {
		t.Fatalf("关闭后配置未释放：%v %v", paths, err)
	}
}

func TestRefreshAccountCancelledLocalDoesNotWrite(t *testing.T) {
	s := catalogRefreshNode(t)
	a := Account{ID: "account", Backend: "local", LocalPath: t.TempDir(), Free: 11}
	if _, err := s.rt.Store.Save(context.Background(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.refreshAccount(ctx, &a); !errors.Is(err, context.Canceled) {
		t.Fatalf("本地刷新未响应取消：%v", err)
	}
	var current platform.Account
	if err := s.rt.Store.Get(context.Background(), "accounts", a.ID, &current); err != nil || current.Version != 1 || current.FreeBytes != 11 {
		t.Fatalf("取消刷新修改了配额：%+v %v", current, err)
	}
}
