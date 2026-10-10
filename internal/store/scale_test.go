package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Opt-in index benchmark, not a cloud throughput or 5 TB physical backup test.
func TestMillionEntryIndex(t *testing.T) {
	if os.Getenv("VP_SCALE_TEST") != "1" {
		t.Skip("set VP_SCALE_TEST=1 to insert one million synthetic entries")
	}
	dbPath := filepath.Join(t.TempDir(), "million.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Now()
	const count = 1000000
	for batch := 0; batch < count; batch += 1000 {
		tx, err := s.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		stmt, err := tx.Prepare("INSERT INTO file_entries(run_id,source_id,path,kind,size,mtime_ns,mode,repo_id,snapshot_path) VALUES(?,?,?,'file',?,?,?, ?,?)")
		if err != nil {
			t.Fatal(err)
		}
		for i := batch; i < batch+1000; i++ {
			path := fmt.Sprintf("目录/%08d.bin", i)
			_, err = stmt.Exec("synthetic-run", "synthetic-source", path, int64(5<<20), int64(i), 0644, fmt.Sprintf("repo-%02d", i%10), "/synthetic/"+path)
			if err != nil {
				t.Fatal(err)
			}
		}
		stmt.Close()
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	insertTime := time.Since(start)
	start = time.Now()
	rows, err := s.DB.Query("SELECT path,size FROM file_entries WHERE run_id=? AND source_id=? AND path>=? ORDER BY path LIMIT 100", "synthetic-run", "synthetic-source", "目录/00999900")
	if err != nil {
		t.Fatal(err)
	}
	read := 0
	for rows.Next() {
		var path string
		var size int64
		if err = rows.Scan(&path, &size); err != nil {
			t.Fatal(err)
		}
		read++
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if read != 100 {
		t.Fatalf("page count %d", read)
	}
	pageTime := time.Since(start)
	var actual, logical int64
	if err = s.DB.QueryRow("SELECT count(*),sum(size) FROM file_entries WHERE run_id='synthetic-run'").Scan(&actual, &logical); err != nil {
		t.Fatal(err)
	}
	if actual != count {
		t.Fatalf("row count %d", actual)
	}
	_, _ = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	stat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	report := map[string]any{"test": "synthetic SQLite file index, no cloud I/O", "rows": actual, "synthetic_logical_bytes": logical, "physical_backup_verified": false, "insert_seconds": insertTime.Seconds(), "page_100_seconds": pageTime.Seconds(), "database_bytes": stat.Size(), "go_heap_alloc_bytes": memory.Alloc, "go_version": runtime.Version(), "platform": runtime.GOOS + "/" + runtime.GOARCH, "at": time.Now().UTC().Format(time.RFC3339)}
	data, _ := json.MarshalIndent(report, "", "  ")
	t.Log(string(data))
	if path := os.Getenv("VP_SCALE_REPORT"); path != "" {
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
