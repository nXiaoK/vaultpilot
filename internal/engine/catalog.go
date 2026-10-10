package engine

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"vaultpilot/internal/platform"
)

type catalogControl struct {
	Format       int          `json:"format"`
	VaultID      string       `json:"vault_id"`
	CommitID     string       `json:"commit_id"`
	CreatedAt    string       `json:"created_at"`
	Replicas     int          `json:"replicas"`
	Repositories []Repository `json:"repositories"`
	Points       []Point      `json:"points"`
	Sources      []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"sources"`
	DeletedIDs []string `json:"deleted_ids,omitempty"`
}
type catalogMarker struct {
	PreparedReplicas int      `json:"prepared_replicas"`
	State            string   `json:"state"`
	Format           int      `json:"format"`
	VaultID          string   `json:"vault_id"`
	CommitID         string   `json:"commit_id"`
	SHA256           string   `json:"sha256"`
	Drives           []string `json:"drives"`
	CreatedAt        string   `json:"created_at"`
}
type sealWriter struct {
	out  io.Writer
	keys *platform.Keyring
	buf  []byte
}

func (w *sealWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		room := (1 << 20) - len(w.buf)
		if room > len(p) {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
		p = p[room:]
		if len(w.buf) == 1<<20 {
			if err := w.flush(); err != nil {
				return n - len(p), err
			}
		}
	}
	return n, nil
}
func (w *sealWriter) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	sealed, err := w.keys.Seal(w.buf)
	if err != nil {
		return err
	}
	if err = binary.Write(w.out, binary.BigEndian, uint32(len(sealed))); err != nil {
		return err
	}
	if _, err = w.out.Write(sealed); err != nil {
		return err
	}
	w.buf = w.buf[:0]
	return nil
}
func sealFile(keys *platform.Keyring, plain, encrypted string) error {
	in, err := os.Open(plain)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(encrypted, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err = out.Write([]byte("VPC1\n")); err != nil {
		return err
	}
	writer := &sealWriter{out: out, keys: keys}
	if _, err = io.Copy(writer, in); err != nil {
		return err
	}
	if err = writer.flush(); err != nil {
		return err
	}
	return out.Sync()
}
func openCatalog(keys *platform.Keyring, encrypted, plain string) error {
	in, err := os.Open(encrypted)
	if err != nil {
		return err
	}
	defer in.Close()
	magic := make([]byte, 5)
	if _, err = io.ReadFull(in, magic); err != nil || string(magic) != "VPC1\n" {
		return fail("CATALOG_FORMAT", "控制目录格式不受支持")
	}
	out, err := os.OpenFile(plain, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	for {
		var n uint32
		if err = binary.Read(in, binary.BigEndian, &n); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		if n > 2<<20 || n < 28 {
			return fail("CATALOG_CORRUPT", "控制目录认证块损坏")
		}
		sealed := make([]byte, n)
		if _, err = io.ReadFull(in, sealed); err != nil {
			return err
		}
		plain, err := keys.Open(sealed)
		if err != nil {
			return fail("CATALOG_AUTH_FAILED", "控制目录认证失败")
		}
		if _, err = out.Write(plain); err != nil {
			return err
		}
	}
	return out.Sync()
}
func fileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func (s *Service) makeCatalog(ctx context.Context, commitID string, replicas int, newPoint *Point, deleted []string, path string) error {
	control := catalogControl{Format: 1, VaultID: s.rt.VaultID(), CommitID: commitID, CreatedAt: now(), Replicas: replicas, Points: []Point{}, Repositories: []Repository{}, DeletedIDs: deleted}
	raws, err := s.rt.Store.All(ctx, "repositories")
	if err != nil {
		return err
	}
	for _, raw := range raws {
		var repo Repository
		if err = json.Unmarshal(raw, &repo); err != nil {
			return err
		}
		control.Repositories = append(control.Repositories, repo)
	}
	raws, err = s.rt.Store.All(ctx, "recovery_points")
	if err != nil {
		return err
	}
	deletedSet := map[string]bool{}
	for _, id := range deleted {
		deletedSet[id] = true
	}
	for _, raw := range raws {
		var p Point
		if err = json.Unmarshal(raw, &p); err != nil {
			return err
		}
		if deletedSet[p.ID] {
			p.DeletedAt = control.CreatedAt
			p.Status = "deleted"
		}
		if newPoint != nil && newPoint.ID == p.ID {
			continue
		}
		control.Points = append(control.Points, p)
	}
	if newPoint != nil {
		control.Points = append(control.Points, *newPoint)
	}
	raws, err = s.rt.Store.All(ctx, "sources")
	if err != nil {
		return err
	}
	for _, raw := range raws {
		var src Source
		_ = json.Unmarshal(raw, &src)
		control.Sources = append(control.Sources, struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}{src.ID, src.Name})
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	z := zip.NewWriter(f)
	entry, err := z.Create("control.json")
	if err != nil {
		return err
	}
	if err = json.NewEncoder(entry).Encode(control); err != nil {
		return err
	}
	for _, p := range control.Points {
		if p.DeletedAt != "" {
			continue
		}
		entry, err = z.Create("points/" + p.ID + ".jsonl")
		if err != nil {
			return err
		}
		encoder := json.NewEncoder(entry)
		rows, err := s.rt.Store.DB.QueryContext(ctx, "SELECT source_id,path,kind,size,mtime_ns,mode,link_target,repo_id,snapshot_path FROM file_entries WHERE run_id=? ORDER BY source_id,path", p.RunID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var e Entry
			if err = rows.Scan(&e.SourceID, &e.Path, &e.Kind, &e.Size, &e.MtimeNS, &e.Mode, &e.LinkTarget, &e.RepositoryID, &e.SnapshotPath); err != nil {
				rows.Close()
				return err
			}
			if err = encoder.Encode(e); err != nil {
				rows.Close()
				return err
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	if err = z.Close(); err != nil {
		return err
	}
	return f.Sync()
}
func (s *Service) remoteFile(a Account, relative string) string {
	if a.Backend == "local" {
		return filepath.Join(a.LocalPath, filepath.FromSlash(relative))
	}
	return a.Remote + ":" + relative
}
func (s *Service) putFile(ctx context.Context, a Account, local, relative string) error {
	if a.Backend == "local" {
		target := s.remoteFile(a, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(target), ".catalog-")
		if err != nil {
			return err
		}
		name := tmp.Name()
		defer os.Remove(name)
		defer tmp.Close()
		in, err := os.Open(local)
		if err != nil {
			return err
		}
		defer in.Close()
		if _, err = io.Copy(tmp, in); err != nil {
			return err
		}
		if err = tmp.Sync(); err != nil {
			return err
		}
		if err = tmp.Close(); err != nil {
			return err
		}
		return os.Rename(name, target)
	}
	_, err := s.rcloneOutput(ctx, s.rcloneArgs("copyto", local, s.remoteFile(a, relative)))
	_ = s.rt.SyncRcloneCredentials()
	return err
}
func (s *Service) getFile(ctx context.Context, a Account, relative, local string) error {
	if a.Backend == "local" {
		in, err := os.Open(s.remoteFile(a, relative))
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(local, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	}
	_, err := s.rcloneOutput(ctx, s.rcloneArgs("copyto", s.remoteFile(a, relative), local))
	_ = s.rt.SyncRcloneCredentials()
	return err
}
func (s *Service) commit(ctx context.Context, t *Task, pool Pool, newPoint *Point, deleted []string) error {
	commitID := uid()
	if newPoint != nil {
		commitID = newPoint.CommitID
	}
	replicas := pool.CatalogReplicas
	if replicas < 1 {
		replicas = 2
	}
	dir, err := os.MkdirTemp(s.rt.Config.CacheDir, "catalog-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	plain := filepath.Join(dir, "catalog.zip")
	encrypted := filepath.Join(dir, "catalog.vpc")
	if err = s.makeCatalog(ctx, commitID, replicas, newPoint, deleted, plain); err != nil {
		return err
	}
	if err = sealFile(s.rt.Keys, plain, encrypted); err != nil {
		return err
	}
	if err = s.rt.CheckCacheBudget(); err != nil {
		return fail("CACHE_LIMIT_EXCEEDED", err.Error())
	}
	digest, err := fileDigest(encrypted)
	if err != nil {
		return err
	}
	accounts := []Account{}
	drives := map[string]bool{}
	for _, id := range pool.AccountIDs {
		var a Account
		if err = s.rt.Store.Get(ctx, "accounts", id, &a); err != nil {
			return err
		}
		key := a.Region + ":" + a.TenantID + ":" + a.DriveID
		if !a.Verified || !a.Enabled || a.DriveID == "" || drives[key] {
			continue
		}
		drives[key] = true
		accounts = append(accounts, a)
	}
	if len(accounts) < replicas {
		return fail("CATALOG_REPLICAS_INSUFFICIENT", "控制目录需要至少 "+fmt.Sprint(replicas)+" 个不同 Drive，当前不足")
	}
	verified := []Account{}
	relative := "VaultPilot/" + s.rt.VaultID() + "/catalog/" + commitID + ".vpc"
	for _, a := range accounts {
		if err = s.putFile(ctx, a, encrypted, relative); err != nil {
			s.log(t, "控制目录上传失败："+a.Name)
			continue
		}
		copy := filepath.Join(dir, "verify-"+a.ID)
		if err = s.getFile(ctx, a, relative, copy); err != nil {
			s.log(t, "控制目录读回失败："+a.Name)
			continue
		}
		remoteDigest, err := fileDigest(copy)
		if err != nil || remoteDigest != digest {
			s.log(t, "控制目录读回不匹配："+a.Name)
			continue
		}
		verified = append(verified, a)
		if len(verified) >= replicas {
			break
		}
	}
	if len(verified) < replicas {
		return fail("CATALOG_COMMIT_INCOMPLETE", "数据快照已上传，但控制目录副本未满足要求；未发布完整恢复点")
	}
	marker := catalogMarker{Format: 1, VaultID: s.rt.VaultID(), CommitID: commitID, SHA256: digest, CreatedAt: now()}
	for _, a := range verified {
		marker.Drives = append(marker.Drives, a.DriveID)
	}
	data, _ := json.Marshal(marker)
	sealed, err := s.rt.Keys.Seal(data)
	if err != nil {
		return err
	}
	markerPath := filepath.Join(dir, "commit.ok")
	if err = os.WriteFile(markerPath, sealed, 0600); err != nil {
		return err
	}
	// Two-phase metadata commit: all catalog replicas and all prepare receipts must be verified before a completion certificate exists.
	prepareCount := 0
	for _, a := range verified {
		relative := strings.TrimSuffix(relative, ".vpc") + ".prepare"
		if err = s.putFile(ctx, a, markerPath, relative); err != nil {
			continue
		}
		check := filepath.Join(dir, "prepare-"+a.ID)
		if err = s.getFile(ctx, a, relative, check); err != nil {
			continue
		}
		read, e := os.ReadFile(check)
		if e == nil && bytes.Equal(read, sealed) {
			if opened, e := s.rt.Keys.Open(read); e == nil && bytes.Equal(opened, data) {
				prepareCount++
			}
		}
	}
	if prepareCount < replicas {
		return fail("CATALOG_COMMIT_INCOMPLETE", "控制目录准备记录未满足副本要求")
	}
	// A certificate is only created after the required replicas have acknowledged the prepared content.
	certificate := map[string]any{"format": 1, "vault_id": marker.VaultID, "commit_id": marker.CommitID, "sha256": marker.SHA256, "drives": marker.Drives, "created_at": marker.CreatedAt, "prepared_replicas": prepareCount, "state": "committed"}
	certificateJSON, _ := json.Marshal(certificate)
	certificateBytes, err := s.rt.Keys.Seal(certificateJSON)
	if err != nil {
		return err
	}
	if err = os.WriteFile(markerPath, certificateBytes, 0600); err != nil {
		return err
	}
	okCount := 0
	for _, a := range verified {
		markerRelative := strings.TrimSuffix(relative, ".vpc") + ".ok"
		if err = s.putFile(ctx, a, markerPath, markerRelative); err != nil {
			continue
		}
		check := filepath.Join(dir, "ok-"+a.ID)
		if err = s.getFile(ctx, a, markerRelative, check); err != nil {
			continue
		}
		read, e := os.ReadFile(check)
		if e == nil && bytes.Equal(read, certificateBytes) {
			if opened, e := s.rt.Keys.Open(read); e == nil && bytes.Equal(opened, certificateJSON) {
				okCount++
			}
		}
	}
	if okCount == 0 {
		return fail("CATALOG_COMMIT_INCOMPLETE", "控制目录完成证书未持久化")
	}
	if okCount < replicas {
		if newPoint != nil {
			newPoint.ProtectionStatus = "degraded"
		}
		return fail("PROTECTION_DEGRADED", "控制目录已逻辑提交，但完成证书副本未达到要求；可以恢复但未达到完整保护")
	}
	if newPoint != nil {
		newPoint.ProtectionStatus = "complete"
	}
	_ = s.rt.Store.SetMeta(ctx, "catalog_last", commitID)
	return nil
}
func (s *Service) discover(ctx context.Context, t *Task) error {
	s.catalogMu.Lock()
	defer s.catalogMu.Unlock()
	s.stage(t, "discovering")
	raws, err := s.rt.Store.All(ctx, "accounts")
	if err != nil {
		return err
	}
	accounts := []Account{}
	for _, raw := range raws {
		var a Account
		_ = json.Unmarshal(raw, &a)
		if a.Verified && a.Enabled {
			accounts = append(accounts, a)
		}
	}
	dir, err := os.MkdirTemp(s.rt.Config.CacheDir, "discover-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	var best *catalogControl
	var bestZip string
	var bestReceipt []byte
	repoAccounts := map[string]Account{}
	for _, a := range accounts {
		repoAccounts[a.Region+":"+a.TenantID+":"+a.DriveID] = a
	}
	base := "VaultPilot/" + s.rt.VaultID() + "/catalog/"
	for _, a := range accounts {
		names := []string{}
		if a.Backend == "local" {
			entries, e := os.ReadDir(s.remoteFile(a, base))
			if e != nil {
				continue
			}
			for _, e := range entries {
				names = append(names, e.Name())
			}
		} else {
			out, e := s.rcloneOutput(ctx, s.rcloneArgs("lsf", s.remoteFile(a, base), "--files-only"))
			_ = s.rt.SyncRcloneCredentials()
			if e != nil {
				continue
			}
			names = strings.Split(strings.TrimSpace(string(out)), "\n")
		}
		sort.Strings(names)
		for _, name := range names {
			if !strings.HasSuffix(name, ".ok") || strings.ContainsAny(name, "/\\") {
				continue
			}
			id := strings.TrimSuffix(name, ".ok")
			if len(id) != 32 {
				continue
			}
			markerPath := filepath.Join(dir, uid()+".ok")
			if err = s.getFile(ctx, a, base+name, markerPath); err != nil {
				continue
			}
			sealed, e := os.ReadFile(markerPath)
			if e != nil {
				continue
			}
			opened, e := s.rt.Keys.Open(sealed)
			if e != nil {
				continue
			}
			var marker catalogMarker
			if json.Unmarshal(opened, &marker) != nil || marker.Format != 1 || marker.VaultID != s.rt.VaultID() || marker.CommitID != id || marker.State != "committed" || marker.PreparedReplicas < 1 {
				continue
			}
			if best != nil && marker.CreatedAt <= best.CreatedAt {
				continue
			}
			encrypted := filepath.Join(dir, uid()+".vpc")
			if err = s.getFile(ctx, a, base+id+".vpc", encrypted); err != nil {
				continue
			}
			digest, e := fileDigest(encrypted)
			if e != nil || digest != marker.SHA256 {
				continue
			}
			plain := filepath.Join(dir, uid()+".zip")
			if err = openCatalog(s.rt.Keys, encrypted, plain); err != nil {
				continue
			}
			z, e := zip.OpenReader(plain)
			if e != nil {
				continue
			}
			var control catalogControl
			valid := false
			for _, file := range z.File {
				if file.Name == "control.json" {
					f, e := file.Open()
					if e == nil {
						e = json.NewDecoder(io.LimitReader(f, 32<<20)).Decode(&control)
						f.Close()
						valid = e == nil
					}
					break
				}
			}
			z.Close()
			if !valid || control.Format != 1 || control.VaultID != s.rt.VaultID() || control.CommitID != id || len(marker.Drives) < control.Replicas || marker.PreparedReplicas < control.Replicas {
				continue
			}
			control.CreatedAt = marker.CreatedAt
			best = &control
			bestZip = plain
			bestReceipt = append([]byte{}, sealed...)
		}
	}
	if best == nil {
		return fail("CATALOG_NOT_FOUND", "未发现已认证提交的控制目录；请接入恢复包所列的正确 Drive")
	}
	verifiedCertificates := 0
	seenCertificateDrives := map[string]bool{}
	for _, account := range accounts {
		if seenCertificateDrives[account.DriveID] {
			continue
		}
		path := filepath.Join(dir, uid()+"-receipt.ok")
		if s.getFile(ctx, account, base+best.CommitID+".ok", path) != nil {
			continue
		}
		read, e := os.ReadFile(path)
		if e == nil && bytes.Equal(read, bestReceipt) {
			verifiedCertificates++
			seenCertificateDrives[account.DriveID] = true
		}
	}
	protection := "complete"
	if verifiedCertificates < best.Replicas {
		protection = "degraded"
		s.log(t, "当前可验证完成证书副本少于配置要求；恢复点保护降级，请检查其他 Drive")
	}
	s.stage(t, "rebuilding_index")
	z, err := zip.OpenReader(bestZip)
	if err != nil {
		return err
	}
	defer z.Close()
	files := map[string]*zip.File{}
	for _, f := range z.File {
		files[f.Name] = f
	}
	for _, repo := range best.Repositories {
		found := false
		for i := len(repo.Locations) - 1; i >= 0; i-- {
			loc := repo.Locations[i]
			a, ok := repoAccounts[loc.Region+":"+loc.TenantID+":"+loc.DriveID]
			if ok && loc.Verified {
				repo.AccountID = a.ID
				repo.Path = loc.Path
				found = true
				break
			}
		}
		if !found {
			repo.AccountID = ""
			repo.Status = "account_missing"
		}
		if _, err = s.rt.Store.Save(ctx, "repositories", repo.ID, -1, repo); err != nil {
			return err
		}
	}
	for _, point := range best.Points {
		point.ProtectionStatus = protection
		point.RunID = "import-" + uid()
		if point.DeletedAt != "" {
			_, err = s.rt.Store.Save(ctx, "recovery_points", point.ID, -1, point)
			if err != nil {
				return err
			}
			continue
		}
		file := files["points/"+point.ID+".jsonl"]
		if file == nil {
			return fail("CATALOG_MANIFEST_MISSING", "控制目录缺少恢复点清单")
		}
		reader, err := file.Open()
		if err != nil {
			return err
		}
		_, err = s.rt.Store.DB.ExecContext(ctx, "DELETE FROM file_entries WHERE run_id=?", point.RunID)
		if err != nil {
			reader.Close()
			return err
		}
		decoder := json.NewDecoder(bufio.NewReader(reader))
		batch := []Entry{}
		flush := func() error {
			tx, e := s.rt.Store.DB.BeginTx(ctx, nil)
			if e != nil {
				return e
			}
			defer tx.Rollback()
			for _, entry := range batch {
				if !safeLogical(entry.Path) || !safeID(entry.SourceID) {
					return fail("CATALOG_PATH_INVALID", "清单路径无效")
				}
				_, e = tx.ExecContext(ctx, "INSERT INTO file_entries(run_id,source_id,path,kind,size,mtime_ns,mode,link_target,repo_id,snapshot_path) VALUES(?,?,?,?,?,?,?,?,?,?)", point.RunID, entry.SourceID, entry.Path, entry.Kind, entry.Size, entry.MtimeNS, entry.Mode, entry.LinkTarget, entry.RepositoryID, entry.SnapshotPath)
				if e != nil {
					return e
				}
			}
			batch = batch[:0]
			return tx.Commit()
		}
		for {
			var entry Entry
			e := decoder.Decode(&entry)
			if errors.Is(e, io.EOF) {
				break
			}
			if e != nil {
				reader.Close()
				return e
			}
			batch = append(batch, entry)
			if len(batch) == 512 {
				if err = flush(); err != nil {
					reader.Close()
					return err
				}
			}
		}
		if len(batch) > 0 {
			err = flush()
		}
		reader.Close()
		if err != nil {
			return err
		}
		var fileCount, logicalBytes int64
		if err = s.rt.Store.DB.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(size),0) FROM file_entries WHERE run_id=?", point.RunID).Scan(&fileCount, &logicalBytes); err != nil {
			return err
		}
		if fileCount != point.FileCount || logicalBytes != point.LogicalBytes {
			return fail("CATALOG_COVERAGE_MISMATCH", "控制目录文件数量或大小与恢复点不符")
		}
		if _, err = s.rt.Store.Save(ctx, "recovery_points", point.ID, -1, point); err != nil {
			return err
		}
	}
	t.Result = map[string]any{"commit_id": best.CommitID, "recovery_points": len(best.Points), "repositories": len(best.Repositories), "discovered_at": time.Now().UTC(), "protection_status": protection, "certificate_replicas": verifiedCertificates}
	return s.rt.Store.Audit(ctx, "catalog.discover", best.CommitID)
}

// DecryptCatalog authenticates the encrypted block stream. Callers must verify the commit marker SHA-256 first.
func DecryptCatalog(keys *platform.Keyring, inputPath, outputZipPath string) error {
	return openCatalog(keys, inputPath, outputZipPath)
}
