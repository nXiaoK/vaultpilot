package platform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const telegramConfigKey = "telegram_config"

type telegramConfig struct {
	Version       int    `json:"version"`
	Enabled       bool   `json:"enabled"`
	BotToken      string `json:"bot_token"`
	ChatID        string `json:"chat_id"`
	NodeName      string `json:"node_name"`
	BackupSuccess bool   `json:"backup_success"`
	BackupFailure bool   `json:"backup_failure"`
	AccountError  bool   `json:"account_error"`
	Generation    string `json:"generation"`
	Since         string `json:"since"`
}

func (r *Runtime) sealNotification(id string, value []byte) (string, error) {
	box, err := encrypt(r.nodeKey, value, []byte("vaultpilot-notification-v1:"+id))
	return base64.StdEncoding.EncodeToString(box), err
}
func (r *Runtime) openNotification(id, value string) ([]byte, error) {
	box, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return nil, err
	}
	return decrypt(r.nodeKey, box, []byte("vaultpilot-notification-v1:"+id))
}
func (r *Runtime) telegramConfig() (telegramConfig, error) {
	c := telegramConfig{NodeName: "VaultPilot", BackupSuccess: true, BackupFailure: true, AccountError: true}
	raw, err := r.meta(telegramConfigKey)
	if err != nil || raw == "" {
		return c, err
	}
	plain, err := r.openNotification("config", raw)
	if err != nil {
		return c, errors.New("Telegram 配置无法解密")
	}
	if json.Unmarshal(plain, &c) != nil {
		return c, errors.New("Telegram 配置无效")
	}
	return c, nil
}

func (r *Runtime) telegramView(ctx context.Context, c telegramConfig) (map[string]any, error) {
	rows, err := r.Store.DB.QueryContext(ctx, "SELECT id,kind,status,attempts,created_at,sent_at,last_error FROM notification_outbox ORDER BY created_at DESC,id DESC LIMIT 10")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	recent := []map[string]any{}
	for rows.Next() {
		var id, kind, status, created, sent, lastError string
		var attempts int
		if err = rows.Scan(&id, &kind, &status, &attempts, &created, &sent, &lastError); err != nil {
			return nil, err
		}
		recent = append(recent, map[string]any{"id": id, "kind": kind, "status": status, "attempts": attempts, "created_at": created, "sent_at": sent, "last_error": lastError})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	var queued, failed int
	if err = r.Store.DB.QueryRowContext(ctx, "SELECT count(CASE WHEN status='queued' THEN 1 END),count(CASE WHEN status='failed' THEN 1 END) FROM notification_outbox").Scan(&queued, &failed); err != nil {
		return nil, err
	}
	return map[string]any{"version": c.Version, "enabled": c.Enabled, "token_configured": c.BotToken != "", "chat_id": c.ChatID, "node_name": c.NodeName, "backup_success": c.BackupSuccess, "backup_failure": c.BackupFailure, "account_error": c.AccountError, "recent": recent, "queued_count": queued, "failed_count": failed, "health_interval_minutes": 15}, nil
}
func (r *Runtime) telegramSettingsHTTP(w http.ResponseWriter, req *http.Request) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	c, err := r.telegramConfig()
	if err != nil {
		fail(w, 500, "notification_config_error", err.Error())
		return
	}
	v, err := r.telegramView(req.Context(), c)
	if err != nil {
		fail(w, 500, "database_error", "无法读取通知状态")
		return
	}
	writeJSON(w, 200, v)
}

var telegramTokenPattern = regexp.MustCompile(`^[0-9]{1,20}:[A-Za-z0-9_-]{20,150}$`)

