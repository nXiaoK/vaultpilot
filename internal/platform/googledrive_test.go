package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func googleTestAccount(t *testing.T, h *testHarness) Account {
	t.Helper()
	res := h.call("POST", "/api/accounts", map[string]any{"name": "Google One", "backend": "gdrive"}, true)
	if res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	var a Account
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := h.r.saveCredential(a.ID, accountCredential{Mode: "rclone", Provider: backendGoogleDrive, Token: OAuthToken{AccessToken: "google-test", RefreshToken: "google-refresh", Expiry: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestGoogleDriveListAndIdentityUseParallelReads(t *testing.T) {
	for _, limited := range []bool{true, false} {
		t.Run(map[bool]string{true: "known_quota", false: "unknown_quota"}[limited], func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			a := googleTestAccount(t, h)
			rootStarted, aboutStarted := make(chan struct{}), make(chan struct{})
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Host != "www.googleapis.com" || req.Header.Get("Authorization") != "Bearer google-test" || req.Method != "GET" {
					t.Error("invalid Google API request")
				}
				switch req.URL.Path {
				case "/drive/v3/files/root":
					close(rootStarted)
					select {
					case <-aboutStarted:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					return cloudResponse(200, `{"id":"google-root"}`), nil
				case "/drive/v3/about":
					close(aboutStarted)
					select {
					case <-rootStarted:
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
					quota := `"usage":"20"`
					if limited {
						quota += `,"limit":"1000"`
					}
					return cloudResponse(200, `{"user":{"displayName":"Google member","emailAddress":"member@gmail.com"},"storageQuota":{`+quota+`}}`), nil
				default:
					t.Errorf("unexpected Google request: %s", req.URL)
					return cloudResponse(404, ""), nil
				}
			})}
			res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
			if res.Code != 200 {
				t.Fatal(res.Body.String())
			}
			var result struct {
				Items   []Drive
				Account Account
			}
			if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Items) != 1 || result.Items[0].ID != "google-root" || result.Items[0].DriveType != "my_drive" || result.Items[0].Supported != limited || (result.Items[0].UnsupportedReason != "") == limited || result.Account.AccountEmail != "member@gmail.com" {
				t.Fatalf("invalid Drive listing: %+v", result)
			}
			var persisted Account
			if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &persisted); err != nil || persisted.AccountEmail != "" || persisted.Version != a.Version {
				t.Fatalf("GET changed persistent identity: %+v %v", persisted, err)
			}
			if !limited {
				res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"drive_id": "google-root", "version": a.Version}, true)
				if res.Code != 400 || !strings.Contains(res.Body.String(), "drive_type_unsupported") || !strings.Contains(res.Body.String(), "容量上限") {
					t.Fatalf("unknown quota permitted verification: %d %s", res.Code, res.Body.String())
				}
			}
		})
	}
}

func TestGoogleDriveRecoveryVerificationUsesOnlyReads(t *testing.T) {
	for _, connected := range []bool{false, true} {
		t.Run(map[bool]string{true: "connected", false: "pending"}[connected], func(t *testing.T) {
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
			a := googleTestAccount(t, h)
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" {
					t.Errorf("recovery verification wrote to Google: %s", req.Method)
				}
				switch req.URL.Path {
				case "/drive/v3/files/root", "/drive/v3/files/google-root":
					return cloudResponse(200, `{"id":"google-root"}`), nil
				case "/drive/v3/about":
					return cloudResponse(200, `{"user":{"emailAddress":"member@gmail.com"},"storageQuota":{"limit":"1000","usage":"120"}}`), nil
				case "/drive/v3/files":
					q := req.URL.Query().Get("q")
					if strings.Contains(q, "name = 'VaultPilot'") {
						return cloudResponse(200, `{"files":[{"id":"project","mimeType":"application/vnd.google-apps.folder"}]}`), nil
					}
					if connected && strings.Contains(q, "name = '"+h.r.VaultID()+"'") {
						return cloudResponse(200, `{"files":[{"id":"vault","mimeType":"application/vnd.google-apps.folder"}]}`), nil
					}
					if strings.Contains(q, "name =") {
						t.Errorf("invalid vault lookup: %s", q)
					}
					return cloudResponse(200, `{"files":[{"id":"backup","mimeType":"application/octet-stream","size":"35"}]}`), nil
				default:
					t.Errorf("unexpected Google request: %s", req.URL)
					return cloudResponse(404, ""), nil
				}
			})}
			res := h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"drive_id": "google-root", "version": a.Version}, true)
			if res.Code != 200 {
				t.Fatal(res.Body.String())
			}
			json.Unmarshal(res.Body.Bytes(), &a)
			if !a.Verified || a.Region != "google" || a.TenantID != "personal" || a.QuotaDomain != "google:personal:google-root" || a.FreeBytes != 880 || a.ProjectUsedBytes != 35 || a.AccountEmail != "member@gmail.com" {
				t.Fatalf("invalid verified account: %+v", a)
			}
			if !connected && (h.r.VaultID() != "" || h.r.Keys.Unlocked()) {
				t.Fatal("read-only verification initialized the vault")
			}
		})
	}
}

