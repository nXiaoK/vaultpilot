package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"vaultpilot/internal/platform"
	"vaultpilot/internal/process"
)

const cloudBootstrapName = "connect.vpkey"

func (s *Service) rcloneOutputBounded(ctx context.Context, accounts []Account, limit int, args ...string) ([]byte, error) {
	ids := make([]string, 0, len(accounts))
	for _, a := range accounts {
		ids = append(ids, a.ID)
	}
	config, err := s.rt.RcloneConfigFor(ctx, ids...)
	if err != nil {
		return nil, fail("RCLONE_CONFIG_FAILED", "无法准备 Rclone 凭据："+err.Error())
	}
	defer s.rt.ReleaseRcloneConfig(config)
	output, err := process.OutputLimited(ctx, s.rt.Config.RcloneBin, append([]string{"--config", config}, args...), nil, limit)
	s.observeRcloneAccounts(accounts, output, err)
	return output, err
}

func cloudVaultID(id string) bool {
	b, err := hex.DecodeString(id)
	return err == nil && len(b) == 16 && id == strings.ToLower(id)
}

func (s *Service) cloudAccounts(ctx context.Context) ([]Account, error) {
	raws, err := s.rt.Store.All(ctx, "accounts")
	if err != nil {
		return nil, err
	}
	accounts := []Account{}
	for _, raw := range raws {
		var a Account
		if json.Unmarshal(raw, &a) == nil && a.Verified && a.Enabled {
			accounts = append(accounts, a)
		}
	}
	sort.Slice(accounts, func(i, j int) bool { return accounts[i].ID < accounts[j].ID })
	return accounts, nil
}

// Bootstrap reads are bounded before parsing or running any password KDF.
func (s *Service) readCloudBootstrap(ctx context.Context, a Account, vault string) ([]byte, error) {
	if !cloudVaultID(vault) {
		return nil, fail("INVALID_VAULT", "备份库标识无效")
	}
	path := "VaultPilot/" + vault + "/" + cloudBootstrapName
	var raw []byte
	var err error
	if a.Backend == "local" {
		root, e := os.OpenRoot(a.LocalPath)
		if e != nil {
			return nil, e
		}
		defer root.Close()
		f, e := root.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		info, e := f.Stat()
		if e != nil || !info.Mode().IsRegular() || info.Size() > platform.CloudBootstrapMaxBytes {
			return nil, fail("CLOUD_BOOTSTRAP_INVALID", "云端连接资料必须是大小受限的普通文件")
		}
		raw, err = io.ReadAll(io.LimitReader(f, platform.CloudBootstrapMaxBytes+1))
	} else {
		stat, e := s.rcloneOutputBounded(ctx, []Account{a}, 4096, "lsjson", s.remoteFile(a, path), "--stat")
		if e != nil {
			return nil, e
		}
		var info struct {
			IsDir bool  `json:"IsDir"`
			Size  int64 `json:"Size"`
		}
		if json.Unmarshal(stat, &info) != nil || info.IsDir || info.Size < 0 || info.Size > platform.CloudBootstrapMaxBytes {
			return nil, fail("CLOUD_BOOTSTRAP_INVALID", "云端连接资料必须是大小受限的普通文件")
		}
		raw, err = s.rcloneOutputBounded(ctx, []Account{a}, platform.CloudBootstrapMaxBytes+1, "cat", s.remoteFile(a, path), "--head", fmt.Sprint(platform.CloudBootstrapMaxBytes+1))
		_ = s.rt.SyncRcloneCredentials()
	}
	if err != nil {
		return nil, err
	}
	if len(raw) > platform.CloudBootstrapMaxBytes {
		return nil, fail("CLOUD_BOOTSTRAP_INVALID", "云端连接资料超过大小限制")
	}
	return raw, nil
}

