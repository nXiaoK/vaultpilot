package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/process"
	"vaultpilot/internal/store"
)

type CheckRequest struct {
	RepositoryIDs []string `json:"repository_ids"`
	Mode          string   `json:"mode"`
	Sample        string   `json:"sample"`
}

func (s *Service) check(ctx context.Context, t *Task) error {
	var req CheckRequest
	if err := json.Unmarshal(t.Request, &req); err != nil {
		return err
	}
	if req.Mode == "" {
		req.Mode = "structure"
	}
	if req.Mode != "structure" && req.Mode != "sample" && req.Mode != "full" {
		return fail("INVALID_CHECK_MODE", "检查类型必须为 structure、sample 或 full")
	}
	if len(req.RepositoryIDs) == 0 {
		raws, err := s.rt.Store.All(ctx, "repositories")
		if err != nil {
			return err
		}
		for _, raw := range raws {
			var repo Repository
			_ = json.Unmarshal(raw, &repo)
			if repo.Status != "retired" {
				req.RepositoryIDs = append(req.RepositoryIDs, repo.ID)
			}
		}
	}
	results := []map[string]any{}
	s.stage(t, "checking_repositories")
	for i, id := range req.RepositoryIDs {
		var repo Repository
		if err := s.rt.Store.Get(ctx, "repositories", id, &repo); err != nil {
			return err
		}
		if repo.Status == "retired" {
			return fail("REPOSITORY_RETIRED", "所选仓库已清空退役，请选择现有仓库")
		}
		var account Account
		if err := s.rt.Store.Get(ctx, "accounts", repo.AccountID, &account); err != nil {
			return fail("CHECK_ACCOUNT_MISSING", "检查所需账号不可用")
		}
		unlockAccount := s.lock("account:" + account.ID)
		unlockRepo := s.lock("repo:" + repo.ID)
		args := []string{"check"}
		if req.Mode == "full" {
			args = append(args, "--read-data")
		} else if req.Mode == "sample" {
			if req.Sample == "" {
				req.Sample = "10%"
			}
			args = append(args, "--read-data-subset", req.Sample)
		}
		_, err := s.restic(ctx, t, repo, account, args, func(line string) { s.log(t, line) })
		unlockRepo()
		unlockAccount()
		if err != nil {
			return fail("REPOSITORY_CHECK_FAILED", "仓库检查失败："+id+"："+err.Error())
		}
		results = append(results, map[string]any{"repository_id": id, "mode": req.Mode, "status": "passed", "checked_at": now()})
		t.Progress = float64(i+1) / float64(len(req.RepositoryIDs))
		s.save(t)
	}
	t.Result = map[string]any{"checks": results, "mode": req.Mode, "reads_all_data": req.Mode == "full"}
	return s.rt.Store.Audit(ctx, "repository.check", t.ID)
}

type RetentionRequest struct {
	PlanID   string   `json:"plan_id"`
	PointIDs []string `json:"point_ids"`
	Confirm  bool     `json:"confirm"`
}
type RetentionPreview struct {
	PendingPruneRepositories int        `json:"pending_prune_repositories"`
	PlanID                   string     `json:"plan_id"`
	KeepIDs                  []string   `json:"keep_ids"`
	DeleteIDs                []string   `json:"delete_ids"`
	Snapshots                []Snapshot `json:"snapshots"`
	Blocked                  bool       `json:"blocked"`
	Reasons                  []string   `json:"reasons"`
}

