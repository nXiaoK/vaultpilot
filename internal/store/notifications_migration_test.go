package store

import (
	"path/filepath"
	"testing"
)

func TestNotificationV2MigrationPreservesV1StateAndIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Save(t.Context(), "accounts", "existing", 0, map[string]any{"name": "original account"}); err != nil {
		t.Fatal(err)
	}
	if err = s.SetMeta(t.Context(), "credential:existing", "existing encrypted credential"); err != nil {
		t.Fatal(err)
	}
	// Recreate the pre-upgrade schema while preserving its application state.
	if _, err = s.DB.Exec("DROP TABLE notification_outbox; DROP TABLE notification_incidents; DELETE FROM schema_migrations WHERE version=2"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	for i := 0; i < 2; i++ {
		s, err = Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var versions int
		if err = s.DB.QueryRow("SELECT count(*) FROM schema_migrations WHERE version=2").Scan(&versions); err != nil || versions != 1 {
			t.Fatal(versions, err)
		}
		var account map[string]any
		if err = s.Get(t.Context(), "accounts", "existing", &account); err != nil || account["name"] != "original account" {
			t.Fatal(account, err)
		}
		if credential, err := s.Meta(t.Context(), "credential:existing"); err != nil || credential != "existing encrypted credential" {
			t.Fatal("existing credential changed")
		}
		if _, err = s.DB.Exec("INSERT OR IGNORE INTO notification_incidents(account_id,episode,category,updated_at) VALUES('a','first','authorization_required','now')"); err != nil {
			t.Fatal(err)
		}
		var count int
		s.DB.QueryRow("SELECT count(*) FROM notification_incidents").Scan(&count)
		if count != 1 {
			t.Fatal("incident lost on reopen")
		}
		s.Close()
	}
}