func (r *Runtime) telegramSaveHTTP(w http.ResponseWriter, req *http.Request) {
	var v struct {
		Version       int    `json:"version"`
		Enabled       bool   `json:"enabled"`
		BotToken      string `json:"bot_token"`
		ClearToken    bool   `json:"clear_token"`
		ChatID        string `json:"chat_id"`
		NodeName      string `json:"node_name"`
		BackupSuccess bool   `json:"backup_success"`
		BackupFailure bool   `json:"backup_failure"`
		AccountError  bool   `json:"account_error"`
	}
	if !decode(w, req, &v) {
		return
	}
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	old, err := r.telegramConfig()
	if err != nil {
		fail(w, 500, "notification_config_error", err.Error())
		return
	}
	if v.Version != old.Version {
		fail(w, 409, "version_conflict", "通知设置已更新，请重新加载")
		return
	}
	c := old
	c.Enabled, c.ChatID, c.NodeName = v.Enabled, strings.TrimSpace(v.ChatID), strings.TrimSpace(v.NodeName)
	c.BackupSuccess, c.BackupFailure, c.AccountError = v.BackupSuccess, v.BackupFailure, v.AccountError
	if v.ClearToken {
		c.BotToken = ""
	}
	if strings.TrimSpace(v.BotToken) != "" {
		c.BotToken = strings.TrimSpace(v.BotToken)
	}
	chat, chatErr := strconv.ParseInt(c.ChatID, 10, 64)
	if (c.BotToken != "" && !telegramTokenPattern.MatchString(c.BotToken)) || (c.ChatID != "" && (chatErr != nil || chat <= 0 || strconv.FormatInt(chat, 10) != c.ChatID)) || len([]rune(c.NodeName)) > 80 || strings.ContainsAny(c.NodeName, "\r\n\x00") {
		fail(w, 400, "invalid_notification_config", "请填写有效的 Bot Token、正整数个人 Chat ID，以及不超过 80 字的节点名称")
		return
	}
	if c.NodeName == "" {
		c.NodeName = "VaultPilot"
	}
	if c.Enabled && (c.BotToken == "" || c.ChatID == "" || !(c.BackupSuccess || c.BackupFailure || c.AccountError)) {
		fail(w, 400, "invalid_notification_config", "开启通知需要 Bot Token、个人 Chat ID，并至少选择一种事件")
		return
	}
	reset := c.Generation == "" || c.Enabled != old.Enabled || c.BotToken != old.BotToken || c.ChatID != old.ChatID
	if reset {
		c.Generation = newID()
	}
	if reset || c.BackupSuccess != old.BackupSuccess || c.BackupFailure != old.BackupFailure || c.AccountError != old.AccountError {
		c.Since = time.Now().UTC().Format(time.RFC3339Nano)
	}
	c.Version++
	plain, _ := json.Marshal(c)
	sealed, err := r.sealNotification("config", plain)
	if err != nil {
		fail(w, 500, "notification_config_error", "无法加密通知配置")
		return
	}
	tx, err := r.Store.DB.BeginTx(req.Context(), nil)
	if err != nil {
		fail(w, 500, "database_error", "无法保存通知设置")
		return
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(req.Context(), "INSERT INTO metadata(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", telegramConfigKey, sealed)
	if err == nil && reset {
		_, err = tx.ExecContext(req.Context(), "UPDATE notification_outbox SET status='cancelled',body='' WHERE status IN ('queued','failed')")
	}
	if err == nil && reset {
		_, err = tx.ExecContext(req.Context(), "DELETE FROM notification_incidents")
	}
	if err == nil && reset {
		_, err = tx.ExecContext(req.Context(), "DELETE FROM metadata WHERE key='telegram_retry_after'")
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		fail(w, 500, "database_error", "无法保存通知设置")
		return
	}
	_ = r.Store.Audit(req.Context(), "notification.telegram_updated", "telegram")
	view, err := r.telegramView(req.Context(), c)
	if err != nil {
		fail(w, 500, "database_error", "设置已保存，但无法读取状态")
		return
	}
	r.wakeNotifications()
	writeJSON(w, 200, view)
}

func (r *Runtime) queueNotification(ctx context.Context, c telegramConfig, id, kind, text string) error {
	sealed, err := r.sealNotification(id, []byte(text))
	if err != nil {
		return err
	}
	_, err = r.Store.DB.ExecContext(ctx, "INSERT OR IGNORE INTO notification_outbox(id,kind,generation,body,created_at) VALUES(?,?,?,?,?)", id, kind, c.Generation, sealed, time.Now().UTC().Format(time.RFC3339Nano))
	r.wakeNotifications()
	return err
}
func (r *Runtime) wakeNotifications() {
	if r.notificationWake != nil {
		select {
		case r.notificationWake <- struct{}{}:
		default:
		}
	}
}
func (r *Runtime) telegramTestHTTP(w http.ResponseWriter, req *http.Request) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled {
		fail(w, 409, "notification_disabled", "请先保存并启用 Telegram 通知")
		return
	}
	var count int
	if r.Store.DB.QueryRowContext(req.Context(), "SELECT count(*) FROM notification_outbox WHERE kind='test' AND created_at>?", time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)).Scan(&count) != nil {
		fail(w, 500, "database_error", "无法读取通知记录")
		return
	}
	if count >= 3 {
		fail(w, 429, "notification_rate_limit", "测试通知每分钟最多 3 次")
		return
	}
	id := c.Generation + ":test:" + newID()
	err = r.queueNotification(req.Context(), c, id, "test", fmt.Sprintf("VaultPilot · Telegram 测试通知\n节点：%s\n通知连接测试已提交。\n时间：%s", noticeName(c.NodeName), time.Now().UTC().Format(time.RFC3339)))
	if err != nil {
		fail(w, 500, "database_error", "无法创建测试通知")
		return
	}
	writeJSON(w, 202, map[string]any{"id": id, "status": "queued"})
}
func (r *Runtime) telegramRetryHTTP(w http.ResponseWriter, req *http.Request) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled {
		fail(w, 409, "notification_disabled", "请先启用 Telegram 通知")
		return
	}
	_, err = r.Store.DB.ExecContext(req.Context(), "UPDATE notification_outbox SET status='queued',attempts=0,next_at=0,last_error='' WHERE status='failed' AND generation=?", c.Generation)
	if err != nil {
		fail(w, 500, "database_error", "无法重试通知")
		return
	}
	r.wakeNotifications()
	writeJSON(w, 202, map[string]string{"status": "queued"})
}
func (r *Runtime) telegramCheckHTTP(w http.ResponseWriter, req *http.Request) {
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled || !c.AccountError {
		fail(w, 409, "notification_disabled", "请先保存并开启账号授权异常通知")
		return
	}
	select {
	case r.healthWake <- struct{}{}:
	default:
	}
	writeJSON(w, 202, map[string]string{"status": "queued"})
}