func TestGoogleDriveWriterCreatesProbesAndPermanentlyDeletes(t *testing.T) {
	h := newHarness(t, false)
	a := googleTestAccount(t, h)
	a.DriveID = "google-root"
	created := map[string]string{}
	var uploaded []byte
	deleted := false
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/drive/v3/files/google-root" {
			return cloudResponse(200, `{"id":"google-root"}`), nil
		}
		if req.URL.Path == "/drive/v3/about" {
			return cloudResponse(200, `{"storageQuota":{"limit":"1000","usage":"120"}}`), nil
		}
		if req.URL.Path == "/upload/drive/v3/files" {
			if req.Method != "POST" || req.URL.Query().Get("uploadType") != "multipart" {
				t.Error("invalid multipart upload")
			}
			kind, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil || kind != "multipart/related" {
				t.Fatalf("invalid multipart content type: %s", kind)
			}
			reader := multipart.NewReader(req.Body, params["boundary"])
			part, err := reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			metadata, _ := io.ReadAll(part)
			if !bytes.Contains(metadata, []byte(`"parents":["folder-`+h.r.VaultID()+`"]`)) {
				t.Fatalf("probe outside vault: %s", metadata)
			}
			part, err = reader.NextPart()
			if err != nil {
				t.Fatal(err)
			}
			uploaded, _ = io.ReadAll(part)
			return cloudResponse(200, `{"id":"probe"}`), nil
		}
		if req.URL.Path == "/drive/v3/files/probe" {
			if req.Method == "DELETE" {
				deleted = true
				return cloudResponse(204, ""), nil
			}
			if req.URL.Query().Get("alt") != "media" {
				t.Error("probe was not read as media")
			}
			return cloudResponse(200, string(uploaded)), nil
		}
		if req.URL.Path == "/drive/v3/files" {
			if req.Method == "POST" {
				var metadata struct {
					Name, MimeType string
					Parents        []string
				}
				if err := json.NewDecoder(req.Body).Decode(&metadata); err != nil || metadata.MimeType != googleFolderMIME || len(metadata.Parents) != 1 {
					t.Fatal("invalid folder metadata")
				}
				created[metadata.Name] = "folder-" + metadata.Name
				return cloudResponse(200, `{"id":"folder-`+metadata.Name+`"}`), nil
			}
			q := req.URL.Query().Get("q")
			for name, id := range created {
				if strings.Contains(q, "name = '"+name+"'") {
					return cloudResponse(200, `{"files":[{"id":"`+id+`","mimeType":"application/vnd.google-apps.folder"}]}`), nil
				}
			}
			return cloudResponse(200, `{"files":[]}`), nil
		}
		t.Errorf("unexpected request: %s %s", req.Method, req.URL)
		return cloudResponse(404, ""), nil
	})}
	if err := h.r.googleVerifyCloud(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 || len(uploaded) != 48 || !deleted || !a.Verified {
		t.Fatalf("incomplete verification: created=%v uploaded=%d deleted=%v", created, len(uploaded), deleted)
	}
}

