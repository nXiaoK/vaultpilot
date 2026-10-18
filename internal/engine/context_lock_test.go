package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"vaultpilot/internal/platform"
)

func awaitBlockedLock(t *testing.T, acquire func(context.Context) (func(), error)) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
		release, err := acquire(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		release()
		if time.Now().After(deadline) {
			t.Fatal("operation did not acquire its first lock")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCancelledMaintenanceHTTPWaitsReleaseOuterGates(t *testing.T) {
	for _, test := range []struct {
		name    string
		protect bool
		hold    func(*Service) func()
	}{
		{"protect catalog", true, func(s *Service) func() { s.catalogMu.Lock(); return s.catalogMu.Unlock }},
		{"protect configuration", true, func(s *Service) func() { s.configurationMu.Lock(); return s.configurationMu.Unlock }},
		{"clear preview catalog", false, func(s *Service) func() { s.catalogMu.Lock(); return s.catalogMu.Unlock }},
		{"clear preview account configuration", false, func(s *Service) func() { return s.rt.LockAccountConfiguration() }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := cancellationService(t)
			s.rt.Keys = &platform.Keyring{}
			if err := s.rt.Keys.SetMaster(make([]byte, 32)); err != nil {
				t.Fatal(err)
			}
			release := test.hold(s)
			defer func() {
				if release != nil {
					release()
				}
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := httptest.NewRequest("POST", "/", strings.NewReader(`{"account_ids":["a"],"protected":true,"version":1}`)).WithContext(ctx)
			res := httptest.NewRecorder()
			done := make(chan struct{})
			go func() {
				defer close(done)
				if test.protect {
					s.protect(res, req)
				} else {
					s.clearPreviewHTTP(res, req)
				}
			}()
			select {
			case <-done:
				t.Fatalf("handler bypassed held gate: %s", res.Body.String())
			case <-time.After(20 * time.Millisecond):
			}
			cancel()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("cancelled HTTP handler remained blocked")
			}
			if res.Body.Len() != 0 {
				t.Fatalf("cancelled wait started response processing: %s", res.Body.String())
			}
			release()
			release = nil
			checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := s.configurationMu.LockContext(checkCtx); err != nil {
				t.Fatalf("configuration read gate leaked: %v", err)
			}
			s.configurationMu.Unlock()
			unlock, err := s.rt.LockAccountConfigurationContext(checkCtx)
			if err != nil {
				t.Fatalf("outer account configuration gate leaked: %v", err)
			}
			unlock()
			if err = s.catalogMu.LockContext(checkCtx); err != nil {
				t.Fatalf("catalog gate leaked: %v", err)
			}
			s.catalogMu.Unlock()
			var writes int
			if err = s.rt.Store.DB.QueryRow("SELECT count(*) FROM audit_events").Scan(&writes); err != nil || writes != 0 {
				t.Fatalf("cancelled wait wrote audit state: %d %v", writes, err)
			}
		})
	}
}

func TestResourceLockCancellationReleasesPartialAcquisition(t *testing.T) {
	for _, blocked := range []string{"account:b", "repo:repo"} {
		t.Run(blocked, func(t *testing.T) {
			var s Service
			release, err := s.lock(context.Background(), blocked)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				unlock, err := s.lockResources(ctx, []string{"b", "a", "a"}, "repo")
				if unlock != nil {
					unlock()
				}
				done <- err
			}()
			awaitBlockedLock(t, func(ctx context.Context) (func(), error) { return s.lock(ctx, "account:a") })
			if blocked == "repo:repo" {
				awaitBlockedLock(t, func(ctx context.Context) (func(), error) { return s.lock(ctx, "account:b") })
			}
			cancel()
			select {
			case err = <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("resource wait cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("resource acquisition ignored cancellation")
			}
			checkCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			unlock, err := s.lock(checkCtx, "account:a")
			if err != nil {
				t.Fatalf("first account lock leaked: %v", err)
			}
			unlock()
			if blocked == "repo:repo" {
				unlock, err = s.lock(checkCtx, "account:b")
				if err != nil {
					t.Fatalf("second account lock leaked: %v", err)
				}
				unlock()
			}
		})
	}
	var s Service
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := s.lockResources(ctx, []string{"b", "a", "a"}, "repo")
	if err != nil {
		t.Fatalf("duplicate account IDs tried to lock the same mutex twice: %v", err)
	}
	unlock()
}

func TestRestoreReadLockAllowsFileBrowsing(t *testing.T) {
	s, req := permissionRestoreFixture(t)
	var point Point
	if err := s.rt.Store.Get(t.Context(), "recovery_points", req.PointID, &point); err != nil {
		t.Fatal(err)
	}
	point.Snapshots = []Snapshot{{RepositoryID: "repo", SnapshotID: strings.Repeat("a", 64)}}
	if _, err := s.rt.Store.Save(t.Context(), "recovery_points", point.ID, point.Version, point); err != nil {
		t.Fatal(err)
	}
	release, err := s.lock(context.Background(), "account:a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	raw, _ := json.Marshal(req)
	done := make(chan error, 1)
	go func() { done <- s.restore(ctx, &Task{ID: "blocked-restore", Kind: "restore", Request: raw}) }()
	deadline := time.Now().Add(time.Second)
	for {
		var task Task
		if err = s.rt.Store.Get(t.Context(), "backup_runs", "blocked-restore", &task); err == nil && task.Stage == "restoring" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restore did not reach the resource gate")
		}
		time.Sleep(time.Millisecond)
	}
	browseCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	browse := httptest.NewRequest("GET", "/?limit=30", nil).WithContext(browseCtx)
	browse.SetPathValue("id", point.ID)
	res := httptest.NewRecorder()
	s.pointFiles(res, browse)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), "nested/file.txt") {
		t.Fatalf("restore blocked file browsing: %d %s", res.Code, res.Body.String())
	}
	preview := httptest.NewRecorder()
	s.restorePreviewHTTP(preview, httptest.NewRequest("POST", "/", bytes.NewReader(raw)).WithContext(browseCtx))
	if preview.Code != http.StatusOK || !strings.Contains(preview.Body.String(), "file_count") {
		t.Fatalf("restore blocked preview: %d %s", preview.Code, preview.Body.String())
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("restore resource wait ignored cancellation: %v", err)
	}
	if err = s.recoveryIndex.LockContext(browseCtx); err != nil {
		t.Fatalf("restore leaked index reader: %v", err)
	}
	s.recoveryIndex.Unlock()
}

func TestCleanupIndexWriteWaitCanBeCancelled(t *testing.T) {
	s := cancellationService(t)
	s.recoveryIndex.RLock()
	defer s.recoveryIndex.RUnlock()
	insertCleanupEntries(t, s, "orphan", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.cleanupFileEntries(ctx, "1=1", nil) }()
	// A queued writer owns admission, so a new reader can no longer enter.
	// This proves cleanup reached the lock rather than only ctx.Err above it.
	awaitBlockedLock(t, func(ctx context.Context) (func(), error) {
		if err := s.recoveryIndex.RLockContext(ctx); err != nil {
			return nil, err
		}
		return s.recoveryIndex.RUnlock, nil
	})
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cleanup wait cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cleanup ignored cancellation while a reader held the index")
	}
	if got := cleanupEntryCount(t, s, "orphan"); got != 1 {
		t.Fatalf("cancelled writer removed index rows: %d", got)
	}
}
