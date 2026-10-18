package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"

	"vaultpilot/internal/store"
)

func (s *Service) validateRetentionAccounts(ctx context.Context, snapshots map[string][]string) error {
	if len(snapshots) == 0 {
		return ctx.Err()
	}
	ids := make([]string, 0, len(snapshots))
	for id := range snapshots {
		ids = append(ids, id)
	}
	selected, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	rows, err := s.rt.Store.DB.QueryContext(ctx, "SELECT a.data FROM repositories r LEFT JOIN accounts a ON a.id=json_extract(r.data,'$.account_id') WHERE r.id IN (SELECT value FROM json_each(?))", string(selected))
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var raw sql.NullString
		if err = rows.Scan(&raw); err != nil {
			return err
		}
		var account Account
		if !raw.Valid || json.Unmarshal([]byte(raw.String), &account) != nil || !account.Verified || !account.Enabled {
			return fail("RETENTION_ACCOUNT_UNAVAILABLE", "清理所需账号不存在、未验证或已停用")
		}
		if account.MaintenanceReserve <= 0 {
			return fail("MAINTENANCE_RESERVE_REQUIRED", "清理需要明确的维护空间预留")
		}
		count++
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if count != len(snapshots) {
		return sql.ErrNoRows
	}
	return nil
}

// Remove only IDs included in the authenticated completion catalog, preserving
// IDs added while prune was running. JSON fields outside this change survive.
func (s *Service) removePendingForget(ctx context.Context, removals map[string][]string) error {
	if len(removals) == 0 {
		return ctx.Err()
	}
	ids := make([]string, 0, len(removals))
	for id := range removals {
		ids = append(ids, id)
	}
	selected, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id,version,data FROM repositories WHERE id IN (SELECT value FROM json_each(?))", string(selected))
	if err != nil {
		return err
	}
	type update struct {
		ID      string `json:"id"`
		Version int    `json:"version"`
		Data    string `json:"data"`
	}
	updates := make([]update, 0, len(removals))
	for rows.Next() {
		var id string
		var version int
		var raw []byte
		if err = rows.Scan(&id, &version, &raw); err != nil {
			rows.Close()
			return err
		}
		var repository Repository
		var object map[string]json.RawMessage
		if err = json.Unmarshal(raw, &repository); err != nil {
			rows.Close()
			return err
		}
		if err = json.Unmarshal(raw, &object); err != nil {
			rows.Close()
			return err
		}
		remove := map[string]bool{}
		for _, snapshot := range removals[id] {
			remove[snapshot] = true
		}
		repository.PendingForget = slices.DeleteFunc(repository.PendingForget, func(snapshot string) bool { return remove[snapshot] })
		slices.Sort(repository.PendingForget)
		repository.PendingForget = slices.Compact(repository.PendingForget)
		if len(repository.PendingForget) == 0 {
			delete(object, "pending_forget")
		} else {
			object["pending_forget"], err = json.Marshal(repository.PendingForget)
			if err != nil {
				rows.Close()
				return err
			}
		}
		object["version"], err = json.Marshal(version + 1)
		if err != nil {
			rows.Close()
			return err
		}
		raw, err = json.Marshal(object)
		if err != nil {
			rows.Close()
			return err
		}
		updates = append(updates, update{id, version, string(raw)})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(updates) != len(removals) {
		return sql.ErrNoRows
	}
	payload, err := json.Marshal(updates)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, "UPDATE repositories SET version=repositories.version+1,data=json_extract(u.value,'$.data'),updated_at=? FROM json_each(?) u WHERE repositories.id=json_extract(u.value,'$.id') AND repositories.version=json_extract(u.value,'$.version')", now(), string(payload))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != int64(len(updates)) {
		return store.ErrConflict
	}
	return tx.Commit()
}
