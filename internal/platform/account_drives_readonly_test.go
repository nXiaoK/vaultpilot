package platform

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestVerificationPersistsOptionalIdentityAfterCloudChecks(t *testing.T) {
	for _, mode := range []string{"direct", "cached", "expired", "profile_expired", "expired_during_verification", "reauthorized", "permission_denied"} {
		t.Run(mode, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			a := performanceAccount(t, h)
			var lists, profiles atomic.Int64
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1.0/me/drives":
					lists.Add(1)
					return cloudResponse(200, `{"value":[{"id":"drive","driveType":"personal"}]}`), nil
				case "/v1.0/me":
					count := profiles.Add(1)
					if mode == "permission_denied" {
						return cloudResponse(403, `{}`), nil
					}
					if mode == "reauthorized" && count > 1 {
						return cloudResponse(200, `{"displayName":"New member","mail":"new@outlook.com"}`), nil
					}
					return cloudResponse(200, `{"displayName":"Family member","userPrincipalName":"member@outlook.com"}`), nil
				case "/v1.0/drives/drive/root":
					if mode == "expired_during_verification" {
						entry := h.r.recentDrives[a.ID]
						entry.Expires = time.Now().Add(-time.Second)
						h.r.recentDrives[a.ID] = entry
					}
					return cloudResponse(200, `{"id":"root"}`), nil
				case "/v1.0/drives/drive":
					return cloudResponse(200, `{"id":"drive","quota":{"total":1000,"used":100,"remaining":900}}`), nil
				default:
					return cloudResponse(200, `{"folder":{},"size":20}`), nil
				}
			})}
			if mode != "direct" && mode != "permission_denied" {
				res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
				if res.Code != 200 {
					t.Fatal(res.Body.String())
				}
				if mode == "expired" {
					entry := h.r.recentDrives[a.ID]
					entry.Expires = time.Now().Add(-time.Second)
					h.r.recentDrives[a.ID] = entry
				}
				if mode == "profile_expired" {
					h.r.recentDrives[a.ID].Profile.Expires = time.Now().Add(-time.Second)
					// Refreshing the list must not extend an older profile's TTL.
					h.r.rememberDrives(a, h.r.recentDrives[a.ID].Items, h.r.recentDrives[a.ID].Profile)
				}
				if mode == "reauthorized" {
					var err error
					a, err = h.r.installCredential(t.Context(), a.ID, a.Version, h.r.customCredential(OAuthToken{AccessToken: "new", Expiry: time.Now().Add(time.Hour)}), "test")
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			res := h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "drive"}, true)
			if res.Code != 200 {
				t.Fatal(res.Body.String())
			}
			var result Account
			if err := json.Unmarshal(res.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			var persisted Account
			if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(result, persisted) || !persisted.Verified || persisted.Version != a.Version+1 {
				t.Fatalf("verification did not persist exactly once: %+v", persisted)
			}
			if mode == "permission_denied" {
				if persisted.AccountEmail != "" || persisted.AccountDisplayName != "" {
					t.Fatal("unavailable optional identity was persisted")
				}
			} else if mode == "reauthorized" {
				if persisted.AccountEmail != "new@outlook.com" || persisted.AccountDisplayName != "New member" {
					t.Fatalf("stale authorization identity persisted: %+v", persisted)
				}
			} else if persisted.AccountEmail != "member@outlook.com" || persisted.AccountDisplayName != "Family member" {
				t.Fatalf("verified identity missing: %+v", persisted)
			}
			wantLists, wantProfiles := int64(1), int64(1)
			if mode == "expired" || mode == "reauthorized" {
				wantLists, wantProfiles = 2, 2
			}
			if mode == "expired_during_verification" || mode == "profile_expired" {
				wantProfiles = 2
			}
			if lists.Load() != wantLists || profiles.Load() != wantProfiles {
				t.Fatalf("unexpected request count: lists=%d profiles=%d", lists.Load(), profiles.Load())
			}
		})
	}
}

func TestVerificationFailureAndMissingCSRFDoNotPersistIdentity(t *testing.T) {
	for _, failure := range []string{"csrf", "access", "quota", "stale_authorization"} {
		t.Run(failure, func(t *testing.T) {
			h := newPendingCloudHarness(t, adminPassword)
			a := performanceAccount(t, h)
			var cloudChecks atomic.Int64
			h.r.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/v1.0/me/drives":
					return cloudResponse(200, `{"value":[{"id":"drive","driveType":"personal"}]}`), nil
				case "/v1.0/me":
					return cloudResponse(200, `{"mail":"unverified@outlook.com"}`), nil
				case "/v1.0/drives/drive/root":
					cloudChecks.Add(1)
					if failure == "access" {
						return cloudResponse(403, `{}`), nil
					}
					return cloudResponse(200, `{"id":"root"}`), nil
				case "/v1.0/drives/drive":
					return cloudResponse(200, `{"id":"drive","quota":{"total":1000}}`), nil
				default:
					return cloudResponse(200, `{"folder":{},"size":20}`), nil
				}
			})}
			res := h.call("GET", "/api/accounts/"+a.ID+"/drives", nil, false)
			if res.Code != 200 {
				t.Fatal(res.Body.String())
			}
			before := a
			if failure == "stale_authorization" {
				var err error
				before, err = h.r.installCredential(t.Context(), a.ID, a.Version, h.r.customCredential(OAuthToken{AccessToken: "new", Expiry: time.Now().Add(time.Hour)}), "test")
				if err != nil {
					t.Fatal(err)
				}
			}
			res = h.call("POST", "/api/accounts/"+a.ID+"/verify", map[string]any{"version": a.Version, "drive_id": "drive"}, failure != "csrf")
			wantStatus := 409
			if failure == "csrf" {
				wantStatus = 403
			} else if failure == "quota" {
				wantStatus = 502
			}
			if res.Code != wantStatus {
				t.Fatalf("unexpected failure: %d %s", res.Code, res.Body.String())
			}
			var persisted Account
			if err := h.r.Store.Get(t.Context(), "accounts", a.ID, &persisted); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, persisted) {
				t.Fatalf("failed verification changed account: %+v", persisted)
			}
			if (failure == "csrf" || failure == "stale_authorization") && cloudChecks.Load() != 0 {
				t.Fatal("rejected verification contacted cloud storage")
			}
		})
	}
}
