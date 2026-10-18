package platform

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func performanceAccount(t *testing.T, h *testHarness) Account {
	t.Helper()
	res := h.call("POST", "/api/accounts", map[string]any{"name": "Family"}, true)
	if res.Code != 201 {
		t.Fatal(res.Body.String())
	}
	var a Account
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		t.Fatal(err)
	}
	if err := h.r.saveToken(a.ID, OAuthToken{AccessToken: "test", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestDriveAndProfileReadsOverlap(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := performanceAccount(t, h)
	listStarted, profileStarted := make(chan struct{}), make(chan struct{})
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1.0/me/drives" {
			close(listStarted)
			select {
			case <-profileStarted:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return cloudResponse(200, `{"value":[{"id":"drive","driveType":"personal"}]}`), nil
		}
		close(profileStarted)
		select {
		case <-listStarted:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return cloudResponse(200, `{"mail":"family@outlook.com"}`), nil
	})}
	res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
	if res.Code != 200 {
		t.Fatal(res.Body.String())
	}
	var result struct{ Account Account }
	json.Unmarshal(res.Body.Bytes(), &result)
	if result.Account.AccountEmail != "family@outlook.com" {
		t.Fatal("profile and list did not overlap")
	}
	// Known identity does not require a second optional profile request.
	h.r.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/v1.0/me/drives" {
			t.Error("repeated optional profile lookup")
		}
		return cloudResponse(200, `{"value":[]}`), nil
	})
	if res = h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false); res.Code != 200 {
		t.Fatal(res.Body.String())
	}
}

func TestSlowOptionalProfileDoesNotBlockDrives(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := performanceAccount(t, h)
	canceled := make(chan struct{})
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1.0/me/drives" {
			return cloudResponse(200, `{"value":[]}`), nil
		}
		<-req.Context().Done()
		close(canceled)
		return nil, req.Context().Err()
	})}
	started := time.Now()
	res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
	if res.Code != 200 || time.Since(started) > profileTimeout+2*time.Second {
		t.Fatalf("slow optional profile blocked list: %s", res.Body.String())
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("profile not canceled")
	}
}

func TestConcurrentReauthorizationRejectsOldDriveList(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := performanceAccount(t, h)
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/v1.0/me/drives" {
			if _, err := h.r.installCredential(req.Context(), a.ID, a.Version, h.r.customCredential(OAuthToken{AccessToken: "new", Expiry: time.Now().Add(time.Hour)}), "test"); err != nil {
				t.Error(err)
			}
			return cloudResponse(200, `{"value":[{"id":"old","driveType":"personal"}]}`), nil
		}
		return cloudResponse(200, `{"mail":"old@outlook.com"}`), nil
	})}
	res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
	if res.Code != 409 {
		t.Fatal("stale authorization response accepted: " + res.Body.String())
	}
	h.r.Store.Get(t.Context(), "accounts", a.ID, &a)
	if a.AccountEmail != "" || len(h.r.recentDrives) != 0 {
		t.Fatal("stale identity or list retained")
	}
}

func TestRecentDriveSelectionStillVerifiesFreshCloudAccess(t *testing.T) {
	for _, invalidate := range []string{"none", "expired", "edited", "reauthorized"} {
		t.Run(invalidate, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			a := performanceAccount(t, h)
			var lists, roots, quotas atomic.Int64
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1.0/me/drives":
					lists.Add(1)
					return cloudResponse(200, `{"value":[{"id":"drive","driveType":"personal"}]}`), nil
				case "/v1.0/me":
					return cloudResponse(403, `{}`), nil
				case "/v1.0/drives/drive/root":
					roots.Add(1)
					return cloudResponse(200, `{"id":"root"}`), nil
				case "/v1.0/drives/drive":
					quotas.Add(1)
					return cloudResponse(200, `{"id":"drive","quota":{"total":1000,"used":100,"remaining":900}}`), nil
				default:
					return cloudResponse(200, `{"folder":{},"size":20}`), nil
				}
			})}
			res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
			if res.Code != 200 {
				t.Fatal(res.Body.String())
			}
			switch invalidate {
			case "expired":
				entry := h.r.recentDrives[a.ID]
				entry.Expires = time.Now().Add(-time.Second)
				h.r.recentDrives[a.ID] = entry
			case "edited":
				a.Name = "edited"
				version, err := h.r.Store.Save(t.Context(), "accounts", a.ID, a.Version, a)
				if err != nil {
					t.Fatal(err)
				}
				a.Version = version
			case "reauthorized":
				var err error
				a, err = h.r.installCredential(t.Context(), a.ID, a.Version, h.r.customCredential(OAuthToken{AccessToken: "new", Expiry: time.Now().Add(time.Hour)}), "test")
				if err != nil {
					t.Fatal(err)
				}
			}
			res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "drive"}, true)
			expected := int64(2)
			if invalidate == "none" {
				expected = 1
			}
			if res.Code != 200 || lists.Load() != expected || roots.Load() != 1 || quotas.Load() != 1 {
				t.Fatalf("verify bypassed fresh access or queried list unnecessarily: %d lists=%d root=%d quota=%d %s", res.Code, lists.Load(), roots.Load(), quotas.Load(), res.Body.String())
			}
		})
	}
}

func TestQuotaReadsOverlapAndRejectIncompleteResponse(t *testing.T) {
	for _, valid := range []bool{true, false} {
		h := newPendingCloudHarness(t, adminPassword)
		a := performanceAccount(t, h)
		a.DriveID = "drive"
		a.TotalBytes = 77
		quotaStarted, projectStarted := make(chan struct{}), make(chan struct{})
		h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path == "/v1.0/drives/drive" {
				close(quotaStarted)
				select {
				case <-projectStarted:
				case <-time.After(2 * time.Second):
					t.Error("quota reads serialized")
				}
				if !valid {
					return cloudResponse(200, `{"id":"drive","quota":{"total":1000}}`), nil
				}
				return cloudResponse(200, `{"id":"drive","quota":{"total":1000,"used":100,"remaining":900}}`), nil
			}
			close(projectStarted)
			select {
			case <-quotaStarted:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
			return cloudResponse(200, `{"size":20}`), nil
		})}
		err := h.r.cloudQuota(t.Context(), &a)
		if valid && (err != nil || a.FreeBytes != 900 || a.ProjectUsedBytes != 20) {
			t.Fatalf("quota: %+v %v", a, err)
		}
		if !valid && (err == nil || a.TotalBytes != 77) {
			t.Fatal("incomplete quota accepted or partially applied")
		}
	}
}

func TestDriveCacheBounded(t *testing.T) {
	r := &Runtime{}
	for i := 0; i < 256; i++ {
		r.rememberDrives(Account{ID: newID(), Version: 1}, nil, nil)
	}
	if len(r.recentDrives) != 128 {
		t.Fatalf("unbounded cache: %d", len(r.recentDrives))
	}
}

func TestCachedDrivesCannotAuthorizeUnknownOrUnsupportedSelection(t *testing.T) {
	h := newPendingCloudHarness(t, adminPassword)
	a := performanceAccount(t, h)
	h.r.rememberDrives(a, []Drive{{ID: "metadata", DriveType: "documentLibrary", Supported: false}}, nil)
	h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("invalid selection contacted Graph: %s", req.URL.Path)
		return cloudResponse(500, `{}`), nil
	})}
	for _, id := range []string{"unknown", "metadata"} {
		res := h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": id}, true)
		if res.Code != 400 {
			t.Fatalf("cached invalid selection accepted: %s", res.Body.String())
		}
	}
}
