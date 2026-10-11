package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// CloudBootstrapMaxBytes bounds untrusted connection files before password
// derivation. Bootstrap files contain only a password-encrypted master key and
// discovery locations, never OAuth credentials or cleartext vault secrets.
const CloudBootstrapMaxBytes = 64 * 1024

type cloudBootstrap struct {
	Format  string      `json:"format"`
	Version int         `json:"version"`
	VaultID string      `json:"vault_id"`
	Kit     RecoveryKit `json:"kit"`
}

// ConsumeSecretAttempt applies the same IP-based attempt limit used for login
// and vault unlocking. Callers must invoke it before processing a supplied
// cloud connection password, including candidate scans.
func (r *Runtime) ConsumeSecretAttempt(req *http.Request) bool { return !r.throttled(req) }

func validCloudPassword(password string) bool { return len(password) >= 12 && len(password) <= 1024 }

// BuildCloudBootstrap publishes the existing random vault/master identity under
// its original backup password. Exporting an offline kit with another password
// does not change the password accepted here, and this does not unlock the node.
func (r *Runtime) BuildCloudBootstrap(password string) ([]byte, error) {
	if r.RecoveryMode() || r.VaultID() == "" {
		return nil, errors.New("只有备份写入节点可以发布云端连接资料")
	}
	if !validCloudPassword(password) {
		return nil, errors.New("备份密码必须为 12 至 1024 个字符")
	}
	raw, err := r.meta("recovery_kit")
	if err != nil || len(raw) == 0 || len(raw) > CloudBootstrapMaxBytes {
		return nil, errors.New("本地备份解锁资料不可用")
	}
	var kit RecoveryKit
	if json.Unmarshal([]byte(raw), &kit) != nil {
		return nil, errors.New("本地备份解锁资料格式错误")
	}
	payload, err := openRecovery(kit, password)
	if err != nil {
		return nil, err
	}
	if payload.VaultID != r.VaultID() {
		return nil, errors.New("本地解锁资料不属于当前备份库")
	}
	rawBootstrap, err := json.Marshal(cloudBootstrap{Format: "vaultpilot-cloud-connect", Version: 1, VaultID: payload.VaultID, Kit: kit})
	if err != nil || len(rawBootstrap) > CloudBootstrapMaxBytes {
		return nil, errors.New("云端连接资料超过大小限制")
	}
	return rawBootstrap, nil
}

// UnlockCloudBootstrap authenticates a candidate without changing node state.
// The caller supplies the vault ID obtained from the authorized Drive path, so
// renamed files and substituted headers cannot connect a different vault.
func UnlockCloudBootstrap(raw []byte, password, expectedVaultID string) (RecoveryPayload, error) {
	var payload RecoveryPayload
	if len(raw) == 0 || len(raw) > CloudBootstrapMaxBytes || !validCloudPassword(password) {
		return payload, errors.New("云端连接资料或备份密码格式错误")
	}
	id, err := hex.DecodeString(expectedVaultID)
	if err != nil || len(id) != 16 {
		return payload, errors.New("云端备份库标识无效")
	}
	var bootstrap cloudBootstrap
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&bootstrap) != nil {
		return payload, errors.New("云端连接资料格式错误")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return payload, errors.New("云端连接资料格式错误")
	}
	if bootstrap.Format != "vaultpilot-cloud-connect" || bootstrap.Version != 1 {
		return payload, errors.New("不支持的云端连接资料版本")
	}
	if bootstrap.VaultID != expectedVaultID {
		return payload, errors.New("云端连接资料与所在备份库不匹配")
	}
	payload, err = openRecovery(bootstrap.Kit, password)
	if err != nil {
		return RecoveryPayload{}, err
	}
	if payload.VaultID != expectedVaultID {
		return RecoveryPayload{}, errors.New("云端连接资料与加密备份库不匹配")
	}
	return payload, nil
}

// ConnectCloudBootstrap transitions an admin-only pending node into a recovery
// reader. All persisted vault identity and unlock data commit together before
// the original master key is installed; failed attempts leave the node pending.
func (r *Runtime) ConnectCloudBootstrap(raw []byte, password, expectedVaultID string) error {
	r.setupMu.Lock()
	defer r.setupMu.Unlock()
	if !r.CloudPending() || r.VaultID() != "" || !r.Initialized() {
		return errors.New("只有尚未连接备份库的云端恢复节点可以执行此操作")
	}
	payload, err := UnlockCloudBootstrap(raw, password, expectedVaultID)
	if err != nil {
		return err
	}
	adminRaw, err := r.meta("admin_password")
	if err != nil {
		return err
	}
	var adminHash PasswordHash
	if json.Unmarshal([]byte(adminRaw), &adminHash) != nil {
		return errors.New("本地管理员凭据不可用")
	}
	if verifyPassword(password, adminHash) {
		return errors.New("请为本节点设置与备份密码不同的管理员登录密码")
	}
	// Reuse the authenticated cloud envelope's encrypted kit locally so the
	// shared backup password also unlocks this recovery node after restart.
	var bootstrap cloudBootstrap
	_ = json.Unmarshal(raw, &bootstrap)
	kitJSON, err := json.Marshal(bootstrap.Kit)
	if err != nil {
		return err
	}
	entriesJSON, err := json.Marshal(payload.Entries)
	if err != nil {
		return err
	}
	master, err := base64.StdEncoding.DecodeString(payload.MasterKey)
	if err != nil || len(master) != 32 {
		return errors.New("云端连接资料中的备份密钥无效")
	}
	defer func() {
		for i := range master {
			master[i] = 0
		}
	}()
	tx, err := r.Store.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for key, value := range map[string]string{"recovery_kit": string(kitJSON), "discovery_entries": string(entriesJSON), "mode": "recovery", "vault_id": payload.VaultID} {
		if _, err = tx.Exec("INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	r.mu.Lock()
	r.vaultID = payload.VaultID
	r.mode = "recovery"
	r.mu.Unlock()
	return r.Keys.set(master)
}