// Names are capped and flattened; notification bodies never include raw engine
// logs, OAuth/Telegram responses, file paths, passwords or credential material.
func noticeName(value string) string {
	runes := []rune(strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return ' '
		}
		return c
	}, value))
	if len(runes) > 100 {
		runes = runes[:100]
	}
	return string(runes)
}
func (r *Runtime) NotifyBackup(id string) error {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled {
		return err
	}
	var t struct {
		ID, Kind, Status, Code string
		PlanID                 string `json:"plan_id"`
		StartedAt              string `json:"started_at"`
		FinishedAt             string `json:"finished_at"`
		ScannedBytes           int64  `json:"scanned_bytes"`
		UploadedBytes          int64  `json:"uploaded_bytes"`
	}
	ctx := context.Background()
	if err = r.Store.Get(ctx, "backup_runs", id, &t); err != nil {
		return err
	}
	if t.Kind != "backup" || t.FinishedAt == "" || t.FinishedAt < c.Since {
		return nil
	}
	label, kind := "", "backup_failure"
	switch t.Status {
	case "completed":
		if !c.BackupSuccess {
			return nil
		}
		label, kind = "备份成功", "backup_success"
	case "failed", "partial_failed":
		label = "备份失败"
	case "cancelled":
		label = "备份已取消"
	case "interrupted":
		label = "备份因节点重启中断"
	default:
		return nil
	}
	if kind == "backup_failure" && !c.BackupFailure {
		return nil
	}
	var plan struct{ Name string }
	_ = r.Store.Get(ctx, "backup_plans", t.PlanID, &plan)
	if plan.Name == "" {
		plan.Name = t.PlanID
	}
	text := fmt.Sprintf("VaultPilot · %s\n节点：%s\n计划：%s\n任务：%s\n结束：%s\n扫描处理量：%d Bytes\n实际上传量：%d Bytes", label, noticeName(c.NodeName), noticeName(plan.Name), noticeName(t.ID), t.FinishedAt, t.ScannedBytes, t.UploadedBytes)
	if start, e := time.Parse(time.RFC3339Nano, t.StartedAt); e == nil {
		if end, e := time.Parse(time.RFC3339Nano, t.FinishedAt); e == nil && end.After(start) {
			text += fmt.Sprintf("\n用时：%s", end.Sub(start).Round(time.Second))
		}
	}
	if kind == "backup_failure" {
		text += "\n请在任务中心查看详情与日志。"
	}
	return r.queueNotification(ctx, c, c.Generation+":backup:"+id, kind, text)
}

