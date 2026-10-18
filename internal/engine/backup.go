package engine

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
	"vaultpilot/internal/platform"
	"vaultpilot/internal/process"
	"vaultpilot/internal/store"
)

func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
}
func excluded(path string, patterns []string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(pattern, "/")
		if pattern == "" {
			continue
		}
		if match, err := doublestar.PathMatch(pattern, path); err == nil && match {
			return true
		}
		if strings.HasSuffix(pattern, "/**") {
			base := strings.TrimSuffix(pattern, "/**")
			if match, err := doublestar.PathMatch(base, path); err == nil && match {
				return true
			}
		}
		if !strings.Contains(pattern, "/") {
			for _, part := range strings.Split(path, "/") {
				if match, err := doublestar.PathMatch(pattern, part); err == nil && match {
					return true
				}
			}
		}
	}
	return false
}
func (s *Service) scan(ctx context.Context, t *Task, p Plan) (map[string]Source, error) {
	_, err := s.rt.Store.DB.ExecContext(ctx, "DELETE FROM file_entries WHERE run_id=?", t.ID)
	if err != nil {
		return nil, err
	}
	sources := map[string]Source{}
	batch := []Entry{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		stmt, err := tx.PrepareContext(ctx, "INSERT INTO file_entries(run_id,source_id,path,kind,size,mtime_ns,mode,link_target) VALUES(?,?,?,?,?,?,?,?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, e := range batch {
			if _, err = stmt.ExecContext(ctx, t.ID, e.SourceID, e.Path, e.Kind, e.Size, e.MtimeNS, e.Mode, e.LinkTarget); err != nil {
				return err
			}
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		batch = batch[:0]
		s.save(t)
		return nil
	}
	for _, id := range p.SourceIDs {
		var src Source
		if err := s.rt.Store.Get(ctx, "sources", id, &src); err != nil {
			return nil, err
		}
		canonical, err := s.rt.ValidateSourcePath(src.Path)
		if err != nil {
			return nil, fail("SOURCE_BOUNDARY", err.Error())
		}
		src.Path = canonical
		identity, err := platform.MountIdentity(src.Path)
		if err != nil || identity != src.MountIdentity {
			return nil, fail("SOURCE_MOUNT_CHANGED", "源目录挂载身份变化："+src.Name)
		}
		sources[id] = src
		rootInfo, err := os.Lstat(src.Path)
		if err != nil {
			return nil, err
		}
		rootDev := uint64(0)
		if st, ok := rootInfo.Sys().(*syscall.Stat_t); ok {
			rootDev = uint64(st.Dev)
		}
		patterns := append(append([]string{}, src.Excludes...), p.Excludes...)
		err = filepath.WalkDir(src.Path, func(path string, d fs.DirEntry, walkErr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if walkErr != nil {
				return fail("SOURCE_READ_FAILED", "读取源失败："+path+"："+walkErr.Error())
			}
			rel, err := filepath.Rel(src.Path, path)
			if err != nil {
				return err
			}
			if rel == "." {
				rel = ""
			}
			rel = filepath.ToSlash(rel)
			if !utf8.ValidString(rel) {
				return fail("UNSUPPORTED_FILENAME_ENCODING", "源路径不是有效 UTF-8，无法生成可恢复清单")
			}
			if rel != "" && (!src.IncludeHidden && strings.HasPrefix(d.Name(), ".") || excluded(rel, patterns)) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			if src.OneFileSystem && d.IsDir() {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && uint64(st.Dev) != rootDev {
					return filepath.SkipDir
				}
			}
			e := Entry{SourceID: id, Path: rel, Size: info.Size(), MtimeNS: info.ModTime().UnixNano(), Mode: uint32(info.Mode())}
			switch {
			case info.Mode().IsRegular():
				e.Kind = "file"
				if src.MaxFileBytes > 0 && e.Size > src.MaxFileBytes {
					return nil
				}
				f, err := os.Open(path)
				if err != nil {
					return fail("SOURCE_READ_FAILED", "文件不可读："+path)
				}
				f.Close()
				t.ScannedBytes += e.Size
			case info.IsDir():
				e.Kind = "dir"
				e.Size = 0
			case info.Mode()&os.ModeSymlink != 0:
				e.Kind = "symlink"
				e.Size = 0
				e.LinkTarget, err = os.Readlink(path)
				if err != nil {
					return err
				}
				if !utf8.ValidString(e.LinkTarget) {
					return fail("UNSUPPORTED_FILENAME_ENCODING", "符号链接目标不是有效 UTF-8")
				}
			default:
				return fail("UNSUPPORTED_FILE_TYPE", "不支持特殊文件："+path)
			}
			batch = append(batch, e)
			if len(batch) >= 512 {
				return flush()
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return sources, flush()
}
func (s *Service) accounts(ctx context.Context, pool Pool) (map[string]Account, error) {
	out := map[string]Account{}
	domains := map[string]bool{}
	for _, id := range pool.AccountIDs {
		var a Account
		if err := s.rt.Store.Get(ctx, "accounts", id, &a); err != nil {
			return nil, err
		}
		if a.QuotaDomain != "" && domains[a.QuotaDomain] {
			return nil, fail("DUPLICATE_QUOTA_DOMAIN", "存储池重复使用同一配额域")
		}
		domains[a.QuotaDomain] = true
		if !a.Verified || !a.Enabled {
			continue
		}
		if err := s.refreshAccount(ctx, &a); err != nil {
			return nil, err
		}
		_, _ = s.rt.Store.DB.ExecContext(ctx, "DELETE FROM capacity_reservations WHERE account_id=? AND run_id IN (SELECT id FROM backup_runs WHERE json_extract(data,\x27$.status\x27) IN (\x27interrupted\x27,\x27failed\x27,\x27cancelled\x27,\x27completed\x27))", a.ID)
		out[a.ID] = a
	}
	if len(out) == 0 {
		return nil, fail("NO_USABLE_ACCOUNT", "池中没有已验证、启用的账号")
	}
	return out, nil
}
func (s *Service) refreshAccount(ctx context.Context, a *Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var free, total, used, project int64
	if a.Backend == "local" {
		var st syscall.Statfs_t
		if err := syscall.Statfs(a.LocalPath, &st); err != nil {
			return fail("ACCOUNT_OFFLINE", "本地存储不可用："+a.Name)
		}
		free = int64(st.Bavail) * int64(st.Bsize)
		total = int64(st.Blocks) * int64(st.Bsize)
		used = total - free
		root := filepath.Join(a.LocalPath, "VaultPilot", s.rt.VaultID())
		err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.Type().IsRegular() {
				info, e := d.Info()
				if e != nil {
					return e
				}
				project += info.Size()
			}
			return nil
		})
		if err != nil {
			return err
		}
	} else {
		out, err := s.rcloneOutput(ctx, []Account{*a}, "about", a.Remote+":", "--json")
		_ = s.rt.SyncRcloneCredentials()
		if err != nil {
			return fail("QUOTA_UNAVAILABLE", "无法刷新账号配额："+a.Name+"："+err.Error())
		}
		var quota struct {
			Free  *int64 `json:"free"`
			Total *int64 `json:"total"`
			Used  *int64 `json:"used"`
		}
		if json.Unmarshal(out, &quota) != nil || quota.Free == nil || quota.Total == nil || quota.Used == nil {
			return fail("QUOTA_UNKNOWN", "账号未返回完整容量："+a.Name)
		}
		free, total, used = *quota.Free, *quota.Total, *quota.Used
		out, err = s.rcloneOutput(ctx, []Account{*a}, "size", a.Remote+":VaultPilot/"+s.rt.VaultID(), "--json")
		_ = s.rt.SyncRcloneCredentials()
		if err != nil {
			return fail("PROJECT_SIZE_UNKNOWN", "无法核对项目占用："+a.Name+"："+err.Error())
		}
		var usage struct {
			Bytes int64 `json:"bytes"`
		}
		if err = json.Unmarshal(out, &usage); err != nil {
			return err
		}
		project = usage.Bytes
	}
	a.Free = free
	a.ProjectUsed = project
	a.QuotaAt = now()
	return saveAccountQuota(ctx, s.rt.Store, *a, total, used)
}

type accountQuotaStore interface {
	Get(context.Context, string, string, any) error
	Save(context.Context, string, string, int, any) (int, error)
}

func saveAccountQuota(ctx context.Context, db accountQuotaStore, a Account, total, used int64) error {
	for attempt := 0; attempt < 3; attempt++ {
		var raw map[string]any
		if err := db.Get(ctx, "accounts", a.ID, &raw); err != nil {
			return err
		}
		version, ok := raw["version"].(float64)
		if !ok || version < 1 {
			return errors.New("账号版本无效")
		}
		raw["free_bytes"] = a.Free
		raw["total_bytes"] = total
		raw["used_bytes"] = used
		raw["project_used_bytes"] = a.ProjectUsed
		raw["quota_at"] = a.QuotaAt
		if _, err := db.Save(ctx, "accounts", a.ID, int(version), raw); !errors.Is(err, store.ErrConflict) {
			return err
		}
	}
	return store.ErrConflict
}

func (s *Service) refreshAccountAfterTask(t *Task, a *Account) error {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Minute)
	defer cancel()
	err := s.refreshAccount(ctx, a)
	if err != nil {
		s.log(t, "收尾刷新账号配额失败："+a.Name+"："+err.Error())
	}
	return err
}

func capacity(a Account, reserved int64) int64 {
	cloud := a.Free - a.CloudReserve
	budget := a.Budget - a.ProjectUsed - a.MaintenanceReserve
	if budget < cloud {
		cloud = budget
	}
	cloud -= reserved
	if cloud < 0 {
		return 0
	}
	return cloud
}
func estimate(e Entry, unchanged bool) int64 {
	if unchanged {
		return 1024
	}
	if e.Kind != "file" {
		return 8192
	}
	if e.Size > math.MaxInt64/113 {
		return math.MaxInt64
	}
	return e.Size*113/100 + 65536
}
func (s *Service) allocate(ctx context.Context, t *Task, p Plan, pool Pool, accounts map[string]Account) (Planning, map[string]Repository, error) {
	repos := map[string]Repository{}
	raws, err := s.rt.Store.All(ctx, "repositories")
	if err != nil {
		return Planning{}, nil, err
	}
	for _, raw := range raws {
		var repo Repository
		_ = json.Unmarshal(raw, &repo)
		if repo.PlanID == p.ID {
			repo.LogicalBytes = 0
			repos[repo.ID] = repo
		}
	}
	tx, err := s.rt.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return Planning{}, nil, err
	}
	defer tx.Rollback()
	free := map[string]int64{}
	reservations := map[string]int64{}
	for id, a := range accounts {
		var n int64
		if err := tx.QueryRowContext(ctx, "SELECT coalesce(sum(bytes),0) FROM capacity_reservations WHERE account_id=? AND run_id<>?", id, t.ID).Scan(&n); err != nil {
			return Planning{}, nil, err
		}
		free[id] = capacity(a, n)
	}
	target := pool.ShardTarget
	if target < 1 {
		target = 150 << 30
	}
	alloc := map[string]*Allocation{}
	result := Planning{Shards: []Allocation{}, CatalogReplicas: pool.CatalogReplicas, Warnings: []string{}}
	offset := 0
	for {
		rows, err := tx.QueryContext(ctx, "SELECT source_id,path,kind,size,mtime_ns,mode,link_target FROM file_entries WHERE run_id=? ORDER BY source_id,path LIMIT 512 OFFSET ?", t.ID, offset)
		if err != nil {
			return result, nil, err
		}
		batch := []Entry{}
		for rows.Next() {
			var e Entry
			if err := rows.Scan(&e.SourceID, &e.Path, &e.Kind, &e.Size, &e.MtimeNS, &e.Mode, &e.LinkTarget); err != nil {
				rows.Close()
				return result, nil, err
			}
			batch = append(batch, e)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return result, nil, err
		}
		if len(batch) == 0 {
			break
		}
		offset += len(batch)
		for _, e := range batch {
			var repoID string
			var size, mtime int64
			err := tx.QueryRowContext(ctx, "SELECT repo_id,size,mtime_ns FROM assignments WHERE plan_id=? AND source_id=? AND path=?", p.ID, e.SourceID, e.Path).Scan(&repoID, &size, &mtime)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return result, nil, err
			}
			growth := estimate(e, err == nil && size == e.Size && mtime == e.MtimeNS)
			if repoID != "" {
				repo, ok := repos[repoID]
				if !ok {
					return result, nil, fail("ASSIGNMENT_BROKEN", "稳定归属指向不存在仓库")
				}
				if _, ok := accounts[repo.AccountID]; !ok {
					return result, nil, fail("ASSIGNED_ACCOUNT_OFFLINE", "现有文件归属账号已停用或不可用")
				}
				if free[repo.AccountID] < growth {
					return result, nil, fail("CAPACITY_EXHAUSTED", "原分片容量不足，请扩容或整仓迁移："+e.SourceID+"/"+e.Path)
				}
			} else {
				ids := make([]string, 0, len(accounts))
				for id := range accounts {
					ids = append(ids, id)
				}
				sort.Slice(ids, func(i, j int) bool { return free[ids[i]] > free[ids[j]] })
				accountID := ""
				for _, id := range ids {
					candidate := ""
					for repositoryID, repository := range repos {
						if repository.AccountID == id && repository.PlanID == p.ID && repository.LogicalBytes+e.Size <= target && repository.Status != "retired" {
							candidate = repositoryID
							break
						}
					}
					overhead := int64(8 << 20)
					if candidate != "" {
						overhead = 0
					}
					if free[id] >= growth+overhead {
						accountID = id
						repoID = candidate
						break
					}
				}
				if accountID == "" {
					return result, nil, fail("CAPACITY_EXHAUSTED", "文件超出所有账号可用预算："+e.SourceID+"/"+e.Path)
				}
				if repoID == "" {
					repoID = uid()
					password := uid() + uid()
					sealed, err := s.rt.Keys.Seal([]byte(password))
					if err != nil {
						return result, nil, err
					}
					a := accounts[accountID]
					repo := Repository{ID: repoID, PlanID: p.ID, AccountID: accountID, Path: "VaultPilot/" + s.rt.VaultID() + "/repositories/" + repoID, Password: base64.StdEncoding.EncodeToString(sealed), Status: "new", Locations: []Location{{AccountID: accountID, DriveID: a.DriveID, Region: a.Region, TenantID: a.TenantID, Path: "VaultPilot/" + s.rt.VaultID() + "/repositories/" + repoID, At: now(), Verified: false}}}
					repos[repoID] = repo
					free[accountID] -= 8 << 20
					reservations[accountID] += 8 << 20
				}
			}
			repo := repos[repoID]
			free[repo.AccountID] -= growth
			reservations[repo.AccountID] += growth
			repo.LogicalBytes += e.Size
			repos[repoID] = repo
			if alloc[repoID] == nil {
				alloc[repoID] = &Allocation{RepositoryID: repoID, AccountID: repo.AccountID}
			}
			sh := alloc[repoID]
			sh.FileCount++
			sh.LogicalBytes += e.Size
			sh.EstimatedGrowth += growth
			result.FileCount++
			result.LogicalBytes += e.Size
			if _, err = tx.ExecContext(ctx, "UPDATE file_entries SET repo_id=? WHERE run_id=? AND source_id=? AND path=?", repoID, t.ID, e.SourceID, e.Path); err != nil {
				return result, nil, err
			}
		}
	}
	if t.Kind != "preview" {
		for id, n := range reservations {
			if _, err := tx.ExecContext(ctx, "INSERT INTO capacity_reservations(run_id,account_id,bytes) VALUES(?,?,?) ON CONFLICT(run_id,account_id) DO UPDATE SET bytes=excluded.bytes", t.ID, id, n); err != nil {
				return result, nil, err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return result, nil, err
	}
	for _, a := range alloc {
		result.Shards = append(result.Shards, *a)
	}
	sort.Slice(result.Shards, func(i, j int) bool { return result.Shards[i].RepositoryID < result.Shards[j].RepositoryID })
	return result, repos, nil
}
func (s *Service) backup(ctx context.Context, t *Task) error {
	releasePlan := s.lock("plan:" + t.PlanID)
	defer releasePlan()
	var p Plan
	if err := s.rt.Store.Get(ctx, "backup_plans", t.PlanID, &p); err != nil {
		return err
	}
	var pool Pool
	if err := s.rt.Store.Get(ctx, "storage_pools", p.PoolID, &pool); err != nil {
		return err
	}
	if pool.CatalogReplicas < 1 {
		pool.CatalogReplicas = 2
	}
	s.stage(t, "scanning")
	sources, err := s.scan(ctx, t, p)
	if err != nil {
		return err
	}
	s.stage(t, "planning")
	accounts, err := s.accounts(ctx, pool)
	if err != nil {
		return err
	}
	plan, repos, err := s.allocate(ctx, t, p, pool, accounts)
	if err != nil {
		return err
	}
	t.Result = plan
	s.save(t)
	if t.Kind == "preview" {
		return nil
	}
	defer func() {
		clean := true
		growth := int64(0)
		for id := range accounts {
			a := accounts[id]
			if s.refreshAccountAfterTask(t, &a) != nil {
				clean = false
			} else {
				growth += a.ProjectUsed - accounts[id].ProjectUsed
			}
		}
		t.CloudGrowthBytes = growth
		if clean {
			_, _ = s.rt.Store.DB.Exec("DELETE FROM capacity_reservations WHERE run_id=?", t.ID)
		}
	}()
	for _, allocation := range plan.Shards {
		repo := repos[allocation.RepositoryID]
		// A failed initialization leaves status "new" on an already persisted row.
		// Only persist newly allocated repositories; retries must reuse their secrets.
		if repo.Version == 0 {
			v, err := s.rt.Store.Save(ctx, "repositories", repo.ID, 0, repo)
			if err != nil {
				return err
			}
			repo.Version = v
			repos[repo.ID] = repo
		}
	}
	stage, err := os.MkdirTemp(s.rt.Config.CacheDir, "manifest-dirs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	point := Point{ID: uid(), PlanID: p.ID, RunID: t.ID, Name: p.Name, StartedAt: t.StartedAt, FileCount: plan.FileCount, LogicalBytes: plan.LogicalBytes, Snapshots: []Snapshot{}, Status: "committing"}
	s.stage(t, "backing_up")
	for i, a := range plan.Shards {
		if err = s.rt.CheckCacheBudget(); err != nil {
			return fail("CACHE_LIMIT_EXCEEDED", err.Error())
		}
		repo := repos[a.RepositoryID]
		t.progressBase = float64(i) / float64(len(plan.Shards)+1)
		t.progressWeight = 1 / float64(len(plan.Shards)+1)
		t.processedBase = t.ProcessedBytes
		account := accounts[repo.AccountID]
		unlockAccount := s.lock("account:" + account.ID)
		unlockRepo := s.lock("repo:" + repo.ID)
		snap, err := s.backupShard(ctx, t, p, repo, account, sources, stage)
		unlockRepo()
		unlockAccount()
		if err != nil {
			return err
		}
		repo.Status = "ready"
		for n := range repo.Locations {
			if repo.Locations[n].AccountID == account.ID && repo.Locations[n].Path == repo.Path {
				repo.Locations[n].Verified = true
			}
		}
		_, err = s.rt.Store.Save(ctx, "repositories", repo.ID, -1, repo)
		if err != nil {
			return err
		}
		point.Snapshots = append(point.Snapshots, Snapshot{RepositoryID: repo.ID, SnapshotID: snap})
		t.Progress = float64(i+1) / float64(len(plan.Shards)+1)
		s.save(t)
	}
	// Historical empty shards are explicit and never borrow their preceding snapshot.
	used := map[string]bool{}
	for _, snap := range point.Snapshots {
		used[snap.RepositoryID] = true
	}
	for id := range repos {
		if !used[id] {
			point.Snapshots = append(point.Snapshots, Snapshot{RepositoryID: id, Empty: true})
		}
	}
	if err = s.rt.CheckCacheBudget(); err != nil {
		return fail("CACHE_LIMIT_EXCEEDED", err.Error())
	}
	point.FinishedAt = now()
	s.stage(t, "committing")
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	point.Status = "completed"
	point.CommitID = uid()
	commitErr := s.commit(ctx, t, pool, &point, nil)
	if commitErr != nil {
		var f *failure
		if !errors.As(commitErr, &f) || f.Code != "PROTECTION_DEGRADED" {
			return commitErr
		}
	}
	v, err := s.rt.Store.Save(ctx, "recovery_points", point.ID, 0, point)
	if err != nil {
		return err
	}
	point.Version = v
	_, err = s.rt.Store.DB.ExecContext(ctx, "INSERT INTO assignments(plan_id,source_id,path,repo_id,size,mtime_ns) SELECT ?,source_id,path,repo_id,size,mtime_ns FROM file_entries WHERE run_id=? ON CONFLICT(plan_id,source_id,path) DO UPDATE SET repo_id=excluded.repo_id,size=excluded.size,mtime_ns=excluded.mtime_ns", p.ID, t.ID)
	if err != nil {
		return err
	}
	t.Result = map[string]any{"point_id": point.ID, "commit_id": point.CommitID, "planning": plan}
	_ = s.rt.Store.Audit(ctx, "backup.commit", point.ID)
	return commitErr
}
func (s *Service) repoAddress(repo Repository, a Account) string {
	if a.Backend == "local" {
		return filepath.Join(a.LocalPath, filepath.FromSlash(repo.Path))
	}
	return "rclone:" + a.Remote + ":" + repo.Path
}
func (s *Service) restic(ctx context.Context, t *Task, repo Repository, a Account, args []string, onLine func(string)) ([]byte, error) {
	sealed, err := base64.StdEncoding.DecodeString(repo.Password)
	if err != nil {
		return nil, err
	}
	password, err := s.rt.Keys.Open(sealed)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(s.rt.Config.CacheDir, "repository-secret-")
	if err != nil {
		return nil, err
	}
	name := file.Name()
	defer os.Remove(name)
	if err = file.Chmod(0600); err != nil {
		file.Close()
		return nil, err
	}
	if _, err = file.Write(password); err != nil {
		file.Close()
		return nil, err
	}
	file.Close()
	for i := range password {
		password[i] = 0
	}
	full := []string{"--repo", s.repoAddress(repo, a), "--password-file", name, "--cache-dir", filepath.Join(s.rt.Config.CacheDir, "restic"), "--json"}
	var env []string
	if a.Backend != "local" {
		config, err := s.rt.RcloneConfigFor(ctx, a.ID)
		if err != nil {
			return nil, fail("RCLONE_CONFIG_FAILED", "无法准备 Rclone 凭据："+err.Error())
		}
		defer s.rt.ReleaseRcloneConfig(config)
		// Inherit the config path in Rclone's subprocess without nested command quoting.
		env = append(env, "RCLONE_CONFIG="+config)
		full = append(full, "-o", "rclone.program="+s.rt.Config.RcloneBin, "-o", "rclone.args=serve restic --stdio")
	}
	full = append(full, args...)
	output, err := process.Run(ctx, s.rt.Config.ResticBin, full, env, onLine)
	if a.Backend != "local" {
		_ = s.rt.SyncRcloneCredentials()
		s.rt.ObserveRcloneFailure(a.ID, output, err)
	}
	return output, err
}
func (s *Service) backupShard(ctx context.Context, t *Task, p Plan, repo Repository, a Account, sources map[string]Source, stage string) (string, error) {
	if repo.Status == "new" {
		if _, err := s.restic(ctx, t, repo, a, []string{"init"}, func(line string) { s.log(t, line) }); err != nil {
			if _, checkErr := s.restic(ctx, t, repo, a, []string{"cat", "config"}, nil); checkErr != nil {
				return "", fail("REPOSITORY_INIT_FAILED", "仓库初始化或已有仓库密码验证失败："+err.Error())
			}
		}
	}
	list, err := os.CreateTemp(s.rt.Config.CacheDir, "file-list-")
	if err != nil {
		return "", err
	}
	defer os.Remove(list.Name())
	defer list.Close()
	root := filepath.Join(stage, repo.ID)
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	offset := 0
	for {
		entries, err := s.entries(ctx, "run_id=? AND repo_id=?", []any{t.ID, repo.ID}, 512, offset)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			break
		}
		offset += len(entries)
		for _, e := range entries {
			path := filepath.Join(sources[e.SourceID].Path, filepath.FromSlash(e.Path))
			if e.Kind == "dir" {
				path = filepath.Join(root, e.SourceID, filepath.FromSlash(e.Path))
				if err = os.MkdirAll(path, 0700); err != nil {
					return "", err
				}
			} else {
				info, err := os.Lstat(path)
				if err != nil || e.Kind == "file" && !info.Mode().IsRegular() || e.Kind == "symlink" && info.Mode()&os.ModeSymlink == 0 {
					return "", fail("SOURCE_CHANGED", "扫描后源类型变化："+path)
				}
				if _, err = list.Write(append([]byte(path), 0)); err != nil {
					return "", err
				}
			}
			if _, err = s.rt.Store.DB.ExecContext(ctx, "UPDATE file_entries SET snapshot_path=? WHERE run_id=? AND source_id=? AND path=?", filepath.ToSlash(path), t.ID, e.SourceID, e.Path); err != nil {
				return "", err
			}
		}
	}
	// Only the private metadata tree is traversed recursively; business directories are never roots.
	if _, err = list.Write(append([]byte(root), 0)); err != nil {
		return "", err
	}
	if err = list.Close(); err != nil {
		return "", err
	}
	offset = 0
	for {
		entries, err := s.entries(ctx, "run_id=? AND repo_id=? AND kind=\x27dir\x27", []any{t.ID, repo.ID}, 512, offset)
		if err != nil {
			return "", err
		}
		if len(entries) == 0 {
			break
		}
		offset += len(entries)
		for _, e := range entries {
			path := filepath.FromSlash(e.SnapshotPath)
			// Keep the private staging tree at 0700. Source permissions can rely
			// on a different owner/group or ACL; copying them here can prevent
			// traversal, Restic reads and cleanup. The encrypted manifest retains
			// the original mode for restore's final directory metadata pass.
			at := time.Unix(0, e.MtimeNS)
			if err = os.Chtimes(path, at, at); err != nil {
				return "", err
			}
		}
	}
	args := []string{"backup", "--files-from-raw", list.Name(), "--tag", "vaultpilot-run:" + t.ID, "--tag", "vaultpilot-repository:" + repo.ID}
	if p.BandwidthKiB > 0 {
		args = append(args, "--limit-upload", fmt.Sprint(p.BandwidthKiB))
	}
	snapshot := ""
	lastSave := time.Time{}
	_, err = s.restic(ctx, t, repo, a, args, func(line string) {
		var message struct {
			Type            string  `json:"message_type"`
			SnapshotID      string  `json:"snapshot_id"`
			DataAdded       int64   `json:"data_added"`
			DataAddedPacked int64   `json:"data_added_packed"`
			Percent         float64 `json:"percent_done"`
			BytesDone       int64   `json:"bytes_done"`
		}
		if json.Unmarshal([]byte(line), &message) == nil {
			if message.SnapshotID != "" {
				snapshot = message.SnapshotID
				if message.DataAddedPacked > 0 {
					t.UploadedBytes += message.DataAddedPacked
				} else {
					t.UploadedBytes += message.DataAdded
				}
			}
			if message.Type == "status" {
				percent := message.Percent
				if percent < 0 {
					percent = 0
				}
				if percent > 1 {
					percent = 1
				}
				t.Progress = t.progressBase + percent*t.progressWeight
				t.ProcessedBytes = t.processedBase + message.BytesDone
				if time.Since(lastSave) > 500*time.Millisecond {
					_ = s.save(t)
					lastSave = time.Now()
				}
			}
			if message.Type == "error" {
				s.log(t, line)
			}
		} else {
			s.log(t, line)
		}
	})
	status := map[string]any{"id": uid(), "run_id": t.ID, "repository_id": repo.ID, "snapshot_id": snapshot, "status": "failed"}
	if err == nil {
		status["status"] = "completed"
	}
	_, _ = s.rt.Store.Save(context.Background(), "shard_runs", status["id"].(string), 0, status)
	if err != nil {
		if process.ExitCode(err) == 3 {
			return "", fail("SOURCE_READ_FAILED", "Restic报告源文件读取失败，不发布完整恢复点")
		}
		return "", &failure{Code: "BACKUP_ENGINE_FAILED", Message: "分片备份失败：" + err.Error(), Retryable: process.ExitCode(err) != 3}
	}
	if len(snapshot) != 64 {
		return "", fail("SNAPSHOT_MISSING", "Restic 未返回完整快照 ID")
	}
	if err = s.verifySnapshot(ctx, t, repo, a, snapshot, root); err != nil {
		return "", err
	}
	return snapshot, nil
}
func (s *Service) verifySnapshot(ctx context.Context, t *Task, repo Repository, a Account, snapshot, stageRoot string) error {
	// Stream engine output into an isolated SQL index, then prove each planned path is present.
	_, err := s.rt.Store.DB.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS engine_snapshot_entries_v2(task_id TEXT,repo_id TEXT,path TEXT,kind TEXT,size INTEGER,mtime_ns INTEGER,link_target TEXT,PRIMARY KEY(task_id,repo_id,path))")
	if err != nil {
		return err
	}
	_, _ = s.rt.Store.DB.ExecContext(ctx, "DELETE FROM engine_snapshot_entries_v2 WHERE task_id=? AND repo_id=?", t.ID, repo.ID)
	var callbackErr error
	_, err = s.restic(ctx, t, repo, a, []string{"ls", snapshot}, func(line string) {
		if callbackErr != nil {
			return
		}
		var node struct {
			Type       string `json:"struct_type"`
			Path       string `json:"path"`
			Kind       string `json:"type"`
			Size       int64  `json:"size"`
			Mtime      string `json:"mtime"`
			LinkTarget string `json:"linktarget"`
		}
		if json.Unmarshal([]byte(line), &node) != nil || node.Type != "node" {
			return
		}
		_, callbackErr = s.rt.Store.DB.ExecContext(ctx, "INSERT OR REPLACE INTO engine_snapshot_entries_v2(task_id,repo_id,path,kind,size,mtime_ns,link_target) VALUES(?,?,?,?,?,?,?)", t.ID, repo.ID, node.Path, node.Kind, node.Size, parseMtime(node.Mtime), node.LinkTarget)
	})
	if err != nil {
		return err
	}
	if callbackErr != nil {
		return callbackErr
	}
	var missing int
	if err = s.rt.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM file_entries f LEFT JOIN engine_snapshot_entries_v2 e ON e.task_id=f.run_id AND e.repo_id=f.repo_id AND e.path=f.snapshot_path WHERE f.run_id=? AND f.repo_id=? AND (e.path IS NULL OR e.kind<>CASE f.kind WHEN \x27symlink\x27 THEN \x27symlink\x27 ELSE f.kind END OR (f.kind=\x27file\x27 AND (e.size<>f.size OR e.mtime_ns<>f.mtime_ns)))", t.ID, repo.ID).Scan(&missing); err != nil {
		return err
	}
	if missing > 0 {
		return fail("SNAPSHOT_COVERAGE_MISSING", "快照缺少计划文件或目录")
	}
	var extra int
	err = s.rt.Store.DB.QueryRowContext(ctx, "SELECT count(*) FROM engine_snapshot_entries_v2 e WHERE e.task_id=? AND e.repo_id=? AND e.kind<>\x27dir\x27 AND NOT EXISTS(SELECT 1 FROM file_entries f WHERE f.run_id=e.task_id AND f.repo_id=e.repo_id AND f.snapshot_path=e.path)", t.ID, repo.ID).Scan(&extra)
	if err != nil {
		return err
	}
	if extra > 0 {
		return fail("SNAPSHOT_BOUNDARY_CHANGED", "快照包含未规划文件；源在扫描后变化，未发布恢复点")
	}
	if err = s.verifySnapshotLinks(ctx, t, repo, a, snapshot); err != nil {
		return err
	}
	_, _ = s.rt.Store.DB.ExecContext(ctx, "DELETE FROM engine_snapshot_entries_v2 WHERE task_id=? AND repo_id=?", t.ID, repo.ID)
	return nil
}

func parseMtime(value string) int64 {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return -1
	}
	return at.UnixNano()
}

