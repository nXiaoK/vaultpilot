package platform

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCloudReaderAccountVerificationUsesOnlyReads(t *testing.T) {
	for _, connected := range []bool{false, true} {
		name := "pending"
		if connected {
			name = "connected"
		}
		t.Run(name, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			if connected {
				writer := newHarness(t, false)
				raw, err := writer.r.BuildCloudBootstrap(recoveryPassword)
				if err != nil {
					t.Fatal(err)
				}
				if err = h.r.ConnectCloudBootstrap(raw, recoveryPassword, writer.r.VaultID()); err != nil {
					t.Fatal(err)
				}
			}
			res := h.call("POST", "/api/accounts", map[string]any{"name": "original OneDrive"}, true)
			if res.Code != 201 {
				t.Fatalf("pending account create: %s", res.Body.String())
			}
			var a Account
			json.Unmarshal(res.Body.Bytes(), &a)
			if err := h.r.saveToken(a.ID, OAuthToken{AccessToken: "reader-access-token", RefreshToken: "reader-refresh-token", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}); err != nil {
				t.Fatal(err)
			}
			base := "/v1.0/drives/shared-personal-drive"
			projectPath := base + "/root:/VaultPilot"
			if connected {
				projectPath += "/" + h.r.VaultID()
			}
			calls := []string{}
			var callsMu sync.Mutex
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				callsMu.Lock()
				calls = append(calls, req.Method+" "+req.URL.Path)
				callsMu.Unlock()
				if req.Method != "GET" {
					t.Errorf("recovery account verification wrote to Graph: %s %s", req.Method, req.URL.Path)
					return cloudResponse(405, ""), nil
				}
				if req.Header.Get("Authorization") != "Bearer reader-access-token" {
					t.Error("node-local authorization was not used")
				}
				if strings.HasSuffix(req.URL.Path, "/VaultPilot/") || strings.Contains(req.URL.Path, "//") || strings.Contains(req.URL.Path, ".verification-") {
					t.Errorf("empty vault or probe path: %s", req.URL.Path)
				}
				switch req.URL.Path {
				case "/v1.0/me/drives":
					return cloudResponse(200, `{"value":[{"id":"shared-personal-drive","name":"Documents","driveType":"personal"}]}`), nil
				case base + "/root":
					return cloudResponse(200, `{"id":"root","folder":{}}`), nil
				case base:
					return cloudResponse(200, `{"id":"shared-personal-drive","quota":{"total":10000000,"used":100000,"remaining":9900000}}`), nil
				case base + "/root:/VaultPilot":
					return cloudResponse(200, `{"id":"project","folder":{},"size":4096}`), nil
				default:
					if connected && req.URL.Path == projectPath {
						return cloudResponse(200, `{"id":"vault","folder":{},"size":2048}`), nil
					}
					t.Errorf("unexpected Graph request: %s", req.URL.Path)
					return cloudResponse(404, ""), nil
				}
			})}
			res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "shared-personal-drive"}, true)
			if res.Code != 200 {
				t.Fatalf("read-only verify: %d %s", res.Code, res.Body.String())
			}
			json.Unmarshal(res.Body.Bytes(), &a)
			expectedSize := int64(4096)
			if connected {
				expectedSize = 2048
			}
			if !a.Verified || a.Status != "ready" || a.DriveID != "shared-personal-drive" || a.Region != "global" || a.TenantID != "consumers" || a.QuotaDomain != "global:personal:shared-personal-drive" || a.FreeBytes != 9900000 || a.ProjectUsedBytes != expectedSize {
				t.Fatalf("verification lost cloud identity or quota: %+v", a)
			}
			if len(calls) != 5 {
				t.Fatalf("unexpected verification calls: %v", calls)
			}
			if !connected && (!h.r.CloudPending() || h.r.VaultID() != "" || h.r.Keys.Unlocked()) {
				t.Fatal("account verification initialized a vault or unlocked the pending reader")
			}
		})
	}
}

func TestPendingCloudAccountRejectsMissingBackupRootWithoutCreatingIt(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := Account{ID: newID(), Backend: "onedrive", DriveID: "missing-drive"}
	if err := h.r.saveToken(a.ID, OAuthToken{AccessToken: "reader-access-token", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != "GET" {
			t.Error("missing backup directory was created by recovery node")
		}
		if req.URL.Path == "/v1.0/drives/missing-drive/root" {
			return cloudResponse(200, `{"id":"root","folder":{}}`), nil
		}
		if req.URL.Path != "/v1.0/drives/missing-drive/root:/VaultPilot" {
			t.Errorf("unexpected request: %s", req.URL.Path)
		}
		return cloudResponse(404, ""), nil
	})}
	if err := h.r.verifyCloud(context.Background(), &a); err == nil || a.Verified || requests != 2 {
		t.Fatalf("missing backup root accepted: verified=%v requests=%d error=%v", a.Verified, requests, err)
	}
}

func TestLocalRecoveryAccountVerificationPreservesReadOnlyBackupTree(t *testing.T) {
	for _, connected := range []bool{false, true} {
		name := "pending"
		if connected {
			name = "connected"
		}
		t.Run(name, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			if connected {
				writer := newHarness(t, false)
				raw, err := writer.r.BuildCloudBootstrap(recoveryPassword)
				if err != nil {
					t.Fatal(err)
				}
				if err = h.r.ConnectCloudBootstrap(raw, recoveryPassword, writer.r.VaultID()); err != nil {
					t.Fatal(err)
				}
			}
			remote := t.TempDir()
			vault := h.r.VaultID()
			if vault == "" {
				vault = strings.Repeat("a", 32)
			}
			catalog := filepath.Join(remote, "VaultPilot", vault, "catalog")
			if err := os.MkdirAll(catalog, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(catalog, "existing.ok")
			if err := os.WriteFile(marker, []byte("existing catalog"), 0400); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(remote, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(remote, 0700) })
			a := Account{ID: newID(), Backend: "local", LocalPath: remote}
			if err := h.r.verifyLocal(context.Background(), &a); err != nil {
				t.Fatalf("read-only local backup cannot be verified: %v", err)
			}
			canonical, err := filepath.EvalSymlinks(remote)
			if err != nil {
				t.Fatal(err)
			}
			if !a.Verified || a.Status != "ready" || a.LocalPath != canonical || a.DriveID != "local:"+canonical || a.QuotaDomain != a.DriveID || a.Region != "local" || a.DriveType != "local" || a.ProjectUsedBytes != int64(len("existing catalog")) {
				t.Fatalf("wrong local Drive mapping or size: %+v", a)
			}
			entries, err := os.ReadDir(remote)
			if err != nil || len(entries) != 1 || entries[0].Name() != "VaultPilot" {
				t.Fatalf("verification created a probe or modified tree: %v %v", entries, err)
			}
			content, err := os.ReadFile(marker)
			if err != nil || string(content) != "existing catalog" {
				t.Fatal("existing cloud catalog changed during verification")
			}
		})
	}
}
