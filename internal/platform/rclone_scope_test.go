package platform

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRcloneConfigForIsScopedAndIsolatesBrokenAccounts(t *testing.T) {
	h := newHarness(t, false)
	ctx := context.Background()
	good := Account{ID: newID(), Name: "停用但可维护", Backend: "onedrive", Remote: "vp_good", DriveID: "good-drive", DriveType: "personal", Verified: true}
	bad := Account{ID: newID(), Name: "授权损坏账号", Backend: "onedrive", Remote: "vp_bad", DriveID: "bad-drive", DriveType: "personal", Verified: true, Enabled: true}
	for _, a := range []Account{good, bad} {
		if _, err := h.r.Store.Save(ctx, "accounts", a.ID, 0, a); err != nil {
			t.Fatal(err)
		}
	}
	for id, token := range map[string]OAuthToken{
		good.ID: {AccessToken: "good-private-access", RefreshToken: "good-private-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)},
		bad.ID:  {AccessToken: "bad-private-access", TokenType: "Bearer", Expiry: time.Now().Add(-time.Hour)},
	} {
		if err := h.r.saveCredential(id, accountCredential{Mode: "rclone", Token: token}); err != nil {
			t.Fatal(err)
		}
	}
	path, err := h.r.RcloneConfigFor(ctx, good.ID, good.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer h.r.ReleaseRcloneConfig(path)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(content), "[vp_good]") != 1 || strings.Contains(string(content), "vp_bad") || strings.Contains(string(content), "bad-private") {
		t.Fatal("配置未隔离请求账号或未去重")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("配置权限不受限：%v", err)
	}
	if _, err = h.r.RcloneConfigFor(ctx, bad.ID); err == nil || !strings.Contains(err.Error(), bad.Name) || strings.Contains(err.Error(), "private") {
		t.Fatalf("未保留安全的账号错误：%v", err)
	}
	var issue authorizationIssue
	if !errors.As(err, &issue) || issue.category != "authorization_required" {
		t.Fatalf("授权错误语义丢失：%v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = h.r.RcloneConfigFor(cancelled, good.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("未响应取消：%v", err)
	}
	if _, err = h.r.RcloneConfigFor(ctx); err == nil {
		t.Fatal("空账号列表被接受")
	}
}

func TestRcloneConfigForAllowsQuotaUpdatesButRejectsRemoteChanges(t *testing.T) {
	for _, changeRemote := range []bool{false, true} {
		t.Run(map[bool]string{false: "quota", true: "remote"}[changeRemote], func(t *testing.T) {
			h := newHarness(t, false)
			ctx := context.Background()
			a := Account{ID: newID(), Name: "并发更新账号", Backend: "onedrive", Remote: "vp_initial", DriveID: "drive", DriveType: "personal", Verified: true}
			if _, err := h.r.Store.Save(ctx, "accounts", a.ID, 0, a); err != nil {
				t.Fatal(err)
			}
			if err := h.r.saveToken(a.ID, OAuthToken{AccessToken: "expired-private", RefreshToken: "refresh-private", Expiry: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatal(err)
			}
			h.r.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var current Account
				if err := h.r.Store.Get(ctx, "accounts", a.ID, &current); err != nil {
					t.Fatal(err)
				}
				current.FreeBytes = 42
				if changeRemote {
					current.Remote = "vp_changed"
				}
				if _, err := h.r.Store.Save(ctx, "accounts", a.ID, current.Version, current); err != nil {
					t.Fatal(err)
				}
				return cloudResponse(200, `{"access_token":"fresh-private","token_type":"Bearer","expires_in":3600}`), nil
			})
			path, err := h.r.RcloneConfigFor(ctx, a.ID)
			if changeRemote {
				if err == nil || !strings.Contains(err.Error(), a.Name) {
					t.Fatalf("未拒绝 remote 更改：%v", err)
				}
			} else if err != nil {
				t.Fatalf("配额版本更新不应阻止配置：%v", err)
			} else {
				h.r.ReleaseRcloneConfig(path)
			}
		})
	}
}

func TestRcloneConfigForPropagatesSyncFailure(t *testing.T) {
	h := newHarness(t, false)
	path, err := h.r.credentialFile("rclone-test-*.conf", "[broken]\ntoken = malformed\n")
	if err != nil {
		t.Fatal(err)
	}
	h.r.rclonePaths = append(h.r.rclonePaths, path)
	if _, err = h.r.RcloneConfigFor(context.Background(), "unused"); err == nil || !strings.Contains(err.Error(), "刷新凭据格式无效") {
		t.Fatalf("凭据同步错误未保留：%v", err)
	}
}

func TestRcloneConfigForCancelsTokenRefresh(t *testing.T) {
	h := newHarness(t, false)
	a := Account{ID: newID(), Name: "等待刷新账号", Backend: "onedrive", Remote: "vp_wait", Verified: true}
	if _, err := h.r.Store.Save(context.Background(), "accounts", a.ID, 0, a); err != nil {
		t.Fatal(err)
	}
	if err := h.r.saveToken(a.ID, OAuthToken{AccessToken: "expired-private", RefreshToken: "refresh-private", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	h.r.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := h.r.RcloneConfigFor(ctx, a.ID)
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("未开始刷新")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("刷新未保留取消原因：%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("刷新未响应取消")
	}
}
