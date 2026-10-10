package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
)

func TestOptimisticVersionPreventsLostUpdate(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	version, err := s.Save(ctx, "accounts", "a", 0, map[string]any{"name": "initial"})
	if err != nil || version != 1 {
		t.Fatal(version, err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	success, conflict := 0, 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Save(ctx, "accounts", "a", 1, map[string]any{"name": "new"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				success++
			} else if errors.Is(err, ErrConflict) {
				conflict++
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if success != 1 || conflict != 9 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
	var object map[string]any
	if err = s.Get(ctx, "accounts", "a", &object); err != nil {
		t.Fatal(err)
	}
	if object["version"] != float64(2) {
		t.Fatal(object)
	}
}
func TestRejectsDynamicTableNames(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _, err = s.List(context.Background(), "accounts;DROP TABLE sources", 10, 0)
	if err == nil {
		t.Fatal("unsafe table accepted")
	}
}