func (s *Service) writeCloudBootstrap(ctx context.Context, a Account, raw []byte) error {
	dir, err := os.MkdirTemp(s.rt.Config.CacheDir, "cloud-connect-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "connect.vpkey")
	if err = os.WriteFile(file, raw, 0600); err != nil {
		return err
	}
	if err = s.putFile(ctx, a, file, "VaultPilot/"+s.rt.VaultID()+"/"+cloudBootstrapName); err != nil {
		return err
	}
	read, err := s.readCloudBootstrap(ctx, a, s.rt.VaultID())
	if err != nil {
		return err
	}
	if !bytes.Equal(read, raw) {
		return fail("CLOUD_BOOTSTRAP_VERIFY_FAILED", "云端连接入口读回校验失败")
	}
	return nil
}

func (s *Service) syncCloudBootstrap(ctx context.Context, a Account) error {
	raw, err := s.rt.Store.Meta(ctx, "cloud_bootstrap")
	if raw == "" {
		return nil
	}
	if err != nil {
		return err
	}
	return s.writeCloudBootstrap(ctx, a, []byte(raw))
}

func (s *Service) cloudState(w http.ResponseWriter, r *http.Request) {
	value := map[string]any{"pending": s.rt.CloudPending(), "recovery": s.rt.RecoveryMode(), "interval_seconds": 60}
	for _, key := range []string{"cloud_bootstrap", "cloud_auto_discover", "cloud_last_checked", "cloud_last_success", "cloud_last_error"} {
		v, _ := s.rt.Store.Meta(r.Context(), key)
		switch key {
		case "cloud_bootstrap":
			value["enabled"] = v != ""
		case "cloud_auto_discover":
			value["auto_discover"] = v == "1"
		default:
			value[strings.TrimPrefix(key, "cloud_")] = v
		}
	}
	write(w, 200, value)
}

func decodeCloudSecret(r *http.Request, out any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<10))
	if err := d.Decode(out); err != nil {
		return err
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return fail("INVALID_REQUEST", "请求只能包含一个 JSON 对象且不超过 8 KiB")
	}
	return nil
}

