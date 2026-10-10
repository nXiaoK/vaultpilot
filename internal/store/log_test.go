package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLogRetentionKeepsRecentRows(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.SetLogLimit(10); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 150; i++ {
		if err = s.Log(context.Background(), "task", "info", "line"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.SetLogLimit(10); err != nil {
		t.Fatal(err)
	}
	var count, last int
	if err = s.DB.QueryRow("SELECT count(*),max(id) FROM task_logs").Scan(&count, &last); err != nil {
		t.Fatal(err)
	}
	if count != 10 || last != 150 {
		t.Fatal(count, last)
	}
}
