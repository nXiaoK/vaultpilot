package platform

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vaultpilot/internal/ctxlock"
	"vaultpilot/internal/store"
)

type Config struct {
	Addr, StateDir, CacheDir, PublicURL, ResticBin, RcloneBin, AutoUnlockFile string
	SourceRoots, RestoreRoots, AllowedHosts                                   []string
	TrustedProxies                                                            []netip.Prefix
	AllowLocal                                                                bool
	GlobalConcurrency                                                         int
	GoogleDriveTPSLimit                                                       float64
	GoogleDriveTPSLimitBurst                                                  int
	LogMaxRows                                                                int
	CacheMaxBytes                                                             int64
	MaintenanceWindow, MaintenanceTimezone                                    string
	OAuthClientID, OAuthClientSecret, OAuthTenant                             string
}
type session struct {
	Username, CSRF string
	Expires        time.Time
}
type loginAttempt struct {
	Count int
	Since time.Time
}
type oauthState struct {
	AccountID, Verifier, SessionID string
	Expires                        time.Time
	Version                        int
}
type Runtime struct {
	Config             Config
	Store              *store.Store
	Keys               *Keyring
	mu                 sync.Mutex
	setupMu            sync.Mutex
	sessions           map[string]session
	attempts           map[string]loginAttempt
	oauth              map[string]oauthState
	client             *http.Client
	nodeKey            []byte
	vaultID, mode      string
	rcloneMu           sync.Mutex
	credentialsMu      sync.Mutex
	accountMu          ctxlock.Mutex
	recentDrives       map[string]cachedDrives // guarded by accountMu
	accountTransferMu  sync.Mutex
	rclonePath         string
	rclonePaths        []string
	rcloneAuthMu       sync.Mutex
	rcloneAuth         map[string]*rcloneAuthFlow
	rcloneAuthWG       sync.WaitGroup
	closing            bool
	network            *networkMonitor
	notificationMu     sync.Mutex
	notificationCancel context.CancelFunc
	notificationWG     sync.WaitGroup
	notificationWake   chan struct{}
	healthWake         chan struct{}
}

