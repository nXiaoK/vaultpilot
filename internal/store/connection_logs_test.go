package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestConnectionPragmasSurviveReconnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state ?#%.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.DB.SetMaxIdleConns(0)
	for attempt := 0; attempt < 2; attempt++ {
		for pragma, want := range map[string]int{"foreign_keys": 1, "busy_timeout": 10000, "synchronous": 1} {
			var got int
			if err := s.DB.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil || got != want {
				t.Fatalf("reconnection %d: %s=%d, %v", attempt, pragma, got, err)
			}
		}
		var journal string
		if err := s.DB.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil || journal != "wal" {
			t.Fatalf("journal mode: %q %v", journal, err)
		}
		if err := s.SetMeta(context.Background(), "same-file", "kept"); err != nil {
			t.Fatal(err)
		}
	}
	value, err := s.Meta(context.Background(), "same-file")
	if err != nil || value != "kept" {
		t.Fatalf("escaped DSN did not preserve database identity: %q %v", value, err)
	}
}

func TestAuditEventsAtSameNanosecondAreDistinct(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "audit.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	at := time.Unix(1792320000, 123).UTC()
	for _, action := range []string{"first", "second"} {
		if err := s.auditAt(context.Background(), action, "resource", at); err != nil {
			t.Fatal(err)
		}
	}
	var count, distinct int
	if err := s.DB.QueryRow("SELECT count(*),count(DISTINCT id) FROM audit_events WHERE json_extract(data,'$.at')=?", at.Format(time.RFC3339Nano)).Scan(&count, &distinct); err != nil || count != 2 || distinct != 2 {
		t.Fatalf("same-tick audits lost: count=%d distinct=%d %v", count, distinct, err)
	}
	items, total, err := s.List(context.Background(), "audit_events", 10, 0)
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("audit list: %d %v", total, err)
	}
}

func TestLogTruncationPreservesUTF8(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "logs.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Log(context.Background(), "task", "info", strings.Repeat("中", 3000)); err != nil {
		t.Fatal(err)
	}
	var message string
	if err := s.DB.QueryRow("SELECT message FROM task_logs WHERE task_id=?", "task").Scan(&message); err != nil || !utf8.ValidString(message) || len(message) > 8192 || message != strings.Repeat("中", 2730) {
		t.Fatalf("truncated log is invalid or altered: %d bytes, %v", len(message), err)
	}
}