func TestGoogleDriveDuplicateFolderAndConcurrentCreationCleanup(t *testing.T) {
	for _, mode := range []string{"existing", "new_empty", "new_nonempty"} {
		t.Run(mode, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			a := googleTestAccount(t, h)
			created, deleted := false, false
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == "POST" {
					created = true
					return cloudResponse(200, `{"id":"ours"}`), nil
				}
				if req.Method == "DELETE" {
					if req.URL.Path != "/drive/v3/files/ours" {
						t.Errorf("deleted foreign folder: %s", req.URL.Path)
					}
					deleted = true
					return cloudResponse(204, ""), nil
				}
				if strings.HasPrefix(req.URL.Query().Get("q"), "'ours' in parents") {
					if mode == "new_nonempty" {
						return cloudResponse(200, `{"files":[{"id":"data"}]}`), nil
					}
					return cloudResponse(200, `{"files":[]}`), nil
				}
				if mode != "existing" && !created {
					return cloudResponse(200, `{"files":[]}`), nil
				}
				if req.URL.Query().Get("pageToken") == "second" {
					return cloudResponse(200, `{"files":[{"id":"theirs","mimeType":"application/vnd.google-apps.folder"}]}`), nil
				}
				return cloudResponse(200, `{"nextPageToken":"second","files":[{"id":"ours","mimeType":"application/vnd.google-apps.folder"}]}`), nil
			})}
			_, err := h.r.googleEnsureFolder(t.Context(), a, "root", "VaultPilot")
			var duplicate duplicateCloudFolder
			if !errors.As(err, &duplicate) || deleted != (mode == "new_empty") || created != (mode != "existing") {
				t.Fatalf("unsafe duplicate handling: created=%v deleted=%v err=%v", created, deleted, err)
			}
			res := httptest.NewRecorder()
			cloudFailure(res, err)
			if res.Code != 409 || !strings.Contains(res.Body.String(), "duplicate_cloud_folder") {
				t.Fatalf("wrong duplicate error response: %s", res.Body.String())
			}
		})
	}
}

func TestGoogleDriveQuotaTraversesAllPagesAndFolders(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := googleTestAccount(t, h)
	a.DriveID = "root"
	var pages atomic.Int64
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/drive/v3/about" {
			return cloudResponse(200, `{"storageQuota":{"limit":"1000","usage":"1200"}}`), nil
		}
		q := req.URL.Query()
		if strings.Contains(q.Get("q"), "name = 'VaultPilot'") {
			return cloudResponse(200, `{"files":[{"id":"project","mimeType":"application/vnd.google-apps.folder"}]}`), nil
		}
		if q.Get("pageSize") != "1000" || !strings.Contains(q.Get("fields"), "nextPageToken") {
			t.Errorf("incomplete recursive list: %s", req.URL)
		}
		pages.Add(1)
		if strings.HasPrefix(q.Get("q"), "'nested'") {
			return cloudResponse(200, `{"files":[{"id":"c","size":"30"},{"id":"doc","mimeType":"application/vnd.google-apps.document"}]}`), nil
		}
		if q.Get("pageToken") == "second" {
			return cloudResponse(200, `{"files":[{"id":"b","size":"20"}]}`), nil
		}
		return cloudResponse(200, `{"nextPageToken":"second","files":[{"id":"a","size":"10"},{"id":"nested","mimeType":"application/vnd.google-apps.folder"}]}`), nil
	})}
	if err := h.r.googleQuota(t.Context(), &a); err != nil {
		t.Fatal(err)
	}
	if a.TotalBytes != 1000 || a.UsedBytes != 1000 || a.FreeBytes != 0 || a.ProjectUsedBytes != 60 || pages.Load() != 3 {
		t.Fatalf("wrong quota or recursive occupancy: %+v pages=%d", a, pages.Load())
	}
	h.r.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return cloudResponse(200, `{"storageQuota":{"usage":"20"}}`), nil
	})
	if err := h.r.googleQuota(t.Context(), &a); err == nil || !strings.Contains(err.Error(), "容量上限") || a.TotalBytes != 1000 {
		t.Fatalf("unknown quota accepted or modified account: %v %+v", err, a)
	}
}