func (s *Service) cloudPublish(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	if !s.rt.ConsumeSecretAttempt(r, "cloud_secret") {
		write(w, 429, map[string]any{"code": "RATE_LIMITED", "error": "密码尝试过于频繁，请稍后重试"})
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := decodeCloudSecret(r, &req); err != nil {
		problem(w, err)
		return
	}
	raw, err := s.rt.BuildCloudBootstrap(req.Password)
	if err != nil {
		problem(w, fail("CLOUD_CONNECT_PASSWORD", err.Error()))
		return
	}
	s.rt.ResetSecretAttempts(r, "cloud_secret")
	accounts, err := s.cloudAccounts(r.Context())
	if err != nil {
		problem(w, err)
		return
	}
	if len(accounts) == 0 {
		problem(w, fail("ACCOUNT_REQUIRED", "请先授权并验证至少一个原备份账号"))
		return
	}
	unlock := s.lock("cloud-connect")
	defer unlock()
	verified, failed := []string{}, []string{}
	for _, a := range accounts {
		if err = s.writeCloudBootstrap(ctx, a, raw); err != nil {
			failed = append(failed, a.ID)
		} else {
			verified = append(verified, a.ID)
		}
	}
	if len(verified) == 0 {
		problem(w, fail("CLOUD_PUBLISH_FAILED", "连接入口上传或读回失败，请检查账号访问与网络"))
		return
	}
	if err = s.rt.Store.SetMeta(r.Context(), "cloud_bootstrap", string(raw)); err != nil {
		problem(w, err)
		return
	}
	_ = s.rt.Store.Audit(r.Context(), "cloud.connect.enable", s.rt.VaultID())
	write(w, 200, map[string]any{"enabled": true, "verified_accounts": verified, "failed_accounts": failed})
}

func (s *Service) cloudVaults(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	if !s.rt.CloudPending() {
		problem(w, fail("ALREADY_CONNECTED", "当前节点已连接备份库"))
		return
	}
	accounts, err := s.cloudAccounts(r.Context())
	if err != nil {
		problem(w, err)
		return
	}
	type candidate struct {
		VaultID    string   `json:"vault_id"`
		AccountIDs []string `json:"account_ids"`
	}
	items := map[string]*candidate{}
	failed := []string{}
	inspected := 0
	for _, a := range accounts {
		names := []string{}
		if a.Backend == "local" {
			entries, e := os.ReadDir(s.remoteFile(a, "VaultPilot"))
			if e != nil {
				failed = append(failed, a.ID)
				continue
			}
			for _, entry := range entries {
				if entry.IsDir() {
					names = append(names, entry.Name())
				}
			}
		} else {
			out, e := s.rcloneOutputBounded(ctx, []Account{a}, 64*1024, "lsf", s.remoteFile(a, "VaultPilot"), "--dirs-only")
			_ = s.rt.SyncRcloneCredentials()
			if e != nil {
				failed = append(failed, a.ID)
				continue
			}
			names = strings.Split(strings.TrimSpace(string(out)), "\n")
		}
		for _, name := range names {
			id := strings.TrimSuffix(name, "/")
			if !cloudVaultID(id) {
				continue
			}
			inspected++
			if inspected > 100 {
				problem(w, fail("TOO_MANY_VAULTS", "检查的备份库目录超过100个，请缩小授权账号范围"))
				return
			}
			raw, e := s.readCloudBootstrap(ctx, a, id)
			if e != nil {
				continue
			}
			var header struct {
				Format  string `json:"format"`
				Version int    `json:"version"`
				VaultID string `json:"vault_id"`
			}
			if json.Unmarshal(raw, &header) != nil || header.Format != "vaultpilot-cloud-connect" || header.Version != 1 || header.VaultID != id {
				continue
			}
			if items[id] == nil {
				items[id] = &candidate{VaultID: id}
			}
			items[id].AccountIDs = append(items[id].AccountIDs, a.ID)
			if len(items) > 100 {
				problem(w, fail("TOO_MANY_VAULTS", "发现的备份库超过100个，请缩小授权账号范围"))
				return
			}
		}
	}
	out := []*candidate{}
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VaultID < out[j].VaultID })
	write(w, 200, map[string]any{"items": out, "failed_accounts": failed})
}

func (s *Service) cloudJoin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 50*time.Second)
	defer cancel()
	if !s.rt.ConsumeSecretAttempt(r, "cloud_secret") {
		write(w, 429, map[string]any{"code": "RATE_LIMITED", "error": "密码尝试过于频繁，请稍后重试"})
		return
	}
	var req struct {
		VaultID  string `json:"vault_id"`
		Password string `json:"password"`
	}
	if err := decodeCloudSecret(r, &req); err != nil {
		problem(w, err)
		return
	}
	if !s.rt.CloudPending() {
		problem(w, fail("ALREADY_CONNECTED", "当前节点已连接备份库，不能切换为其他库"))
		return
	}
	if !cloudVaultID(req.VaultID) || len(req.Password) < 12 || len(req.Password) > 1024 {
		problem(w, fail("INVALID_CLOUD_CONNECT", "请选择备份库并输入至少12位的备份密码"))
		return
	}
	accounts, err := s.cloudAccounts(r.Context())
	if err != nil {
		problem(w, err)
		return
	}
	connected := false
	for _, a := range accounts {
		raw, e := s.readCloudBootstrap(ctx, a, req.VaultID)
		if e != nil {
			continue
		}
		if err = s.rt.ConnectCloudBootstrap(raw, req.Password, req.VaultID); err == nil {
			s.rt.ResetSecretAttempts(r, "cloud_secret")
			connected = true
			break
		}
	}
	if !connected {
		message := "无法连接：备份密码错误、入口已损坏或账号不可用，请核对原备份密码与账号"
		if err != nil {
			message += "。" + err.Error()
		}
		problem(w, fail("CLOUD_CONNECT_FAILED", message))
		return
	}
	_ = s.rt.Store.SetMeta(r.Context(), "cloud_auto_discover", "1")
	_ = s.rt.Store.Audit(r.Context(), "cloud.connect.join", req.VaultID)
	task, err := s.enqueue("discover", "", "", nil)
	if err != nil {
		write(w, 200, map[string]any{"connected": true, "vault_id": req.VaultID, "discovery_error": "已连接备份库，但发现任务未入队，请稍后点击立即发现"})
		return
	}
	write(w, 200, map[string]any{"connected": true, "vault_id": req.VaultID, "task": task})
}