// Restic ls JSON deliberately omits symbolic link targets, so verify these through a metadata-only selective restore.
func (s *Service) verifySnapshotLinks(ctx context.Context, t *Task, repo Repository, a Account, snapshot string) error {
	dir, err := os.MkdirTemp(s.rt.Config.CacheDir, "verify-links-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()
	offset := 0
	for {
		entries, err := s.entries(ctx, "run_id=? AND repo_id=? AND kind='symlink'", []any{t.ID, repo.ID}, 128, offset)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		offset += len(entries)
		args := []string{"restore", snapshot, "--target", dir}
		for _, e := range entries {
			args = append(args, "--include", globLiteral(e.SnapshotPath))
		}
		if _, err = s.restic(ctx, t, repo, a, args, nil); err != nil {
			return fail("SNAPSHOT_LINK_VERIFY_FAILED", "无法验证快照中的符号链接")
		}
		for _, e := range entries {
			relative, err := sourceSnapshotRelative(e.SnapshotPath)
			if err != nil {
				return err
			}
			link, err := root.Readlink(relative)
			if err != nil || link != e.LinkTarget {
				return fail("SOURCE_CHANGED", "扫描后符号链接目标发生变化："+e.Path)
			}
			if err = root.Remove(relative); err != nil {
				return err
			}
		}
	}
}

func (s *Service) rcloneOutput(ctx context.Context, accounts []Account, args ...string) ([]byte, error) {
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	config, err := s.rt.RcloneConfigFor(ctx, ids...)
	if err != nil {
		return nil, fail("RCLONE_CONFIG_FAILED", "无法准备 Rclone 凭据："+err.Error())
	}
	defer s.rt.ReleaseRcloneConfig(config)
	output, err := process.Output(ctx, s.rt.Config.RcloneBin, append([]string{"--config", config}, args...), nil)
	s.observeRcloneAccounts(accounts, output, err)
	return output, err
}
func (s *Service) rcloneRun(ctx context.Context, accounts []Account, onLine func(string), args ...string) ([]byte, error) {
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	config, err := s.rt.RcloneConfigFor(ctx, ids...)
	if err != nil {
		return nil, fail("RCLONE_CONFIG_FAILED", "无法准备 Rclone 凭据："+err.Error())
	}
	defer s.rt.ReleaseRcloneConfig(config)
	output, err := process.Run(ctx, s.rt.Config.RcloneBin, append([]string{"--config", config}, args...), nil, onLine)
	s.observeRcloneAccounts(accounts, output, err)
	return output, err
}

func (s *Service) observeRcloneAccounts(accounts []Account, output []byte, err error) {
	if err == nil {
		return
	}
	matched := []Account{}
	seen := map[string]bool{}
	for _, a := range accounts {
		if a.Backend == "onedrive" && !seen[a.ID] {
			seen[a.ID] = true
			matched = append(matched, a)
		}
	}
	// A two-remote copy can fail on either side. Without remote attribution in
	// its output, let the individual health probes identify the affected account.
	for _, a := range matched {
		if len(matched) == 1 || strings.Contains(string(output), a.Remote+":") {
			s.rt.ObserveRcloneFailure(a.ID, output, err)
		}
	}
}