func TestGoogleDriveErrorsClassifyQuotaWithoutAuthorizationIncidents(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	a := googleTestAccount(t, h)
	for _, test := range []struct {
		reason   string
		status   int
		code     string
		incident bool
	}{
		{"rateLimitExceeded", 429, "cloud_throttled", false},
		{"userRateLimitExceeded", 429, "cloud_throttled", false},
		{"dailyLimitExceeded", 429, "cloud_throttled", false},
		{"storageQuotaExceeded", 409, "cloud_storage_full", false},
		{"forbidden", 502, "cloud_access_failed", false},
		{"insufficientPermissions", 409, "authorization_required", true},
		{"authError", 409, "authorization_required", true},
	} {
		t.Run(test.reason, func(t *testing.T) {
			h.r.AccountAuthorizationHealthy(a.ID, true)
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return cloudResponse(403, `{"error":{"code":403,"errors":[{"reason":"`+test.reason+`"}]}}`), nil
			})}
			_, err := h.r.gdrive(t.Context(), a.ID, "GET", "/files/root", url.Values{"fields": {"id"}}, nil, "")
			var cloud cloudError
			if !errors.As(err, &cloud) || cloud.Reason != test.reason || cloud.Provider != "gdrive" {
				t.Fatalf("lost provider/reason: %v", err)
			}
			res := httptest.NewRecorder()
			cloudFailure(res, err)
			if res.Code != test.status || !strings.Contains(res.Body.String(), test.code) {
				t.Fatalf("wrong cloud failure: %d %s", res.Code, res.Body.String())
			}
			var incidents int
			if err := h.r.Store.DB.QueryRow("SELECT count(*) FROM notification_incidents WHERE account_id=?", a.ID).Scan(&incidents); err != nil || (incidents > 0) != test.incident {
				t.Fatalf("incorrect authorization incident: count=%d err=%v", incidents, err)
			}
		})
	}
}

func TestGoogleDriveReauthorizationRejectsChangedRoot(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := googleTestAccount(t, h)
	a.DriveID = "original-root"
	version, err := h.r.Store.Save(t.Context(), "accounts", a.ID, a.Version, a)
	if err != nil {
		t.Fatal(err)
	}
	a.Version = version
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/drive/v3/files/root":
			return cloudResponse(200, `{"id":"changed-root"}`), nil
		case "/drive/v3/about":
			return cloudResponse(200, `{"storageQuota":{"limit":"1000","usage":"20"}}`), nil
		default:
			t.Errorf("changed identity reached verification: %s", req.URL)
			return cloudResponse(404, ""), nil
		}
	})}
	res := h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "changed-root"}, true)
	if res.Code != 409 || !strings.Contains(res.Body.String(), "drive_identity_changed") {
		t.Fatal(res.Body.String())
	}
	var current Account
	h.r.Store.Get(t.Context(), "accounts", a.ID, &current)
	if current.DriveID != "original-root" || current.Version != a.Version || current.Verified {
		t.Fatalf("changed root persisted: %+v", current)
	}
}