func New(c Config) (*Runtime, error) {
	// The zero-value Config leaves pacing disabled. The executable supplies its
	// conservative defaults; callers can explicitly opt out with a zero limit.
	if math.IsNaN(c.GoogleDriveTPSLimit) || math.IsInf(c.GoogleDriveTPSLimit, 0) || c.GoogleDriveTPSLimit != 0 && (c.GoogleDriveTPSLimit < 0.01 || c.GoogleDriveTPSLimit > 1000) {
		return nil, errors.New("VP_GDRIVE_TPS_LIMIT 必须为 0（关闭）或 0.01 至 1000 之间的有限数字")
	}
	if c.GoogleDriveTPSLimitBurst == 0 {
		c.GoogleDriveTPSLimitBurst = 1
	}
	if c.GoogleDriveTPSLimitBurst < 1 || c.GoogleDriveTPSLimitBurst > 100 {
		return nil, errors.New("VP_GDRIVE_TPS_LIMIT_BURST 必须为 1 至 100 的整数")
	}
	if c.StateDir == "" {
		c.StateDir = "./state"
	}
	if c.CacheDir == "" {
		c.CacheDir = filepath.Join(c.StateDir, "cache")
	}
	if c.ResticBin == "" {
		c.ResticBin = "restic"
	}
	if c.RcloneBin == "" {
		c.RcloneBin = "rclone"
	}
	if c.OAuthTenant == "" {
		c.OAuthTenant = "consumers"
	}
	if c.GlobalConcurrency < 1 {
		c.GlobalConcurrency = 2
	}
	if c.LogMaxRows == 0 {
		c.LogMaxRows = 100000
	}
	if c.CacheMaxBytes == 0 {
		c.CacheMaxBytes = 20 << 30
	}
	if c.LogMaxRows < 100 || c.CacheMaxBytes < 1 {
		return nil, errors.New("日志上限至少100条，缓存上限必须大于零")
	}
	if c.MaintenanceTimezone == "" {
		c.MaintenanceTimezone = "Asia/Shanghai"
	}
	if _, e := time.LoadLocation(c.MaintenanceTimezone); e != nil {
		return nil, errors.New("维护窗口时区无效")
	}
	if _, _, e := parseMaintenanceWindow(c.MaintenanceWindow); e != nil {
		return nil, e
	}
	for _, value := range []string{c.OAuthClientID, c.OAuthClientSecret, c.OAuthTenant} {
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("OAuth 配置不能包含换行或 NUL")
		}
	}
	var e error
	c.StateDir, e = filepath.Abs(c.StateDir)
	if e != nil {
		return nil, e
	}
	c.CacheDir, e = filepath.Abs(c.CacheDir)
	if e != nil {
		return nil, e
	}
	for _, dir := range []string{c.StateDir, c.CacheDir} {
		if e = os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
		if err := os.Chmod(dir, 0700); err != nil {
			log.Printf("cannot restrict runtime directory %q to 0700: %v", dir, err)
		}
	}
	if e = cleanupStaleManifestDirs(c.CacheDir); e != nil {
		return nil, e
	}
	s, e := store.Open(filepath.Join(c.StateDir, "vaultpilot.db"))
	if e != nil {
		return nil, e
	}
	if e = s.SetLogLimit(c.LogMaxRows); e != nil {
		s.DB.Close()
		return nil, e
	}
	r := &Runtime{Config: c, Store: s, Keys: &Keyring{}, sessions: map[string]session{}, attempts: map[string]loginAttempt{}, oauth: map[string]oauthState{}, client: &http.Client{Timeout: 45 * time.Second}}
	keyPath := filepath.Join(c.StateDir, "node-key")
	key, e := os.ReadFile(keyPath)
	if errors.Is(e, os.ErrNotExist) {
		key = randomBytes(32)
		f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			s.DB.Close()
			return nil, err
		}
		_, e = f.Write(key)
		ce := f.Close()
		if e == nil {
			e = ce
		}
	}
	if e != nil || len(key) != 32 {
		s.DB.Close()
		return nil, errors.New("节点凭据密钥无效")
	}
	info, e := os.Stat(keyPath)
	if e != nil || info.Mode().Perm()&0077 != 0 {
		s.DB.Close()
		return nil, errors.New("节点凭据密钥权限必须为 0600")
	}
	r.nodeKey = key
	// A crash may occur after Rclone writes a refreshed token but before an
	// engine completion hook. Persist those refreshes before deleting files.
	r.rclonePaths, e = filepath.Glob(filepath.Join(c.CacheDir, "rclone-*.conf"))
	if e != nil {
		r.Close()
		return nil, e
	}
	if e = r.SyncRcloneCredentials(); e != nil {
		r.Close()
		return nil, e
	}
	for _, path := range r.rclonePaths {
		if e = os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
			r.Close()
			return nil, e
		}
	}
	r.rclonePaths = nil
	// Authorization/refresh subprocesses are gone after restart.
	staleOAuth, _ := filepath.Glob(filepath.Join(c.CacheDir, "oauth-*.conf"))
	for _, path := range staleOAuth {
		_ = os.Remove(path)
	}
	r.vaultID, _ = r.meta("vault_id")
	r.mode, _ = r.meta("mode")
	if r.mode == "" {
		r.mode = "create"
	}
	if c.AutoUnlockFile != "" && r.vaultID != "" {
		info, e := os.Stat(c.AutoUnlockFile)
		if e != nil || info.Mode().Perm()&0077 != 0 {
			r.Close()
			return nil, errors.New("自动解锁文件必须存在且权限为 0600")
		}
		password, e := os.ReadFile(c.AutoUnlockFile)
		if e != nil {
			r.Close()
			return nil, e
		}
		e = r.unlock(strings.TrimSpace(string(password)))
		for i := range password {
			password[i] = 0
		}
		if e != nil {
			r.Close()
			return nil, e
		}
	}
	r.network = newNetworkMonitor(readNetworkCounters)
	r.startNotifications()
	return r, nil
}
func (r *Runtime) Close() error {
	if r.notificationCancel != nil {
		r.notificationCancel()
		r.notificationWG.Wait()
	}
	if r.network != nil {
		r.network.closeOnce.Do(func() { close(r.network.stop) })
		<-r.network.done
	}
	r.rcloneAuthMu.Lock()
	r.closing = true
	for _, flow := range r.rcloneAuth {
		flow.cancel()
	}
	r.rcloneAuthMu.Unlock()
	r.rcloneAuthWG.Wait()
	r.Keys.Lock()
	r.rcloneMu.Lock()
	for _, path := range r.rclonePaths {
		_ = os.Remove(path)
	}
	r.rcloneMu.Unlock()
	for i := range r.nodeKey {
		r.nodeKey[i] = 0
	}
	return r.Store.DB.Close()
}
func (r *Runtime) VaultID() string { r.mu.Lock(); defer r.mu.Unlock(); return r.vaultID }
func (r *Runtime) Initialized() bool {
	username, err := r.meta("admin_username")
	return err == nil && username != ""
}
func (r *Runtime) RecoveryMode() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode == "recovery" || r.mode == "cloud"
}
func (r *Runtime) CloudPending() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.mode == "cloud"
}
func (r *Runtime) meta(key string) (string, error) {
	v, e := r.Store.Meta(context.Background(), key)
	if errors.Is(e, sql.ErrNoRows) {
		return "", nil
	}
	return v, e
}
func (r *Runtime) setMeta(key, value string) error {
	return r.Store.SetMeta(context.Background(), key, value)
}
func (r *Runtime) unlock(password string) error {
	b, e := r.meta("recovery_kit")
	if e != nil {
		return e
	}
	var kit RecoveryKit
	if json.Unmarshal([]byte(b), &kit) != nil {
		return errors.New("恢复包不可用")
	}
	p, e := openRecovery(kit, password)
	if e != nil {
		return e
	}
	if p.VaultID != r.VaultID() {
		return errors.New("恢复包不属于当前备份库")
	}
	key, e := base64.StdEncoding.DecodeString(p.MasterKey)
	if e != nil {
		return e
	}
	defer func() {
		for i := range key {
			key[i] = 0
		}
	}()
	return r.Keys.set(key)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": message, "code": code, "retryable": status == 429 || status == 503})
}
func decode(w http.ResponseWriter, req *http.Request, out any) bool {
	return decodeLimit(w, req, out, 16*1024*1024)
}
func decodeLimit(w http.ResponseWriter, req *http.Request, out any, limit int64) bool {
	req.Body = http.MaxBytesReader(w, req.Body, limit)
	d := json.NewDecoder(req.Body)
	if e := d.Decode(out); e != nil {
		fail(w, 400, "invalid_json", "请求 JSON 无效")
		return false
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		fail(w, 400, "invalid_json", "请求只能包含一个 JSON 对象")
		return false
	}
	return true
}
func engineAvailable(bin string) bool { _, e := exec.LookPath(bin); return e == nil }
func (r *Runtime) secureCookie() bool {
	u, e := url.Parse(r.Config.PublicURL)
	return e == nil && u.Scheme == "https"
}
func (r *Runtime) Register(m *http.ServeMux) {
	m.HandleFunc("GET /api/status", r.status)
	m.HandleFunc("GET /api/metrics/network", r.networkMetrics)
	m.HandleFunc("POST /api/setup", r.setup)
	m.HandleFunc("POST /api/auth/login", r.login)
	m.HandleFunc("POST /api/auth/logout", r.logout)
	m.HandleFunc("POST /api/security/unlock", r.unlockHandler)
	m.HandleFunc("POST /api/security/lock", r.lockHandler)
	m.HandleFunc("POST /api/security/recovery-kit", r.exportKit)
	m.HandleFunc("GET /api/notifications/telegram", r.telegramSettingsHTTP)
	m.HandleFunc("PUT /api/notifications/telegram", r.telegramSaveHTTP)
	m.HandleFunc("POST /api/notifications/telegram/test", r.telegramTestHTTP)
	m.HandleFunc("POST /api/notifications/telegram/retry", r.telegramRetryHTTP)
	m.HandleFunc("POST /api/notifications/telegram/check-accounts", r.telegramCheckHTTP)
	m.HandleFunc("GET /api/accounts", r.listAccounts)
	m.HandleFunc("POST /api/accounts", r.createAccount)
	m.HandleFunc("POST /api/accounts/export", r.exportAccounts)
	m.HandleFunc("POST /api/accounts/import/preview", r.previewAccounts)
	m.HandleFunc("POST /api/accounts/import", r.importAccounts)
	m.HandleFunc("PUT /api/accounts/{id}", r.updateAccount)
	m.HandleFunc("PATCH /api/accounts/{id}", r.updateAccount)
	m.HandleFunc("DELETE /api/accounts/{id}", r.deleteAccount)
	m.HandleFunc("POST /api/accounts/{id}/authorize", r.authorize)
	m.HandleFunc("POST /api/accounts/{id}/rclone/authorize", r.startRcloneAuth)
	m.HandleFunc("GET /api/accounts/{id}/rclone/authorize", r.getRcloneAuth)
	m.HandleFunc("DELETE /api/accounts/{id}/rclone/authorize", r.cancelRcloneAuth)
	m.HandleFunc("POST /api/accounts/{id}/rclone/callback", r.relayRcloneCallback)
	m.HandleFunc("POST /api/rclone/import/preview", r.previewRcloneImport)
	m.HandleFunc("POST /api/accounts/{id}/rclone/import", r.importRcloneAccount)
	m.HandleFunc("GET /api/oauth/callback", r.oauthCallback)
	m.HandleFunc("GET /api/accounts/{id}/drives", r.drives)
	m.HandleFunc("POST /api/accounts/{id}/verify", r.verifyAccount)
	m.HandleFunc("POST /api/accounts/{id}/quota", r.refreshQuota)
	m.HandleFunc("GET /api/pools", r.listPools)
	m.HandleFunc("POST /api/pools", r.savePool)
	m.HandleFunc("PUT /api/pools/{id}", r.savePool)
	m.HandleFunc("PATCH /api/pools/{id}", r.savePool)
	m.HandleFunc("DELETE /api/pools/{id}", r.deletePool)
	m.HandleFunc("GET /api/sources", r.listSources)
	m.HandleFunc("GET /api/sources/{id}/files", r.sourceFiles)
	m.HandleFunc("GET /api/sources/{id}/content", r.sourceContent)
	m.HandleFunc("POST /api/sources", r.saveSource)
	m.HandleFunc("PUT /api/sources/{id}", r.saveSource)
	m.HandleFunc("PATCH /api/sources/{id}", r.saveSource)
	m.HandleFunc("DELETE /api/sources/{id}", r.deleteSource)
	m.HandleFunc("GET /api/browse", r.browse)
	m.HandleFunc("GET /api/audit-events", func(w http.ResponseWriter, req *http.Request) { r.list(w, req, "audit_events") })
}

// LockAccountConfiguration serializes destructive storage maintenance with
// account edits, deletion, verification and credential installation.
func (r *Runtime) LockAccountConfiguration() func() {
	r.accountMu.Lock()
	return r.accountMu.Unlock
}

// LockAccountConfigurationContext allows maintenance to abandon a cancelled wait.
func (r *Runtime) LockAccountConfigurationContext(ctx context.Context) (func(), error) {
	if err := r.accountMu.LockContext(ctx); err != nil {
		return nil, err
	}
	return r.accountMu.Unlock, nil
}
