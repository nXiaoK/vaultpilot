package platform

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// RcloneConfigFor writes a restricted ephemeral config for the given accounts.
func (r *Runtime) RcloneConfigFor(ctx context.Context, accountIDs ...string) (string, error) {
	if e := ctx.Err(); e != nil {
		return "", e
	}
	if len(accountIDs) == 0 {
		return "", errors.New("未指定 Rclone 账号")
	}
	r.rcloneMu.Lock()
	defer r.rcloneMu.Unlock()
	if e := ctx.Err(); e != nil {
		return "", e
	}
	if e := r.syncRcloneCredentials(); e != nil {
		return "", e
	}
	var b strings.Builder
	seen := map[string]bool{}
	for _, id := range accountIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		var a Account
		if e := r.Store.Get(ctx, "accounts", id, &a); e != nil {
			return "", fmt.Errorf("账号「%s」：%w", id, e)
		}
		if a.Backend == "local" {
			continue
		}
		if !cloudBackend(a.Backend) || !a.Verified {
			return "", fmt.Errorf("账号「%s」：账号未验证或不支持 Rclone", a.Name)
		}
		if _, e := r.accessToken(ctx, a.ID); e != nil {
			return "", fmt.Errorf("账号「%s」：%w", a.Name, e)
		}
		c, e := r.loadCredential(a.ID)
		if e != nil {
			return "", fmt.Errorf("账号「%s」：%w", a.Name, e)
		}
		var current Account
		if e = r.Store.Get(ctx, "accounts", a.ID, &current); e != nil {
			return "", fmt.Errorf("账号「%s」：%w", a.Name, e)
		}
		if current.Backend != a.Backend || current.Remote != a.Remote || current.DriveID != a.DriveID || current.DriveType != a.DriveType || current.Verified != a.Verified {
			return "", fmt.Errorf("账号「%s」：配置已更改，请重试", a.Name)
		}
		configCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		content, e := r.credentialConfig(configCtx, a.Remote, c, a.DriveID, a.DriveType)
		if configCtx.Err() != nil {
			e = configCtx.Err()
		}
		cancel()
		if e != nil {
			return "", fmt.Errorf("账号「%s」：%w", a.Name, e)
		}
		b.WriteString(content)
	}
	if e := ctx.Err(); e != nil {
		return "", e
	}
	f, e := os.CreateTemp(r.Config.CacheDir, "rclone-*.conf")
	if e != nil {
		return "", e
	}
	path := f.Name()
	if e = f.Chmod(0600); e == nil {
		_, e = f.WriteString(b.String())
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		os.Remove(path)
		return "", e
	}
	r.rclonePath = path
	r.rclonePaths = append(r.rclonePaths, path)
	return path, nil
}
func (r *Runtime) SyncRcloneCredentials() error {
	r.rcloneMu.Lock()
	defer r.rcloneMu.Unlock()
	return r.syncRcloneCredentials()
}
func (r *Runtime) syncRcloneCredentials() error {
	for _, path := range r.rclonePaths {
		if e := r.syncRcloneFile(path); e != nil {
			return e
		}
	}
	return nil
}

// ReleaseRcloneConfig persists refreshes and removes a completed task's file.
func (r *Runtime) ReleaseRcloneConfig(path string) error {
	r.rcloneMu.Lock()
	defer r.rcloneMu.Unlock()
	for i, candidate := range r.rclonePaths {
		if candidate == path {
			if e := r.syncRcloneFile(path); e != nil {
				return e
			}
			if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
				return e
			}
			r.rclonePaths = append(r.rclonePaths[:i], r.rclonePaths[i+1:]...)
			return nil
		}
	}
	return nil
}

func (r *Runtime) syncRcloneFile(path string) error {
	f, e := os.Open(path)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer f.Close()
	tokens := map[string]OAuthToken{}
	grants := map[string]string{}
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 4096), 1<<20)
	section := ""
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "vp_credential_id" {
			grants[section] = strings.TrimSpace(value)
			continue
		}
		if !ok || strings.TrimSpace(key) != "token" {
			continue
		}
		var token OAuthToken
		if e = json.Unmarshal([]byte(strings.TrimSpace(value)), &token); e != nil || token.AccessToken == "" {
			return errors.New("rclone 刷新凭据格式无效")
		}
		tokens[section] = token
	}
	if e = scan.Err(); e != nil {
		return e
	}
	accounts, e := r.Store.All(context.Background(), "accounts")
	if e != nil {
		return e
	}
	r.credentialsMu.Lock()
	defer r.credentialsMu.Unlock()
	for _, raw := range accounts {
		var a Account
		if e = json.Unmarshal(raw, &a); e != nil {
			return e
		}
		t, ok := tokens[a.Remote]
		if !ok {
			continue
		}
		credential, e := r.loadCredential(a.ID)
		old := credential.Token
		if e != nil {
			return e
		}
		if grants[a.Remote] != credential.GrantID {
			continue
		}
		if t.AccessToken != old.AccessToken && t.Expiry.After(old.Expiry) {
			if t.RefreshToken == "" {
				t.RefreshToken = old.RefreshToken
			}
			if e = r.saveToken(a.ID, t); e != nil {
				return e
			}
		}
	}
	return nil
}
