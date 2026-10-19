package engine

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"vaultpilot/internal/platform"
)

func TestGoogleDriveBackupDailyLimitDoesNotRetry(t *testing.T) {
	for _, tc := range []struct {
		backend, status, output, code string
		retry                         bool
	}{
		{"gdrive", "ready", "403 userRateLimitExceeded", "CLOUD_DAILY_UPLOAD_LIMIT", false},
		{"gdrive", "ready", "upload limit reached", "CLOUD_DAILY_UPLOAD_LIMIT", false},
		{"gdrive", "ready", "403 insufficientPermissions", "BACKUP_ENGINE_FAILED", true},
		{"onedrive", "ready", "upload limit reached", "BACKUP_ENGINE_FAILED", true},
		{"gdrive", "new", "403 userRateLimitExceeded", "CLOUD_DAILY_UPLOAD_LIMIT", false},
		{"onedrive", "new", "upload limit reached", "REPOSITORY_INIT_FAILED", false},
	} {
		t.Run(tc.backend+"-"+tc.status+"-"+tc.output, func(t *testing.T) {
			s := catalogRefreshNode(t)
			// The stored local account supplies an empty task config. The account
			// passed to the engine still exercises the cloud Restic transport.
			stored := Account{ID: "cloud", Backend: "local", Verified: true}
			if _, err := s.rt.Store.Save(t.Context(), "accounts", stored.ID, 0, stored); err != nil {
				t.Fatal(err)
			}
			program := filepath.Join(t.TempDir(), "restic")
			if err := os.WriteFile(program, []byte("#!/bin/sh\nprintf '%s\\n' '"+tc.output+"' >&2\nexit 2\n"), 0700); err != nil {
				t.Fatal(err)
			}
			s.rt.Config.ResticBin = program
			password, err := s.rt.Keys.Seal([]byte("test-repository-password"))
			if err != nil {
				t.Fatal(err)
			}
			repo := Repository{ID: "repo", Path: "VaultPilot/test", Status: tc.status, Password: base64.StdEncoding.EncodeToString(password)}
			a := Account{ID: stored.ID, Backend: tc.backend, Remote: ":local"}
			_, err = s.backupShard(t.Context(), &Task{ID: "run"}, Plan{}, repo, a, nil, t.TempDir())
			var failure *failure
			if !errors.As(err, &failure) || failure.Code != tc.code || failure.Retryable != tc.retry {
				t.Fatalf("unexpected error: %#v (%v)", failure, err)
			}
		})
	}
}

func TestGoogleDriveQuotaUsesSharedUsageAndFastList(t *testing.T) {
	for _, tc := range []struct {
		name, quota       string
		total, free, used int64
		unknown           bool
	}{
		{"shared-usage", `{"total":100,"free":70,"used":5}`, 100, 70, 30, false},
		{"over-quota", `{"total":100,"free":-5,"used":105}`, 100, 0, 100, false},
		{"derive-used", `{"total":100,"free":70}`, 100, 70, 30, false},
		{"zero-capacity", `{"total":0,"free":0}`, 0, 0, 0, false},
		{"unknown-capacity", `{"free":70,"used":5}`, 0, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := catalogRefreshNode(t)
			stored := Account{ID: "cloud", Backend: "local", Verified: true}
			if _, err := s.rt.Store.Save(t.Context(), "accounts", stored.ID, 0, stored); err != nil {
				t.Fatal(err)
			}
			program := filepath.Join(t.TempDir(), "rclone")
			script := `#!/bin/sh
for arg do
  case "$arg" in
    about) printf '%s\n' '` + tc.quota + `'; exit 0 ;;
    size) for flag do [ "$flag" = --fast-list ] && fast=yes; done
          [ "$fast" = yes ] || exit 9
          printf '{"bytes":12}\n'; exit 0 ;;
  esac
done
exit 8
`
			if err := os.WriteFile(program, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			s.rt.Config.RcloneBin = program
			a := Account{ID: stored.ID, Backend: "gdrive", Remote: ":local"}
			err := s.refreshAccount(t.Context(), &a)
			if tc.unknown {
				var failure *failure
				if !errors.As(err, &failure) || failure.Code != "QUOTA_UNKNOWN" {
					t.Fatalf("unknown quota accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var got platform.Account
			if err := s.rt.Store.Get(t.Context(), "accounts", a.ID, &got); err != nil {
				t.Fatal(err)
			}
			if got.TotalBytes != tc.total || got.FreeBytes != tc.free || got.UsedBytes != tc.used || got.ProjectUsedBytes != 12 {
				t.Fatalf("quota: %+v", got)
			}
		})
	}
}

func TestGoogleDriveClearPreviewMarksPermanentDelete(t *testing.T) {
	s := catalogRefreshNode(t)
	for _, backend := range []string{"onedrive", "gdrive"} {
		a := Account{ID: backend, Backend: backend, DriveID: backend + "-root", Verified: true, Enabled: true}
		if _, err := s.rt.Store.Save(t.Context(), "accounts", a.ID, 0, a); err != nil {
			t.Fatal(err)
		}
	}
	p, err := s.buildClearPreview(t.Context(), []string{"gdrive", "onedrive"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Items) != 2 {
		t.Fatalf("items: %+v", p.Items)
	}
	for _, item := range p.Items {
		if item.PermanentDelete != (item.ID == "gdrive") {
			t.Fatalf("incorrect deletion mode: %+v", item)
		}
	}
}
