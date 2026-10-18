package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var ErrConflict = errors.New("version conflict")
var tables = map[string]bool{"accounts": true, "storage_pools": true, "sources": true, "backup_plans": true, "repositories": true, "backup_runs": true, "shard_runs": true, "recovery_points": true, "restore_jobs": true, "maintenance_jobs": true, "audit_events": true}

type Store struct {
	DB       *sql.DB
	logMu    sync.Mutex
	logLimit int
	logCount int
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, logLimit: 100000}
	for _, p := range []string{"PRAGMA journal_mode=WAL", "PRAGMA foreign_keys=ON", "PRAGMA busy_timeout=10000"} {
		if _, err = db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	schema := `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY,applied_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS file_entries(run_id TEXT NOT NULL,source_id TEXT NOT NULL,path TEXT NOT NULL,kind TEXT NOT NULL,size INTEGER NOT NULL DEFAULT 0,mtime_ns INTEGER NOT NULL DEFAULT 0,mode INTEGER NOT NULL DEFAULT 0,link_target TEXT NOT NULL DEFAULT '',repo_id TEXT NOT NULL DEFAULT '',snapshot_path TEXT NOT NULL DEFAULT '',PRIMARY KEY(run_id,source_id,path));
 CREATE TABLE IF NOT EXISTS assignments(plan_id TEXT NOT NULL,source_id TEXT NOT NULL,path TEXT NOT NULL,repo_id TEXT NOT NULL,size INTEGER NOT NULL DEFAULT 0,mtime_ns INTEGER NOT NULL DEFAULT 0,PRIMARY KEY(plan_id,source_id,path));
 CREATE INDEX IF NOT EXISTS assignments_repo ON assignments(plan_id,repo_id);
 CREATE TABLE IF NOT EXISTS capacity_reservations(run_id TEXT NOT NULL,account_id TEXT NOT NULL,bytes INTEGER NOT NULL CHECK(bytes>=0),PRIMARY KEY(run_id,account_id));
 CREATE TABLE IF NOT EXISTS idempotency(key TEXT PRIMARY KEY,resource TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS task_logs(id INTEGER PRIMARY KEY AUTOINCREMENT,task_id TEXT NOT NULL,level TEXT NOT NULL DEFAULT 'info',message TEXT NOT NULL,created_at TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS task_logs_task ON task_logs(task_id,id);
 CREATE TABLE IF NOT EXISTS sessions(token_hash TEXT PRIMARY KEY,csrf TEXT NOT NULL,expires_at TEXT NOT NULL);
 INSERT OR IGNORE INTO schema_migrations VALUES(1,strftime('%Y-%m-%dT%H:%M:%SZ','now'));`
	if _, err = db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for t := range tables {
		if _, err = db.Exec("CREATE TABLE IF NOT EXISTS " + t + "(id TEXT PRIMARY KEY,version INTEGER NOT NULL DEFAULT 1,data TEXT NOT NULL CHECK(json_valid(data)),updated_at TEXT NOT NULL)"); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err = migrateNotifications(db); err != nil {
		db.Close()
		return nil, err
	}
	if err = migrateEntryIndexes(db); err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, err
	}
	if err = db.QueryRow("SELECT count(*) FROM task_logs").Scan(&s.logCount); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func migrateNotifications(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS notification_outbox(id TEXT PRIMARY KEY,kind TEXT NOT NULL,generation TEXT NOT NULL,body TEXT NOT NULL,status TEXT NOT NULL DEFAULT 'queued',attempts INTEGER NOT NULL DEFAULT 0,next_at INTEGER NOT NULL DEFAULT 0,created_at TEXT NOT NULL,sent_at TEXT NOT NULL DEFAULT '',last_error TEXT NOT NULL DEFAULT '');
 CREATE INDEX IF NOT EXISTS notification_due ON notification_outbox(status,next_at);
 CREATE TABLE IF NOT EXISTS notification_incidents(account_id TEXT PRIMARY KEY,episode TEXT NOT NULL,category TEXT NOT NULL,updated_at TEXT NOT NULL);
 INSERT OR IGNORE INTO schema_migrations VALUES(2,strftime('%Y-%m-%dT%H:%M:%SZ','now'));`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
func migrateEntryIndexes(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`CREATE INDEX IF NOT EXISTS file_entries_repo_path ON file_entries(run_id,repo_id,source_id,path);
 DROP INDEX IF EXISTS file_entries_repo;
 DROP INDEX IF EXISTS file_entries_tree;
 INSERT OR IGNORE INTO schema_migrations VALUES(3,strftime('%Y-%m-%dT%H:%M:%SZ','now'));`)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Close() error { return s.DB.Close() }
func table(t string) error {
	if !tables[t] {
		return fmt.Errorf("invalid table: %s", t)
	}
	return nil
}
func (s *Store) Get(ctx context.Context, t, id string, out any) error {
	if err := table(t); err != nil {
		return err
	}
	var raw []byte
	if err := s.DB.QueryRowContext(ctx, "SELECT data FROM "+t+" WHERE id=?", id).Scan(&raw); err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func (s *Store) List(ctx context.Context, t string, limit, offset int) ([]json.RawMessage, int, error) {
	if err := table(t); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM "+t).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT data FROM "+t+" ORDER BY updated_at DESC,id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	result := make([]json.RawMessage, 0)
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, 0, err
		}
		result = append(result, json.RawMessage(b))
	}
	return result, total, rows.Err()
}
func (s *Store) All(ctx context.Context, t string) ([]json.RawMessage, error) {
	if err := table(t); err != nil {
		return nil, err
	}
	rows, err := s.DB.QueryContext(ctx, "SELECT data FROM "+t+" ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]json.RawMessage, 0)
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		result = append(result, json.RawMessage(b))
	}
	return result, rows.Err()
}
func (s *Store) Save(ctx context.Context, t, id string, expected int, value any) (int, error) {
	if err := table(t); err != nil {
		return 0, err
	}
	if id == "" {
		return 0, errors.New("empty id")
	}
	bytes, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	var object map[string]any
	if err = json.Unmarshal(bytes, &object); err != nil {
		return 0, err
	}
	if object == nil {
		return 0, errors.New("entity must be object")
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing int
	err = tx.QueryRowContext(ctx, "SELECT version FROM "+t+" WHERE id=?", id).Scan(&existing)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if expected == 0 && err == nil {
		return 0, ErrConflict
	}
	if expected > 0 && (existing != expected || err != nil) {
		return 0, ErrConflict
	}
	version := existing + 1
	object["id"] = id
	object["version"] = version
	bytes, err = json.Marshal(object)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+t+"(id,version,data,updated_at) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET version=excluded.version,data=excluded.data,updated_at=excluded.updated_at", id, version, string(bytes), time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return version, nil
}
func (s *Store) Delete(ctx context.Context, t, id string) error {
	if err := table(t); err != nil {
		return err
	}
	_, err := s.DB.ExecContext(ctx, "DELETE FROM "+t+" WHERE id=?", id)
	return err
}
func (s *Store) Meta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM metadata WHERE key=?", key).Scan(&v)
	return v, err
}
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
func (s *Store) Audit(ctx context.Context, action, resource string) error {
	id := fmt.Sprintf("%d", time.Now().UnixNano())
	_, err := s.Save(ctx, "audit_events", id, 0, map[string]any{"action": action, "resource": resource, "at": time.Now().UTC()})
	return err
}
func (s *Store) Log(ctx context.Context, task, level, message string) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	message = strings.TrimSpace(message)
	if len(message) > 8192 {
		message = message[:8192]
	}
	_, err := s.DB.ExecContext(ctx, "INSERT INTO task_logs(task_id,level,message,created_at) VALUES(?,?,?,?)", task, level, message, time.Now().UTC().Format(time.RFC3339Nano))
	if err == nil {
		s.logCount++
		if s.logLimit > 0 && s.logCount >= s.logLimit+128 {
			err = s.pruneLogs(ctx)
		}
	}
	return err
}

// SetLogLimit bounds global task logs, pruning in small batches to avoid a full scan per engine line.
func (s *Store) SetLogLimit(limit int) error {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if limit < 1 {
		return errors.New("log limit must be positive")
	}
	s.logLimit = limit
	if s.logCount > limit {
		return s.pruneLogs(context.Background())
	}
	return nil
}
func (s *Store) pruneLogs(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, "DELETE FROM task_logs WHERE id <= (SELECT id FROM task_logs ORDER BY id DESC LIMIT 1 OFFSET ?)", s.logLimit)
	if err != nil {
		return err
	}
	return s.DB.QueryRowContext(ctx, "SELECT count(*) FROM task_logs").Scan(&s.logCount)
}
