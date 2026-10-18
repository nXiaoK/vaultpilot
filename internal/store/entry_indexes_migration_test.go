package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func TestEntryIndexMigrationPreservesRowsAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index-migration.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(`INSERT INTO file_entries(run_id,source_id,path,kind,repo_id) VALUES('run','source','original','file','repo');
 DROP INDEX file_entries_repo_path;
 CREATE INDEX file_entries_repo ON file_entries(run_id,repo_id);
 CREATE INDEX file_entries_tree ON file_entries(run_id,source_id,path);
 DELETE FROM schema_migrations WHERE version=3;`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for attempt := 0; attempt < 2; attempt++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer s.Close()
			var old int
			if err = s.DB.QueryRow("SELECT count(*) FROM sqlite_schema WHERE type='index' AND name IN ('file_entries_repo','file_entries_tree')").Scan(&old); err != nil || old != 0 {
				t.Fatalf("旧索引仍存在：%d %v", old, err)
			}
			rows, err := s.DB.Query("PRAGMA index_info(file_entries_repo_path)")
			if err != nil {
				t.Fatal(err)
			}
			columns := []string{}
			for rows.Next() {
				var seq, cid int
				var name string
				if err = rows.Scan(&seq, &cid, &name); err != nil {
					t.Fatal(err)
				}
				columns = append(columns, name)
			}
			err = rows.Err()
			rows.Close()
			if err != nil || !reflect.DeepEqual(columns, []string{"run_id", "repo_id", "source_id", "path"}) {
				t.Fatalf("新索引列错误：%v %v", columns, err)
			}
			var count, versions int
			if err = s.DB.QueryRow("SELECT count(*) FROM file_entries WHERE run_id='run' AND source_id='source' AND path='original' AND repo_id='repo'").Scan(&count); err != nil || count != 1 {
				t.Fatalf("存量清单变化：%d %v", count, err)
			}
			if err = s.DB.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=3").Scan(&versions); err != nil || versions != 1 {
				t.Fatalf("迁移版本不幂等：%d %v", versions, err)
			}
		}()
	}
}
