package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"time"

	"vaultpilot/internal/store"
)

type maintenanceReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func sameAccountStorage(a, b Account) bool {
	return a.ID == b.ID && a.Backend == b.Backend && a.Remote == b.Remote && a.LocalPath == b.LocalPath && a.DriveID == b.DriveID && a.Region == b.Region && a.TenantID == b.TenantID
}

func readMaintenanceAccounts(ctx context.Context, db maintenanceReader, first, second string) (map[string]Account, error) {
	rows, err := db.QueryContext(ctx, "SELECT id,data FROM accounts WHERE id IN (?,?)", first, second)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	accounts := map[string]Account{}
	for rows.Next() {
		var id string
		var raw []byte
		var account Account
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &account); err != nil {
			return nil, err
		}
		accounts[id] = account
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if _, ok := accounts[first]; !ok {
		return nil, sql.ErrNoRows
	}
	if _, ok := accounts[second]; !ok {
		return nil, sql.ErrNoRows
	}
	return accounts, nil
}

func readMaintenancePool(ctx context.Context, db maintenanceReader, planID string) (Pool, error) {
	var plan Plan
	var pool Pool
	var raw []byte
	if err := db.QueryRowContext(ctx, "SELECT data FROM backup_plans WHERE id=?", planID).Scan(&raw); err != nil {
		return pool, err
	}
	if err := json.Unmarshal(raw, &plan); err != nil {
		return pool, err
	}
	if err := db.QueryRowContext(ctx, "SELECT data FROM storage_pools WHERE id=?", plan.PoolID).Scan(&raw); err != nil {
		return pool, err
	}
	if err := json.Unmarshal(raw, &pool); err != nil {
		return pool, err
	}
	return pool, nil
}

func repositoryReferences(ctx context.Context, db maintenanceReader, repoID string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT coalesce(json_extract(s.value,'$.snapshot_id'),'') FROM recovery_points p JOIN json_each(p.data,'$.snapshots') s WHERE coalesce(json_extract(p.data,'$.deleted_at'),'')='' AND json_extract(s.value,'$.repository_id')=? AND coalesce(json_extract(s.value,'$.empty'),0)=0 ORDER BY 1", repoID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Validation and mapping CAS share one short transaction; remote publication
// starts only after it commits. The callback must only use the supplied reader.
func (s *Service) saveRepositoryCAS(ctx context.Context, previous, next Repository, validate func(maintenanceReader) error) (Repository, error) {
	tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return Repository{}, err
	}
	defer tx.Rollback()
	var raw []byte
	var version int
	if err = tx.QueryRowContext(ctx, "SELECT version,data FROM repositories WHERE id=?", previous.ID).Scan(&version, &raw); err != nil {
		return Repository{}, err
	}
	var current Repository
	if err = json.Unmarshal(raw, &current); err != nil {
		return Repository{}, err
	}
	if version != previous.Version || !reflect.DeepEqual(current, previous) {
		return Repository{}, store.ErrConflict
	}
	if validate != nil {
		if err = validate(tx); err != nil {
			return Repository{}, err
		}
	}
	next.Version = version + 1
	raw, err = json.Marshal(next)
	if err != nil {
		return Repository{}, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE repositories SET version=?,data=?,updated_at=? WHERE id=? AND version=?", next.Version, string(raw), now(), previous.ID, version)
	if err != nil {
		return Repository{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Repository{}, err
	}
	if count != 1 {
		return Repository{}, store.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return Repository{}, err
	}
	return next, nil
}

func (s *Service) rollbackRepository(ctx context.Context, t *Task, current, previous Repository, cause error) error {
	localCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if _, err := s.saveRepositoryCAS(localCtx, current, previous, nil); err != nil {
		s.log(t, "仓库状态回退失败，请刷新检查当前映射："+err.Error())
		return errors.Join(cause, err)
	}
	return cause
}

func (s *Service) lockMaintenancePublication(ctx context.Context, accounts []string, repoID string, writeIndex bool) (func(), error) {
	if err := s.configurationMu.RLockContext(ctx); err != nil {
		return nil, err
	}
	releaseConfiguration := s.configurationMu.RUnlock
	releaseAccounts, err := s.rt.LockAccountConfigurationContext(ctx)
	if err != nil {
		releaseConfiguration()
		return nil, err
	}
	var releaseIndex func()
	if writeIndex {
		err = s.recoveryIndex.LockContext(ctx)
		releaseIndex = s.recoveryIndex.Unlock
	} else {
		err = s.recoveryIndex.RLockContext(ctx)
		releaseIndex = s.recoveryIndex.RUnlock
	}
	if err != nil {
		releaseAccounts()
		releaseConfiguration()
		return nil, err
	}
	if err = s.catalogMu.LockContext(ctx); err != nil {
		releaseIndex()
		releaseAccounts()
		releaseConfiguration()
		return nil, err
	}
	releaseResources, err := s.lockResources(ctx, accounts, repoID)
	if err != nil {
		s.catalogMu.Unlock()
		releaseIndex()
		releaseAccounts()
		releaseConfiguration()
		return nil, err
	}
	return func() {
		releaseResources()
		s.catalogMu.Unlock()
		releaseIndex()
		releaseAccounts()
		releaseConfiguration()
	}, nil
}

func validateMaintenanceAccounts(ctx context.Context, db maintenanceReader, first, second Account) error {
	current, err := readMaintenanceAccounts(ctx, db, first.ID, second.ID)
	if err != nil {
		return err
	}
	if !sameAccountStorage(first, current[first.ID]) || !sameAccountStorage(second, current[second.ID]) {
		return fail("VERSION_CONFLICT", "账号物理位置已变化，请重新运行")
	}
	return nil
}

func validateCleanupState(ctx context.Context, db maintenanceReader, current, old Account, repoID string, references []string) error {
	accounts, err := readMaintenanceAccounts(ctx, db, current.ID, old.ID)
	if err != nil {
		return err
	}
	if !sameAccountStorage(current, accounts[current.ID]) || !sameAccountStorage(old, accounts[old.ID]) {
		return fail("VERSION_CONFLICT", "账号物理位置已变化，请重新运行")
	}
	if !accounts[current.ID].Verified || accounts[current.ID].Status == "error" {
		return fail("CURRENT_LOCATION_UNVERIFIED", "当前位置账号已失效，不能删除旧副本")
	}
	latest, err := repositoryReferences(ctx, db, repoID)
	if err != nil {
		return err
	}
	if !slices.Equal(latest, references) {
		return store.ErrConflict
	}
	return nil
}