func (s *Service) retentionPreview(ctx context.Context, planID string) (RetentionPreview, error) {
	preview := RetentionPreview{PlanID: planID, KeepIDs: []string{}, DeleteIDs: []string{}, Snapshots: []Snapshot{}, Reasons: []string{}}
	var plan Plan
	if err := s.rt.Store.Get(ctx, "backup_plans", planID, &plan); err != nil {
		return preview, err
	}
	pending, err := s.pendingPruneRepositories(ctx, planID)
	if err != nil {
		return preview, err
	}
	preview.PendingPruneRepositories = len(pending)
	points := []Point{}
	raws, err := s.rt.Store.All(ctx, "recovery_points")
	if err != nil {
		return preview, err
	}
	for _, raw := range raws {
		var p Point
		_ = json.Unmarshal(raw, &p)
		if p.PlanID == planID && p.DeletedAt == "" {
			if p.Status != "completed" {
				preview.Blocked = true
				preview.Reasons = append(preview.Reasons, "存在未完成恢复点")
				continue
			}
			points = append(points, p)
		}
	}
	sortPoints(points)
	keep := map[string]bool{}
	daily, weekly, monthly := map[string]bool{}, map[string]bool{}, map[string]bool{}
	loc, _ := time.LoadLocation(plan.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	for i, p := range points {
		at, err := time.Parse(time.RFC3339Nano, p.FinishedAt)
		if err != nil {
			return preview, err
		}
		at = at.In(loc)
		year, week := at.ISOWeek()
		d, w, m := at.Format("2006-01-02"), fmt.Sprintf("%04d-%02d", year, week), at.Format("2006-01")
		retain := i == 0 || p.Protected
		if !daily[d] && len(daily) < plan.Retention.Daily {
			daily[d] = true
			retain = true
		}
		if !weekly[w] && len(weekly) < plan.Retention.Weekly {
			weekly[w] = true
			retain = true
		}
		if !monthly[m] && len(monthly) < plan.Retention.Monthly {
			monthly[m] = true
			retain = true
		}
		if retain {
			keep[p.ID] = true
			preview.KeepIDs = append(preview.KeepIDs, p.ID)
		} else {
			preview.DeleteIDs = append(preview.DeleteIDs, p.ID)
		}
	}
	if len(points) > 1 && points[0].FileCount*100 < points[1].FileCount*70 {
		preview.Blocked = true
		preview.Reasons = append(preview.Reasons, "最近恢复点文件数量异常下降，清理已暂停")
	}
	for _, id := range plan.SourceIDs {
		var src Source
		if err = s.rt.Store.Get(ctx, "sources", id, &src); err != nil {
			preview.Blocked = true
			preview.Reasons = append(preview.Reasons, "源目录登记缺失")
			continue
		}
		identity, err := platform.MountIdentity(src.Path)
		if err != nil || identity != src.MountIdentity {
			preview.Blocked = true
			preview.Reasons = append(preview.Reasons, "源挂载异常")
		}
	}
	tasks, _, err := s.rt.Store.List(ctx, "backup_runs", 100, 0)
	if err == nil {
		failed := 0
		for _, raw := range tasks {
			var t Task
			_ = json.Unmarshal(raw, &t)
			if t.PlanID != planID || t.Kind != "backup" || !terminal(t.Status) {
				continue
			}
			if t.Status == "completed" {
				break
			}
			failed++
			if failed >= 3 {
				preview.Blocked = true
				preview.Reasons = append(preview.Reasons, "最近备份连续失败")
				break
			}
		}
	}
	referenced := map[string]bool{}
	deleteSet := map[string]bool{}
	for _, id := range preview.DeleteIDs {
		deleteSet[id] = true
	}
	for _, raw := range raws {
		var p Point
		_ = json.Unmarshal(raw, &p)
		if p.DeletedAt != "" || deleteSet[p.ID] {
			continue
		}
		for _, snap := range p.Snapshots {
			if !snap.Empty {
				referenced[snap.RepositoryID+":"+snap.SnapshotID] = true
			}
		}
	}
	for _, p := range points {
		if keep[p.ID] {
			continue
		}
		for _, snap := range p.Snapshots {
			if !snap.Empty && !referenced[snap.RepositoryID+":"+snap.SnapshotID] {
				preview.Snapshots = append(preview.Snapshots, snap)
			}
		}
	}
	return preview, nil
}
func (s *Service) retentionPreviewHTTP(w http.ResponseWriter, r *http.Request) {
	var req RetentionRequest
	if err := decode(r, &req); err != nil {
		problem(w, err)
		return
	}
	preview, err := s.retentionPreview(r.Context(), req.PlanID)
	if err != nil {
		problem(w, err)
		return
	}
	write(w, 200, preview)
}
func retentionSnapshots(raws []json.RawMessage, selected map[string]bool) (map[string][]string, error) {
	referenced := map[string]bool{}
	points := make([]Point, 0, len(raws))
	for _, raw := range raws {
		var p Point
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		points = append(points, p)
		if p.DeletedAt != "" || selected[p.ID] {
			continue
		}
		for _, snap := range p.Snapshots {
			if !snap.Empty {
				referenced[snap.RepositoryID+":"+snap.SnapshotID] = true
			}
		}
	}
	snapshots := map[string][]string{}
	for _, p := range points {
		if !selected[p.ID] {
			continue
		}
		for _, snap := range p.Snapshots {
			if !snap.Empty && !referenced[snap.RepositoryID+":"+snap.SnapshotID] {
				snapshots[snap.RepositoryID] = append(snapshots[snap.RepositoryID], snap.SnapshotID)
			}
		}
	}
	for id, ids := range snapshots {
		slices.Sort(ids)
		snapshots[id] = slices.Compact(ids)
	}
	return snapshots, nil
}

func (s *Service) pendingPruneRepositories(ctx context.Context, planID string) ([]Repository, error) {
	raws, err := s.rt.Store.All(ctx, "repositories")
	if err != nil {
		return nil, err
	}
	repos := []Repository{}
	for _, raw := range raws {
		var repo Repository
		if err = json.Unmarshal(raw, &repo); err != nil {
			return nil, err
		}
		if repo.PlanID == planID && repo.Status != "retired" && len(repo.PendingForget) > 0 {
			repos = append(repos, repo)
		}
	}
	return repos, nil
}

func (s *Service) recordRetentionDeletion(ctx context.Context, selected map[string]bool, snapshots map[string][]string) error {
	for attempt := 0; attempt < 3; attempt++ {
		err := func() error {
			tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, table := range []string{"repositories", "recovery_points"} {
				ids := []string{}
				if table == "repositories" {
					for id := range snapshots {
						ids = append(ids, id)
					}
				} else {
					for id := range selected {
						ids = append(ids, id)
					}
				}
				for _, id := range ids {
					var raw []byte
					var version int
					if err = tx.QueryRowContext(ctx, "SELECT data,version FROM "+table+" WHERE id=?", id).Scan(&raw, &version); err != nil {
						return err
					}
					var object map[string]any
					if err = json.Unmarshal(raw, &object); err != nil {
						return err
					}
					if table == "repositories" {
						var repo Repository
						if err = json.Unmarshal(raw, &repo); err != nil {
							return err
						}
						pending := append(repo.PendingForget, snapshots[id]...)
						slices.Sort(pending)
						object["pending_forget"] = slices.Compact(pending)
					} else {
						object["status"], object["deleted_at"] = "deleted", now()
					}
					object["version"] = version + 1
					raw, err = json.Marshal(object)
					if err != nil {
						return err
					}
					result, err := tx.ExecContext(ctx, "UPDATE "+table+" SET version=?,data=?,updated_at=? WHERE id=? AND version=?", version+1, string(raw), now(), id, version)
					if err != nil {
						return err
					}
					changed, err := result.RowsAffected()
					if err != nil {
						return err
					}
					if changed != 1 {
						return store.ErrConflict
					}
				}
			}
			return tx.Commit()
		}()
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return store.ErrConflict
}

func (s *Service) updatePendingForget(ctx context.Context, repoID string, add, remove []string) error {
	for attempt := 0; attempt < 3; attempt++ {
		var repo Repository
		if err := s.rt.Store.Get(ctx, "repositories", repoID, &repo); err != nil {
			return err
		}
		pending := map[string]bool{}
		for _, id := range repo.PendingForget {
			pending[id] = true
		}
		for _, id := range add {
			pending[id] = true
		}
		for _, id := range remove {
			delete(pending, id)
		}
		repo.PendingForget = make([]string, 0, len(pending))
		for id := range pending {
			repo.PendingForget = append(repo.PendingForget, id)
		}
		slices.Sort(repo.PendingForget)
		_, err := s.rt.Store.Save(ctx, "repositories", repo.ID, repo.Version, repo)
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return store.ErrConflict
}

// 调用方持有计划和目录锁，保持有效快照引用不变。
func (s *Service) prunePending(ctx context.Context, t *Task, planID string) (int, error) {
	repos, err := s.pendingPruneRepositories(ctx, planID)
	if err != nil {
		return 0, err
	}
	raws, err := s.rt.Store.All(ctx, "recovery_points")
	if err != nil {
		return 0, err
	}
	referenced := map[string]bool{}
	for _, raw := range raws {
		var p Point
		if err = json.Unmarshal(raw, &p); err != nil {
			return 0, err
		}
		if p.DeletedAt != "" {
			continue
		}
		for _, snap := range p.Snapshots {
			if !snap.Empty {
				referenced[snap.RepositoryID+":"+snap.SnapshotID] = true
			}
		}
	}
	t.prunedSnapshots = map[string][]string{}
	s.stage(t, "pruning")
	pruned := 0
	for _, repo := range repos {
		var account Account
		if err = s.rt.Store.Get(ctx, "accounts", repo.AccountID, &account); err != nil || !account.Verified || !account.Enabled {
			return pruned, fail("RETENTION_ACCOUNT_UNAVAILABLE", "清理所需账号不存在、未验证或已停用；恢复账号后可重试")
		}
		if account.MaintenanceReserve <= 0 {
			return pruned, fail("MAINTENANCE_RESERVE_REQUIRED", "清理需要明确的维护空间预留")
		}
		unlockAccount := s.lock("account:" + account.ID)
		unlockRepo := s.lock("repo:" + repo.ID)
		err = func() error {
			defer unlockAccount()
			defer unlockRepo()
			var snapshots []struct {
				ID string `json:"id"`
			}
			out, err := s.restic(ctx, t, repo, account, []string{"snapshots", "--json"}, nil)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(out, &snapshots); err != nil {
				return err
			}
			existing := map[string]bool{}
			for _, snap := range snapshots {
				existing[snap.ID] = true
			}
			ids := []string{}
			for _, id := range repo.PendingForget {
				if !referenced[repo.ID+":"+id] && existing[id] {
					ids = append(ids, id)
				}
			}
			// Restic 0.18.1 对不存在的 ID 退出 0，但输出 "Ignoring ... failed to load snapshot"，只 forget 现存快照。
			if len(ids) > 0 {
				if _, err = s.restic(ctx, t, repo, account, append([]string{"forget"}, ids...), func(line string) { s.log(t, line) }); err != nil {
					return err
				}
			}
			// 上次可能已完成 forget，但 prune 失败；即使没有现存待忘记快照也要重试 prune。
			if _, err = s.restic(ctx, t, repo, account, []string{"prune"}, func(line string) { s.log(t, line) }); err != nil {
				return err
			}
			t.prunedSnapshots[repo.ID] = append([]string(nil), repo.PendingForget...)
			return nil
		}()
		if err != nil {
			return pruned, fail("RETENTION_ENGINE_FAILED", "删除决策已提交，仓库清理失败，可在历史清理中重试："+err.Error())
		}
		pruned++
		if err = s.refreshAccount(ctx, &account); err != nil {
			return pruned, err
		}
	}
	return pruned, nil
}

func (s *Service) retain(ctx context.Context, t *Task) error {
	t.prunedSnapshots = nil
	var req RetentionRequest
	if err := json.Unmarshal(t.Request, &req); err != nil {
		return err
	}
	if !req.Confirm {
		return fail("CONFIRM_REQUIRED", "删除历史数据必须显式 confirm")
	}
	releasePlan := s.lock("plan:" + req.PlanID)
	defer releasePlan()
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	preview, err := s.retentionPreview(ctx, req.PlanID)
	if err != nil {
		return err
	}
	if preview.Blocked {
		return fail("RETENTION_PAUSED", strings.Join(preview.Reasons, "；"))
	}
	raws, err := s.rt.Store.All(ctx, "recovery_points")
	if err != nil {
		return err
	}
	if len(req.PointIDs) == 0 && preview.PendingPruneRepositories == 0 {
		completedCleanup := false
		for _, raw := range raws {
			var p Point
			if err = json.Unmarshal(raw, &p); err != nil {
				return err
			}
			if p.PlanID == req.PlanID && p.DeletedAt != "" {
				completedCleanup = true
			}
		}
		if !completedCleanup {
			return fail("EMPTY_RETENTION_SELECTION", "请从预览选择需要删除的恢复点")
		}
	}
	allowed := map[string]bool{}
	for _, id := range preview.DeleteIDs {
		allowed[id] = true
	}
	selected := map[string]bool{}
	for _, id := range req.PointIDs {
		if !allowed[id] {
			return fail("RETENTION_REFERENCE_PROTECTED", "选择包含受保护或保留策略需要的恢复点")
		}
		selected[id] = true
	}
	snapshots, err := retentionSnapshots(raws, selected)
	if err != nil {
		return err
	}
	var plan Plan
	if err = s.rt.Store.Get(ctx, "backup_plans", req.PlanID, &plan); err != nil {
		return err
	}
	var pool Pool
	if err = s.rt.Store.Get(ctx, "storage_pools", plan.PoolID, &pool); err != nil {
		return err
	}
	for id := range snapshots {
		var repo Repository
		if err = s.rt.Store.Get(ctx, "repositories", id, &repo); err != nil {
			return err
		}
		var account Account
		if err = s.rt.Store.Get(ctx, "accounts", repo.AccountID, &account); err != nil || !account.Verified || !account.Enabled {
			return fail("RETENTION_ACCOUNT_UNAVAILABLE", "清理所需账号不存在、未验证或已停用")
		}
		if account.MaintenanceReserve <= 0 {
			return fail("MAINTENANCE_RESERVE_REQUIRED", "清理需要明确的维护空间预留")
		}
	}
	if req.PointIDs == nil {
		req.PointIDs = []string{}
	}
	result := map[string]any{"deleted_point_ids": []string{}, "pruned_repositories": 0, "cleanup_pending": preview.PendingPruneRepositories > 0}
	t.Result = result
	if len(req.PointIDs) > 0 {
		s.stage(t, "recording_deletions")
		commitErr := s.commit(ctx, t, pool, nil, req.PointIDs)
		if commitErr != nil && !degraded(commitErr) {
			return commitErr
		}
		result["deleted_point_ids"] = req.PointIDs
		result["cleanup_pending"] = len(snapshots) > 0 || preview.PendingPruneRepositories > 0
		localCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		err = s.recordRetentionDeletion(localCtx, selected, snapshots)
		cancel()
		if err != nil {
			return err
		}
		if commitErr != nil {
			return commitErr
		}
	}
	pruned, cleanupErr := s.prunePending(ctx, t, req.PlanID)
	result["pruned_repositories"] = pruned
	if cleanupErr == nil && len(t.prunedSnapshots) > 0 {
		cleanupErr = s.commit(ctx, t, pool, nil, nil)
		if cleanupErr == nil || degraded(cleanupErr) {
			localCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			for id, ids := range t.prunedSnapshots {
				if clearErr := s.updatePendingForget(localCtx, id, nil, ids); clearErr != nil {
					cleanupErr = clearErr
					break
				}
			}
			cancel()
		}
	}
	pending, err := s.pendingPruneRepositories(context.WithoutCancel(ctx), req.PlanID)
	result["cleanup_pending"] = err != nil || len(pending) > 0
	if cleanupErr != nil {
		return cleanupErr
	}
	if err != nil {
		return err
	}
	return s.rt.Store.Audit(ctx, "retention.complete", t.ID)
}

type MigrateRequest struct {
	CleanupOld       bool   `json:"cleanup_old"`
	OldLocationIndex int    `json:"old_location_index"`
	OldLocationPath  string `json:"old_location_path"`
	RepositoryID     string `json:"repository_id"`
	TargetAccountID  string `json:"target_account_id"`
	Confirm          bool   `json:"confirm"`
}

func (s *Service) migrate(ctx context.Context, t *Task) error {
	var req MigrateRequest
	if err := json.Unmarshal(t.Request, &req); err != nil {
		return err
	}
	if !req.Confirm {
		return fail("CONFIRM_REQUIRED", "整仓迁移需要显式确认")
	}
	var repo Repository
	if err := s.rt.Store.Get(ctx, "repositories", req.RepositoryID, &repo); err != nil {
		return err
	}
	releasePlan := s.lock("plan:" + repo.PlanID)
	defer releasePlan()
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()

	repo = Repository{}
	if err := s.rt.Store.Get(ctx, "repositories", req.RepositoryID, &repo); err != nil {
		return err
	}
	if repo.Status == "retired" {
		return fail("REPOSITORY_RETIRED", "仓库已清空退役，不能迁移或清理旧副本")
	}
	if req.CleanupOld {
		return s.cleanupOld(ctx, t, repo, req)
	}
	old := repo
	var source, target Account
	if err := s.rt.Store.Get(ctx, "accounts", repo.AccountID, &source); err != nil {
		return err
	}
	if err := s.rt.Store.Get(ctx, "accounts", req.TargetAccountID, &target); err != nil {
		return err
	}
	if source.ID == target.ID || !target.Verified || !target.Enabled {
		return fail("INVALID_MIGRATION_TARGET", "目标必须是其他已验证启用账号")
	}
	var plan Plan
	if err := s.rt.Store.Get(ctx, "backup_plans", repo.PlanID, &plan); err != nil {
		return err
	}
	var pool Pool
	if err := s.rt.Store.Get(ctx, "storage_pools", plan.PoolID, &pool); err != nil {
		return err
	}
	if !slices.Contains(pool.AccountIDs, target.ID) {
		return fail("MIGRATION_TARGET_OUTSIDE_POOL", "目标账号必须属于该仓库所在计划的存储池；如需使用新账号，请先将其加入存储池")
	}
	if err := s.refreshAccount(ctx, &target); err != nil {
		return err
	}
	out, err := s.rcloneOutput(ctx, []Account{source}, "size", s.remoteFile(source, repo.Path), "--json")
	if err != nil {
		return err
	}
	var usage struct {
		Bytes int64 `json:"bytes"`
	}
	if err = json.Unmarshal(out, &usage); err != nil {
		return err
	}
	tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	var reserved int64
	if err = tx.QueryRowContext(ctx, "SELECT coalesce(sum(bytes),0) FROM capacity_reservations WHERE account_id=?", target.ID).Scan(&reserved); err != nil {
		tx.Rollback()
		return err
	}
	estimate := usage.Bytes*113/100 + 8<<20
	if capacity(target, reserved) < estimate {
		tx.Rollback()
		return fail("CAPACITY_EXHAUSTED", "迁移目标容量不足")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO capacity_reservations(run_id,account_id,bytes) VALUES(?,?,?)", t.ID, target.ID, estimate); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	defer func() {
		if s.refreshAccountAfterTask(t, &target) == nil {
			_, _ = s.rt.Store.DB.Exec("DELETE FROM capacity_reservations WHERE run_id=?", t.ID)
		}
	}()
	accountIDs := []string{source.ID, target.ID}
	sort.Strings(accountIDs)
	releaseFirst := s.lock("account:" + accountIDs[0])
	defer releaseFirst()
	releaseSecond := s.lock("account:" + accountIDs[1])
	defer releaseSecond()
	unlockRepo := s.lock("repo:" + req.RepositoryID)
	defer unlockRepo()
	s.stage(t, "copying_repository")
	destination := repo.Path + "-migration-" + t.ID
	if _, err = s.rcloneRun(ctx, []Account{source, target}, func(line string) { s.log(t, line) }, "copy", s.remoteFile(source, repo.Path), s.remoteFile(target, destination), "--checksum"); err != nil {
		return fail("MIGRATION_COPY_FAILED", "整仓复制未完成，旧位置保持有效")
	}
	repo.AccountID = target.ID
	repo.Path = destination
	s.stage(t, "verifying_repository")
	if _, err = s.restic(ctx, t, repo, target, []string{"check", "--read-data"}, func(line string) { s.log(t, line) }); err != nil {
		return fail("MIGRATION_CHECK_FAILED", "目标仓库全量读取检查失败；旧位置保持有效")
	}
	repo.Locations = append(repo.Locations, Location{AccountID: target.ID, DriveID: target.DriveID, Region: target.Region, TenantID: target.TenantID, Path: destination, Verified: true, At: now()})
	_, err = s.rt.Store.Save(ctx, "repositories", repo.ID, -1, repo)
	if err != nil {
		return err
	}

	s.stage(t, "committing_location")
	commitErr := s.commit(ctx, t, pool, nil, nil)
	if commitErr != nil && !degraded(commitErr) {
		_, _ = s.rt.Store.Save(context.Background(), "repositories", old.ID, -1, old)
		return commitErr
	}
	t.Result = map[string]any{"repository_id": repo.ID, "new_account_id": target.ID, "old_copy_preserved": true, "old_location": old.Path, "location_count": len(repo.Locations)}
	if commitErr != nil {
		return commitErr
	}
	return s.rt.Store.Audit(ctx, "repository.migrate", repo.ID)
}

func (s *Service) locationAccount(ctx context.Context, loc Location) (Account, error) {
	var account Account
	if s.rt.Store.Get(ctx, "accounts", loc.AccountID, &account) == nil && account.DriveID == loc.DriveID && account.Region == loc.Region && account.TenantID == loc.TenantID {
		return account, nil
	}
	raws, err := s.rt.Store.All(ctx, "accounts")
	if err != nil {
		return account, err
	}
	for _, raw := range raws {
		var a Account
		_ = json.Unmarshal(raw, &a)
		if a.DriveID == loc.DriveID && a.Region == loc.Region && a.TenantID == loc.TenantID {
			return a, nil
		}
	}
	return account, fail("ACCOUNT_MISSING", "旧副本所在账号未接入")
}

// Called under the plan and catalog mutexes. The current location remains verified throughout deletion.
func (s *Service) cleanupOld(ctx context.Context, t *Task, repo Repository, req MigrateRequest) error {
	if req.OldLocationIndex < 0 || req.OldLocationIndex >= len(repo.Locations) {
		return fail("INVALID_OLD_LOCATION", "旧位置索引无效")
	}
	location := repo.Locations[req.OldLocationIndex]
	if req.OldLocationPath != "" && req.OldLocationPath != location.Path {
		return fail("VERSION_CONFLICT", "仓库位置已变化，请刷新")
	}
	if location.Status == "deleted" {
		return fail("OLD_COPY_ALREADY_DELETED", "旧副本已清理")
	}
	if location.Path == repo.Path && location.AccountID == repo.AccountID {
		return fail("CURRENT_LOCATION_PROTECTED", "不能清理当前仓库位置")
	}
	if !safeLogical(location.Path) || !strings.HasPrefix(location.Path, "VaultPilot/"+s.rt.VaultID()+"/repositories/"+repo.ID) {
		return fail("INVALID_OLD_LOCATION", "旧位置不属于该仓库")
	}
	var current Account
	if err := s.rt.Store.Get(ctx, "accounts", repo.AccountID, &current); err != nil {
		return err
	}
	oldAccount, err := s.locationAccount(ctx, location)
	if err != nil {
		return err
	}
	ids := []string{current.ID}
	if oldAccount.ID != current.ID {
		ids = append(ids, oldAccount.ID)
	}
	sort.Strings(ids)
	releases := []func(){}
	for _, id := range ids {
		releases = append(releases, s.lock("account:"+id))
	}
	defer func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}()
	releaseRepo := s.lock("repo:" + repo.ID)
	defer releaseRepo()
	verifiedCurrent := false
	for _, loc := range repo.Locations {
		if loc.AccountID == current.ID && loc.Path == repo.Path && loc.Verified {
			verifiedCurrent = true
		}
	}
	if !verifiedCurrent {
		return fail("CURRENT_LOCATION_UNVERIFIED", "当前仓库位置尚未验证，不能删除旧副本")
	}
	s.stage(t, "verifying_current_location")
	if _, err = s.restic(ctx, t, repo, current, []string{"check", "--read-data"}, func(line string) { s.log(t, line) }); err != nil {
		return fail("MIGRATION_CHECK_FAILED", "当前仓库全量读取校验失败，旧副本保留")
	}
	raws, err := s.rt.Store.All(ctx, "recovery_points")
	if err != nil {
		return err
	}
	references := map[string]bool{}
	for _, raw := range raws {
		var point Point
		_ = json.Unmarshal(raw, &point)
		if point.DeletedAt != "" {
			continue
		}
		for _, snap := range point.Snapshots {
			if snap.RepositoryID == repo.ID && !snap.Empty {
				references[snap.SnapshotID] = true
			}
		}
	}
	for id := range references {
		if len(id) != 64 {
			return fail("SNAPSHOT_REFERENCE_INVALID", "有效恢复点使用的快照引用无效")
		}
		if _, err = s.restic(ctx, t, repo, current, []string{"cat", "snapshot", id}, nil); err != nil {
			return fail("SNAPSHOT_REFERENCE_MISSING", "当前位置缺少有效恢复点引用的快照，旧副本保留")
		}
	}
	original := repo
	original.Locations = append([]Location(nil), repo.Locations...)
	repo.Locations[req.OldLocationIndex].Verified = false
	repo.Locations[req.OldLocationIndex].Status = "cleanup_pending"
	if _, err = s.rt.Store.Save(ctx, "repositories", repo.ID, -1, repo); err != nil {
		return err
	}
	pool, err := s.catalogPool(ctx, repo.PlanID)
	if err != nil {
		_, _ = s.rt.Store.Save(ctx, "repositories", original.ID, -1, original)
		return err
	}
	s.stage(t, "committing_old_copy_retirement")
	if err = s.commit(ctx, t, pool, nil, nil); err != nil {
		if !degraded(err) {
			_, _ = s.rt.Store.Save(context.Background(), "repositories", original.ID, -1, original)
		}
		return err
	}
	s.stage(t, "deleting_old_copy")
	_, err = s.rcloneRun(ctx, []Account{oldAccount}, func(line string) { s.log(t, line) }, "purge", s.remoteFile(oldAccount, location.Path))
	if err != nil && process.ExitCode(err) != 3 {
		return fail("OLD_COPY_DELETE_FAILED", "当前位置已确认有效，旧副本清理失败可重试")
	}
	repo.Locations[req.OldLocationIndex].Status = "deleted"
	if _, err = s.rt.Store.Save(ctx, "repositories", repo.ID, -1, repo); err != nil {
		return err
	}
	if err = s.commit(ctx, t, pool, nil, nil); err != nil {
		return err
	}
	if err = s.refreshAccount(ctx, &oldAccount); err != nil {
		return err
	}
	t.Result = map[string]any{"repository_id": repo.ID, "cleanup_old": true, "old_location": location, "old_copy_deleted": true, "new_location_verified": true, "snapshot_references_checked": len(references)}
	return s.rt.Store.Audit(ctx, "repository.cleanup_old", repo.ID)
}