func TestGoogleDriveMultipleErrorReasonsKeepQuotaOutOfAuthorizationAlerts(t *testing.T) {
	h := notificationHarness(t)
	enableNotifications(t, h)
	a := googleTestAccount(t, h)
	for _, tc := range []struct {
		reasons       []string
		want          string
		authorization bool
	}{
		{[]string{"forbidden", "authError"}, "authError", true},
		{[]string{"authError", "userRateLimitExceeded"}, "userRateLimitExceeded", false},
		{[]string{"userRateLimitExceeded", "authError"}, "userRateLimitExceeded", false},
		{[]string{"storageQuotaExceeded", "insufficientPermissions"}, "storageQuotaExceeded", false},
	} {
		h.r.AccountAuthorizationHealthy(a.ID, true)
		details := make([]map[string]string, 0, len(tc.reasons))
		for _, reason := range tc.reasons {
			details = append(details, map[string]string{"reason": reason})
		}
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"errors": details}})
		h.r.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return cloudResponse(403, string(body)), nil
		})}
		_, err := h.r.gdrive(t.Context(), a.ID, "GET", "/files/root", nil, nil, "")
		var cloud cloudError
		if !errors.As(err, &cloud) || cloud.Reason != tc.want || cloud.authorizationFailure() != tc.authorization {
			t.Fatalf("reasons %v: %v", tc.reasons, err)
		}
		var count int
		if err := h.r.Store.DB.QueryRow("SELECT count(*) FROM notification_incidents WHERE account_id=?", a.ID).Scan(&count); err != nil || (count > 0) != tc.authorization {
			t.Fatalf("reasons %v: count=%d err=%v", tc.reasons, count, err)
		}
	}
}

func TestGoogleDriveRecoveryRejectsMissingDuplicateOrOccupiedRoot(t *testing.T) {
	for _, mode := range []string{"missing", "duplicate", "occupied", "identity"} {
		t.Run(mode, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			a := googleTestAccount(t, h)
			a.DriveID = "google-root"
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method != "GET" {
					t.Errorf("read-only verification mutated cloud: %s", req.Method)
				}
				if req.URL.Path == "/drive/v3/files/google-root" {
					if mode == "identity" {
						return cloudResponse(200, `{"id":"different-root"}`), nil
					}
					return cloudResponse(200, `{"id":"google-root"}`), nil
				}
				if req.URL.Path != "/drive/v3/files" {
					t.Errorf("invalid root reached quota: %s", req.URL)
				}
				switch mode {
				case "duplicate":
					return cloudResponse(200, `{"files":[{"id":"a","mimeType":"application/vnd.google-apps.folder"},{"id":"b","mimeType":"application/vnd.google-apps.folder"}]}`), nil
				case "occupied":
					return cloudResponse(200, `{"files":[{"id":"file","mimeType":"application/octet-stream"}]}`), nil
				default:
					return cloudResponse(200, `{"files":[]}`), nil
				}
			})}
			if err := h.r.googleVerifyCloud(t.Context(), &a); err == nil || a.Verified {
				t.Fatalf("invalid recovery root accepted: %+v %v", a, err)
			}
		})
	}
}

func TestGoogleDriveRequestBoundsAndCanceledContext(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := googleTestAccount(t, h)
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return cloudResponse(200, strings.Repeat("x", (8<<20)+1)), nil
	})}
	if _, err := h.r.gdrive(t.Context(), a.ID, "GET", "/files", nil, nil, ""); err == nil || !strings.Contains(err.Error(), "响应过大") {
		t.Fatalf("oversized response accepted: %v", err)
	}
	if _, err := h.r.googleFindChild(t.Context(), a, "root", "x' or trashed = true"); err == nil {
		t.Fatal("unsafe directory name accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := h.r.gdrive(ctx, a.ID, "GET", "/files", nil, nil, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request lost context error: %v", err)
	}
}

func TestGoogleDriveConcurrentReauthorizationRejectsStaleDriveList(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := googleTestAccount(t, h)
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/drive/v3/files/root" {
			credential := accountCredential{Mode: "rclone", Provider: backendGoogleDrive, Token: OAuthToken{AccessToken: "replacement", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}}
			if _, err := h.r.installCredential(req.Context(), a.ID, a.Version, credential, "test"); err != nil {
				t.Error(err)
			}
			return cloudResponse(200, `{"id":"stale-root"}`), nil
		}
		return cloudResponse(200, `{"user":{"emailAddress":"stale@gmail.com"},"storageQuota":{"limit":"1000","usage":"20"}}`), nil
	})}
	res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
	if res.Code != 409 {
		t.Fatalf("stale identity accepted: %d %s", res.Code, res.Body.String())
	}
	var current Account
	if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &current); err != nil || current.AccountEmail != "" || len(h.r.recentDrives) != 0 {
		t.Fatalf("retained stale identity: %+v %v", current, err)
	}
}