type authorizationIssue struct{ category, message string }

func (e authorizationIssue) Error() string { return e.message }
func (r *Runtime) ObserveAccountAccess(id string, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	category := ""
	var issue authorizationIssue
	var cloud cloudError
	if errors.As(err, &issue) {
		category = issue.category
	} else if errors.As(err, &cloud) {
		if cloud.Status == 401 {
			category = "authorization_required"
		}
		if cloud.Status == 403 {
			category = "access_denied"
		}
	}
	if category != "" {
		r.AccountAuthorizationFailed(id, category)
	}
}
func (r *Runtime) AccountAuthorizationFailed(id, category string) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled || !c.AccountError {
		return
	}
	labels := map[string]string{"authorization_required": "授权失效或凭据不可用，请重新授权。", "access_denied": "OneDrive 拒绝访问，请检查权限并重新授权。", "refresh_failed": "凭据刷新失败，请检查网络或重新授权。", "authorization_failed": "微软授权未完成，请检查授权结果后重试。"}
	label, ok := labels[category]
	if !ok {
		return
	}
	var a Account
	ctx := context.Background()
	if r.Store.Get(ctx, "accounts", id, &a) != nil || a.Backend != "onedrive" {
		return
	}
	var episode string
	if err = r.Store.DB.QueryRowContext(ctx, "SELECT episode FROM notification_incidents WHERE account_id=?", id).Scan(&episode); err == nil {
		return
	} else if !errors.Is(err, sql.ErrNoRows) {
		return
	}
	episode = newID()
	eventID := c.Generation + ":account:" + id + ":" + episode
	text := fmt.Sprintf("VaultPilot · 存储账号授权异常\n节点：%s\n账号：%s\n账号 ID：%s\n%s\n时间：%s", noticeName(c.NodeName), noticeName(a.Name), noticeName(a.ID), label, time.Now().UTC().Format(time.RFC3339))
	sealed, err := r.sealNotification(eventID, []byte(text))
	if err != nil {
		return
	}
	tx, err := r.Store.DB.BeginTx(ctx, nil)
	if err != nil {
		return
	}
	defer tx.Rollback()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, "INSERT INTO notification_incidents(account_id,episode,category,updated_at) VALUES(?,?,?,?)", id, episode, category, stamp)
	if err == nil {
		_, err = tx.ExecContext(ctx, "INSERT INTO notification_outbox(id,kind,generation,body,created_at) VALUES(?,'account_error',?,?,?)", eventID, c.Generation, sealed, stamp)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err == nil {
		r.wakeNotifications()
	}
}
func (r *Runtime) AccountAuthorizationHealthy(id string, verified bool) {
	r.notificationMu.Lock()
	defer r.notificationMu.Unlock()
	query := "DELETE FROM notification_incidents WHERE account_id=?"
	if !verified {
		query += " AND category IN ('authorization_required','refresh_failed')"
	}
	_, _ = r.Store.DB.Exec(query, id)
}