func (s *Service) cloudAutoSave(w http.ResponseWriter, r *http.Request) {
	if !s.rt.RecoveryMode() || s.rt.CloudPending() {
		problem(w, fail("RECOVERY_NODE_REQUIRED", "请先连接已有备份库"))
		return
	}
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := decode(r, &req); err != nil {
		problem(w, err)
		return
	}
	value := "0"
	if req.Enabled {
		value = "1"
	}
	if err := s.rt.Store.SetMeta(r.Context(), "cloud_auto_discover", value); err != nil {
		problem(w, err)
		return
	}
	write(w, 200, map[string]any{"auto_discover": req.Enabled})
}

// List the small catalog directory; download and rebuild only after its
// contents or the node's Drive mapping changes. No password is retained.
func (s *Service) cloudSignature(ctx context.Context) (string, error) {
	accounts, err := s.cloudAccounts(ctx)
	if err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", fail("ACCOUNT_REQUIRED", "没有已验证的云端账号")
	}
	h := sha256.New()
	available := 0
	for _, a := range accounts {
		path := "VaultPilot/" + s.rt.VaultID() + "/catalog/"
		var names []string
		if a.Backend == "local" {
			entries, e := os.ReadDir(s.remoteFile(a, path))
			if e != nil {
				continue
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".ok") {
					names = append(names, entry.Name())
				}
			}
		} else {
			out, e := s.rcloneOutputBounded(ctx, []Account{a}, 4*1024*1024, "lsf", s.remoteFile(a, path), "--files-only")
			_ = s.rt.SyncRcloneCredentials()
			if e != nil {
				continue
			}
			for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.HasSuffix(name, ".ok") {
					names = append(names, name)
				}
			}
		}
		available++
		sort.Strings(names)
		fmt.Fprintf(h, "%s:%s:%s:%s:%s:%s\n", a.ID, a.Region, a.TenantID, a.DriveID, a.Remote, a.LocalPath)
		for _, name := range names {
			fmt.Fprintln(h, name)
		}
	}
	if available == 0 {
		return "", fail("CLOUD_UNAVAILABLE", "云端目录暂时不可用，请检查账号与网络")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Service) pollCloud() {
	if !s.rt.RecoveryMode() || s.rt.CloudPending() || !s.rt.Keys.Unlocked() {
		return
	}
	enabled, _ := s.rt.Store.Meta(s.ctx, "cloud_auto_discover")
	if enabled != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 50*time.Second)
	defer cancel()
	signature, err := s.cloudSignature(ctx)
	_ = s.rt.Store.SetMeta(s.ctx, "cloud_last_checked", now())
	if err != nil {
		_ = s.rt.Store.SetMeta(s.ctx, "cloud_last_error", err.Error())
		return
	}
	prior, _ := s.rt.Store.Meta(s.ctx, "cloud_auto_signature")
	if signature == prior {
		_ = s.rt.Store.SetMeta(s.ctx, "cloud_last_error", "")
		return
	}
	_, err = s.enqueue("discover", "", "", map[string]string{"cloud_signature": signature})
	if err != nil {
		_ = s.rt.Store.SetMeta(s.ctx, "cloud_last_error", err.Error())
	}
}
