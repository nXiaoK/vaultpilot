package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/store"
)

func TestEntryTraversalScale(t *testing.T) {
	if os.Getenv("VP_ENTRY_SCALE_TEST") != "1" {
		t.Skip("set VP_ENTRY_SCALE_TEST=1 to traverse 200000 synthetic entries")
	}
	db, err := store.Open(filepath.Join(t.TempDir(), "entries.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := &Service{rt: &platform.Runtime{Store: db}}
	const count = 200000
	for base := 0; base < count; base += 512 {
		tx, err := db.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare("INSERT INTO file_entries(run_id,source_id,path,kind,repo_id) VALUES(?,?,?,'file',?)")
		if err != nil {
			t.Fatal(err)
		}
		for i := base; i < base+512 && i < count; i++ {
			if _, err = stmt.Exec("run", fmt.Sprintf("source-%d", i%3), fmt.Sprintf("目录/%08d", i), fmt.Sprintf("repo-%d", i%4)); err != nil {
				t.Fatal(err)
			}
		}
		stmt.Close()
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	seen := make(map[string]bool, count)
	for repo := 0; repo < 4; repo++ {
		read := 0
		err := s.eachEntry(context.Background(), "run_id=? AND repo_id=?", []any{"run", fmt.Sprintf("repo-%d", repo)}, 512, func(entries []Entry) error {
			for _, e := range entries {
				key := e.SourceID + ":" + e.Path
				if seen[key] {
					return fmt.Errorf("duplicate entry: %s", key)
				}
				seen[key] = true
				read++
			}
			return nil
		})
		if err != nil || read != count/4 {
			t.Fatalf("repo=%d count=%d err=%v", repo, read, err)
		}
	}

	if len(seen) != count {
		t.Fatalf("total=%d want=%d", len(seen), count)
	}
	t.Logf("keyset traversal: rows=%d repos=4 page=512 elapsed=%s", count, time.Since(start))
}