// Rclone/Restic errors are inspected locally only. Raw output is never sent to
// Telegram. The caller must identify the remote/account involved.
func (r *Runtime) ObserveRcloneFailure(id string, output []byte, err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	text := strings.ToLower(string(output))
	category := ""
	for _, marker := range []string{"invalid_grant", "invalidauthenticationtoken", "unauthenticated", "token has expired", "token is expired", "401 unauthorized", "401 unauthorised", "status code: 401"} {
		if strings.Contains(text, marker) {
			category = "authorization_required"
			break
		}
	}
	if category == "" {
		for _, marker := range []string{"accessdenied", "access_denied", "403 forbidden", "status code: 403"} {
			if strings.Contains(text, marker) {
				category = "access_denied"
				break
			}
		}
	}
	if category != "" {
		r.AccountAuthorizationFailed(id, category)
	}
}

func (r *Runtime) startNotifications() {
	ctx, cancel := context.WithCancel(context.Background())
	r.notificationCancel = cancel
	r.notificationWake = make(chan struct{}, 1)
	r.healthWake = make(chan struct{}, 1)
	r.notificationWG.Add(2)
	go func() {
		defer r.notificationWG.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-r.notificationWake:
			}
			r.reconcileBackupNotifications(ctx)
			r.deliverNotifications(ctx)
		}
	}()
	go func() {
		defer r.notificationWG.Done()
		initial := time.NewTimer(5 * time.Second)
		defer initial.Stop()
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-initial.C:
			case <-ticker.C:
			case <-r.healthWake:
			}
			r.checkAccountAuthorizations(ctx)
		}
	}()
}
func (r *Runtime) reconcileBackupNotifications(ctx context.Context) {
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled {
		return
	}
	since := c.Since
	floor := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339Nano)
	if since < floor {
		since = floor
	}
	rows, err := r.Store.DB.QueryContext(ctx, `SELECT id FROM backup_runs WHERE json_extract(data,'$.kind')='backup' AND json_extract(data,'$.finished_at')>=? AND ((json_extract(data,'$.status')='completed' AND ?=1) OR (json_extract(data,'$.status') IN ('failed','partial_failed','cancelled','interrupted') AND ?=1)) AND NOT EXISTS (SELECT 1 FROM notification_outbox WHERE notification_outbox.id=?||':backup:'||backup_runs.id) ORDER BY updated_at LIMIT 100`, since, c.BackupSuccess, c.BackupFailure, c.Generation)
	if err != nil {
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		_ = r.NotifyBackup(id)
	}
	_, _ = r.Store.DB.ExecContext(ctx, "DELETE FROM notification_outbox WHERE status IN ('sent','cancelled') AND created_at<?", floor)
}
func (r *Runtime) checkAccountAuthorizations(ctx context.Context) {
	c, err := r.telegramConfig()
	if err != nil || !c.Enabled || !c.AccountError {
		return
	}
	raws, err := r.Store.All(ctx, "accounts")
	if err != nil {
		return
	}
	for _, raw := range raws {
		if ctx.Err() != nil {
			return
		}
		var a Account
		if json.Unmarshal(raw, &a) != nil || a.Backend != "onedrive" || !a.Enabled || !a.Verified || a.DriveID == "" {
			continue
		}
		checkCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		_, err = r.graph(checkCtx, a.ID, "GET", "/drives/"+url.PathEscape(a.DriveID)+"?$select=id", nil, "")
		cancel()
		if err == nil {
			r.AccountAuthorizationHealthy(a.ID, false)
		}
	}
}

type telegramSendError struct {
	message string
	retry   bool
	delay   time.Duration
}

