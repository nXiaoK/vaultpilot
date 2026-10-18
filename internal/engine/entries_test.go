package engine

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

func TestEachEntryPreservesEmptyKeysAndMutableFilters(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "entries.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{rt: &platform.Runtime{Store: db}}
	keys := []entryCursor{{}, {SourceID: "source"}, {SourceID: "source", Path: "a"}, {SourceID: "source", Path: "目录"}, {SourceID: "source-two"}}
	for _, key := range keys {
		if _, err = db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind,repo_id) VALUES('run',?,?,'dir','old')", key.SourceID, key.Path); err != nil {
			t.Fatal(err)
		}
	}
	got := []entryCursor{}
	err = s.eachEntry(context.Background(), "run_id=? AND repo_id=?", []any{"run", "old"}, 1, func(entries []Entry) error {
		for _, e := range entries {
			got = append(got, entryCursor{SourceID: e.SourceID, Path: e.Path})
			if _, err := db.DB.Exec("UPDATE file_entries SET repo_id='new' WHERE run_id='run' AND source_id=? AND path=?", e.SourceID, e.Path); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil || !reflect.DeepEqual(got, keys) {
		t.Fatalf("键集遍历遗漏、重复或失序：got=%+v want=%+v err=%v", got, keys, err)
	}
	page, err := s.entries(context.Background(), "run_id=?", []any{"run"}, 2, 1)
	if err != nil || len(page) != 2 || page[0].SourceID != "source" || page[0].Path != "" || page[1].Path != "a" {
		t.Fatalf("HTTP offset 分页行为变化：%+v %v", page, err)
	}
}

func TestEachEntryStopsOnErrorAndCancellation(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "entries.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{rt: &platform.Runtime{Store: db}}
	if _, err = db.DB.Exec("INSERT INTO file_entries(run_id,source_id,path,kind) VALUES('run','source','','dir')"); err != nil {
		t.Fatal(err)
	}
	want := errors.New("stop")
	calls := 0
	if err = s.eachEntry(context.Background(), "run_id=?", []any{"run"}, 1, func([]Entry) error { calls++; return want }); !errors.Is(err, want) || calls != 1 {
		t.Fatalf("回调错误未停止遍历：calls=%d err=%v", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = s.eachEntry(ctx, "run_id=?", []any{"run"}, 1, func([]Entry) error { t.Fatal("取消后仍调用回调"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未停止遍历：%v", err)
	}
	if _, err = s.entriesAfter(context.Background(), "run_id=?", []any{"run"}, entryCursor{}, 0); err == nil {
		t.Fatal("接受了无效页大小")
	}
}
