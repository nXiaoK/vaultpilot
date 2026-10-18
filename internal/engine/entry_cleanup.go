package engine

import (
	"context"
	"strings"
	"time"
)

// Reevaluate live references for every bounded batch so newly started tasks and
// published recovery points stay protected while the sweep yields the DB.
func (s *Service) cleanupFileEntries(ctx context.Context, where string, args []any) error {
	statuses := make([]any, 0, len(terminalStatuses))
	for _, status := range terminalStatuses {
		statuses = append(statuses, status)
	}
	args = append(append([]any{}, args...), statuses...)
	protected := " AND run_id NOT IN (SELECT coalesce(json_extract(data,'$.run_id'),'') FROM recovery_points WHERE coalesce(json_extract(data,'$.deleted_at'),'')='') AND run_id NOT IN (SELECT id FROM backup_runs WHERE coalesce(json_extract(data,'$.status'),'') NOT IN (" + strings.TrimSuffix(strings.Repeat("?,", len(terminalStatuses)), ",") + "))"
	return s.deleteEntryBatches(ctx, "DELETE FROM file_entries WHERE rowid IN (SELECT rowid FROM file_entries WHERE "+where+protected+" LIMIT 5000)", args, true)
}

func (s *Service) deleteEntryBatches(ctx context.Context, query string, args []any, lockIndex bool) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		unlock := func() {}
		if lockIndex {
			unlock = s.lock("recovery-index")
		}
		result, err := s.rt.Store.DB.ExecContext(ctx, query, args...)
		unlock()
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count < 5000 {
			return nil
		}
	}
}

func (s *Service) cleanupSnapshotEntries(ctx context.Context, where string, args []any) error {
	var exists int
	if err := s.rt.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='engine_snapshot_entries_v2'").Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return nil
	}
	return s.deleteEntryBatches(ctx, "DELETE FROM engine_snapshot_entries_v2 WHERE rowid IN (SELECT rowid FROM engine_snapshot_entries_v2 WHERE "+where+" LIMIT 5000)", args, false)
}

func (s *Service) cleanupOrphanEntries(ctx context.Context) error {
	if err := s.cleanupFileEntries(ctx, "1=1", nil); err != nil {
		return err
	}
	args := make([]any, 0, len(terminalStatuses))
	for _, status := range terminalStatuses {
		args = append(args, status)
	}
	return s.cleanupSnapshotEntries(ctx, "task_id NOT IN (SELECT id FROM backup_runs WHERE coalesce(json_extract(data,'$.status'),'') NOT IN ("+strings.TrimSuffix(strings.Repeat("?,", len(terminalStatuses)), ",")+"))", args)
}

func (s *Service) cleanupTaskEntries(t *Task) {
	if t.Status == "completed" && t.Kind != "preview" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.cleanupFileEntries(ctx, "run_id=?", []any{t.ID}); err != nil {
		s.log(t, "清理任务文件清单失败，启动时将重试："+err.Error())
	}
}

func (s *Service) cleanupDeletedPointEntries(t *Task, planID string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.cleanupFileEntries(ctx, "run_id IN (SELECT coalesce(json_extract(data,'$.run_id'),'') FROM recovery_points WHERE json_extract(data,'$.plan_id')=? AND coalesce(json_extract(data,'$.deleted_at'),'')<>'')", []any{planID}); err != nil {
		s.log(t, "清理已删除恢复点文件清单失败，启动时将重试："+err.Error())
	}
}