func (e telegramSendError) Error() string { return e.message }
func (r *Runtime) sendTelegram(ctx context.Context, c telegramConfig, text string) error {
	body, _ := json.Marshal(map[string]any{"chat_id": c.ChatID, "text": text, "disable_web_page_preview": true})
	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.telegram.org/bot"+c.BotToken+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return telegramSendError{"Telegram 请求配置无效", false, 0}
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second, Transport: r.client.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return telegramSendError{"无法连接 Telegram，请检查服务器网络", true, 0}
	}
	defer res.Body.Close()
	var reply struct {
		OK         bool `json:"ok"`
		ErrorCode  int  `json:"error_code"`
		Parameters struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&reply)
	if res.StatusCode == 200 && decodeErr == nil && reply.OK {
		return nil
	}
	code := res.StatusCode
	if reply.ErrorCode != 0 {
		code = reply.ErrorCode
	}
	if code == 429 {
		delay := time.Duration(reply.Parameters.RetryAfter) * time.Second
		if delay < time.Second {
			delay = time.Minute
		}
		if delay > time.Hour {
			delay = time.Hour
		}
		return telegramSendError{"Telegram 限流，稍后自动重试", true, delay}
	}
	if code == 401 {
		return telegramSendError{"Bot Token 无效，请重新配置", false, 0}
	}
	if code == 403 {
		return telegramSendError{"机器人无权发送，请先私聊机器人发送 /start，并检查是否被屏蔽", false, 0}
	}
	if code == 400 {
		return telegramSendError{"Chat ID 或请求无效，请确认用户已向机器人发送 /start", false, 0}
	}
	return telegramSendError{fmt.Sprintf("Telegram 返回异常响应（HTTP %d）", res.StatusCode), code >= 500 || res.StatusCode == 200, 0}
}
func (r *Runtime) deliverNotifications(ctx context.Context) {
	for n := 0; n < 20 && ctx.Err() == nil; n++ {
		if raw, _ := r.meta("telegram_retry_after"); raw != "" {
			if until, _ := strconv.ParseInt(raw, 10, 64); until > time.Now().Unix() {
				return
			}
		}
		r.notificationMu.Lock()
		c, err := r.telegramConfig()
		if err != nil || !c.Enabled {
			r.notificationMu.Unlock()
			return
		}
		var id, kind, generation, body string
		var attempts int
		err = r.Store.DB.QueryRowContext(ctx, "SELECT id,kind,generation,body,attempts FROM notification_outbox WHERE status='queued' AND next_at<=? ORDER BY created_at,id LIMIT 1", time.Now().Unix()).Scan(&id, &kind, &generation, &body, &attempts)
		if err != nil {
			r.notificationMu.Unlock()
			return
		}
		allowed := kind == "test" || (kind == "backup_success" && c.BackupSuccess) || (kind == "backup_failure" && c.BackupFailure) || (kind == "account_error" && c.AccountError)
		if generation != c.Generation || !allowed {
			_, _ = r.Store.DB.ExecContext(ctx, "UPDATE notification_outbox SET status='cancelled',body='' WHERE id=?", id)
			r.notificationMu.Unlock()
			continue
		}
		plain, err := r.openNotification(id, body)
		r.notificationMu.Unlock()
		if err == nil {
			err = r.sendTelegram(ctx, c, string(plain))
		} else {
			err = telegramSendError{"通知内容无法解密", false, 0}
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			_, _ = r.Store.DB.ExecContext(ctx, "UPDATE notification_outbox SET status='sent',attempts=attempts+1,sent_at=?,last_error='',body='' WHERE id=? AND status='queued'", time.Now().UTC().Format(time.RFC3339Nano), id)
			continue
		}
		attempts++
		var failure telegramSendError
		_ = errors.As(err, &failure)
		status := "failed"
		delay := time.Duration(1<<uint(attempts)) * 15 * time.Second
		if delay > time.Hour {
			delay = time.Hour
		}
		if failure.delay > delay {
			delay = failure.delay
		}
		if failure.retry && attempts < 8 {
			status = "queued"
		}
		_, _ = r.Store.DB.ExecContext(ctx, "UPDATE notification_outbox SET status=?,attempts=?,next_at=?,last_error=? WHERE id=? AND status='queued'", status, attempts, time.Now().Add(delay).Unix(), err.Error(), id)
		if failure.delay > 0 {
			_ = r.setMeta("telegram_retry_after", strconv.FormatInt(time.Now().Add(delay).Unix(), 10))
			return
		} // Telegram rate limiting applies to the bot, not just one message.
	}
}
