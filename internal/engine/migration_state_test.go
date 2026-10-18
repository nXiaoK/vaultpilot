package engine

import (
	"context"
	"errors"
	"testing"

	"vaultpilot/internal/store"
)

func TestMaintenanceRepositoryCASPreservesConcurrentChanges(t *testing.T) {
	s, _ := allocationFixture(t)
	ctx := context.Background()
	original := Repository{ID: "repository", PlanID: "plan", AccountID: "source", Path: "original", Status: "ready"}
	if _, err := s.rt.Store.Save(ctx, "repositories", original.ID, 0, original); err != nil {
		t.Fatal(err)
	}
	if err := s.rt.Store.Get(ctx, "repositories", original.ID, &original); err != nil {
		t.Fatal(err)
	}
	updated := original
	updated.Path = "destination"
	saved, err := s.saveRepositoryCAS(ctx, original, updated, nil)
	if err != nil {
		t.Fatal(err)
	}
	var columnVersion int
	var persisted Repository
	if err = s.rt.Store.Get(ctx, "repositories", original.ID, &persisted); err != nil {
		t.Fatal(err)
	}
	if err = s.rt.Store.DB.QueryRow("SELECT version FROM repositories WHERE id=?", original.ID).Scan(&columnVersion); err != nil {
		t.Fatal(err)
	}
	if saved.Version != original.Version+1 || saved.Version != columnVersion || persisted.Version != columnVersion || persisted.Path != updated.Path {
		t.Fatalf("CAS版本与映射不一致：%+v column=%d persisted=%+v", saved, columnVersion, persisted)
	}
	changed := saved
	changed.Path = "other-writer"
	if _, err = s.rt.Store.Save(ctx, "repositories", saved.ID, saved.Version, changed); err != nil {
		t.Fatal(err)
	}
	if _, err = s.saveRepositoryCAS(ctx, original, updated, nil); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("过期规划未拒绝：%v", err)
	}
	cause := errors.New("publication failed")
	err = s.rollbackRepository(ctx, &Task{ID: "migration"}, saved, original, cause)
	if !errors.Is(err, store.ErrConflict) || !errors.Is(err, cause) {
		t.Fatalf("回退没有返回两个错误：%v", err)
	}
	if err = s.rt.Store.Get(ctx, "repositories", saved.ID, &persisted); err != nil || persisted.Path != "other-writer" {
		t.Fatalf("回退覆盖并发更新：%+v %v", persisted, err)
	}
}

func TestMaintenanceRepositoryCASChecksValidationAndAffectedRows(t *testing.T) {
	for _, mode := range []string{"validation", "ignored update"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := allocationFixture(t)
			original := Repository{ID: "repository", Path: "original"}
			if _, err := s.rt.Store.Save(t.Context(), "repositories", original.ID, 0, original); err != nil {
				t.Fatal(err)
			}
			if err := s.rt.Store.Get(t.Context(), "repositories", original.ID, &original); err != nil {
				t.Fatal(err)
			}
			updated := original
			updated.Path = "destination"
			var validation func(maintenanceReader) error
			expected := store.ErrConflict
			if mode == "validation" {
				expected = errors.New("account state changed")
				validation = func(maintenanceReader) error { return expected }
			} else {
				if _, err := s.rt.Store.DB.Exec("CREATE TRIGGER ignore_mapping BEFORE UPDATE ON repositories BEGIN SELECT RAISE(IGNORE); END"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.saveRepositoryCAS(t.Context(), original, updated, validation); !errors.Is(err, expected) {
				t.Fatalf("CAS失败未返回预期错误：%v", err)
			}
			var persisted Repository
			if err := s.rt.Store.Get(t.Context(), "repositories", original.ID, &persisted); err != nil || persisted.Version != original.Version || persisted.Path != original.Path {
				t.Fatalf("失败CAS改变旧映射：%+v %v", persisted, err)
			}
		})
	}
}
