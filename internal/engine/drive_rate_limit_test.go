package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDriveBackupLimitEvidenceAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lines []string
		code  string
	}{
		{"none", []string{"context canceled", "403 insufficientPermissions"}, ""},
		{"project", []string{"403 rateLimitExceeded"}, "CLOUD_API_RATE_LIMITED"},
		{"user", []string{"403 userRateLimitExceeded"}, "CLOUD_API_RATE_LIMITED"},
		{"details", []string{`"reason": "RATE_LIMIT_EXCEEDED"`}, "CLOUD_API_RATE_LIMITED"},
		{"daily", []string{"Received upload limit error: User rate limit exceeded."}, "CLOUD_DAILY_UPLOAD_LIMIT"},
		{"daily-priority", []string{"403 rateLimitExceeded", "Received upload limit error: User rate limit exceeded."}, "CLOUD_DAILY_UPLOAD_LIMIT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var limits driveBackupLimits
			for _, line := range tc.lines {
				limits.observe(line)
			}
			err := limits.failure(t.Context())
			if tc.code == "" {
				if err != nil {
					t.Fatalf("unexpected limit: %v", err)
				}
			} else {
				var f *failure
				if !errors.As(err, &f) || f.Code != tc.code || f.Retryable != (tc.code == "CLOUD_API_RATE_LIMITED") {
					t.Fatalf("limit: %#v (%v)", f, err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if err := limits.failure(ctx); err != nil {
				t.Fatalf("limit replaced task cancellation: %v", err)
			}
		})
	}
}

func TestBackupRetryDelaySeparatesMinuteLimits(t *testing.T) {
	quota := &failure{Code: "CLOUD_API_RATE_LIMITED", Retryable: true}
	ordinary := &failure{Code: "BACKUP_ENGINE_FAILED", Retryable: true}
	for i, want := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute} {
		attempt := i + 1
		if got := backupRetryDelay(quota, attempt); got != want {
			t.Fatalf("quota retry %d: got %s, want %s", attempt, got, want)
		}
		if got := backupRetryDelay(ordinary, attempt); got != time.Second*time.Duration(1<<uint(attempt)) {
			t.Fatalf("ordinary retry %d: got %s", attempt, got)
		}
	}
}

func TestBackupRetryWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- waitBackupRetry(ctx, 5*time.Minute) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("quota cooldown did not stop after cancellation")
	}
	if err := waitBackupRetry(t.Context(), 0); err != nil {
		t.Fatalf("elapsed wait: %v", err)
	}
}

func TestBackupQuotaRetryPersistsStageAndSafeMessage(t *testing.T) {
	s := cancellationService(t)
	task := Task{ID: uid(), Kind: "backup", Status: "running"}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	quota := &failure{Code: "CLOUD_API_RATE_LIMITED", Message: "private-upload-session", Retryable: true}
	if err := s.waitBackupRetry(ctx, &task, quota, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancelled wait: %v", err)
	}
	var stored Task
	if err := s.rt.Store.Get(t.Context(), "backup_runs", task.ID, &stored); err != nil || stored.Stage != "retry_wait" || stored.Attempt != 1 {
		t.Fatalf("missing retry stage: %+v %v", stored, err)
	}
	var line string
	if err := s.rt.Store.DB.QueryRowContext(t.Context(), "SELECT message FROM task_logs WHERE task_id=? ORDER BY id DESC LIMIT 1", task.ID).Scan(&line); err != nil {
		t.Fatalf("missing retry log: %v", err)
	}
	if strings.Contains(line, "private-upload-session") || !strings.Contains(line, "60 秒") {
		t.Fatalf("unexpected retry message: %s", line)
	}
}
