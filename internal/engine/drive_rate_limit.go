package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

type driveBackupLimits struct {
	dailyUpload bool
	apiRequests bool
}

func (l *driveBackupLimits) observe(line string) {
	text := strings.ToLower(line)
	// A user request-rate error alone does not prove the daily upload cap.
	// Rclone's stop_on_upload_limit path reports the cap as "User rate limit
	// exceeded."; the compact API reason userRateLimitExceeded is not enough.
	l.dailyUpload = l.dailyUpload || strings.Contains(text, "upload limit") || strings.Contains(text, "user rate limit exceeded")
	userRate := strings.Contains(text, "userratelimitexceeded") || strings.Contains(text, "user_rate_limit_exceeded")
	projectRate := strings.Contains(text, "ratelimitexceeded") || strings.Contains(text, "rate_limit_exceeded")
	l.apiRequests = l.apiRequests || userRate || projectRate
}

func (l driveBackupLimits) failure(ctx context.Context) error {
	if ctx.Err() != nil {
		return nil
	}
	if l.dailyUpload {
		return fail("CLOUD_DAILY_UPLOAD_LIMIT", "Google Drive 上传上限已触发，请等待下一次计划继续备份")
	}
	if l.apiRequests {
		return &failure{Code: "CLOUD_API_RATE_LIMITED", Message: "Google Drive API 请求限流导致本次操作未完成；请降低并发或使用独立 Google OAuth 项目后重试", Retryable: true}
	}
	return nil
}

func backupRetryDelay(err error, attempt int) time.Duration {
	attempt = max(1, min(attempt, 8))
	var f *failure
	if errors.As(err, &f) && f.Code == "CLOUD_API_RATE_LIMITED" {
		return min(time.Minute*time.Duration(1<<uint(attempt-1)), 5*time.Minute)
	}
	return time.Second * time.Duration(1<<uint(attempt))
}

func (s *Service) waitBackupRetry(ctx context.Context, t *Task, err error, attempt int) error {
	delay := backupRetryDelay(err, attempt)
	t.Attempt = attempt
	s.stage(t, "retry_wait")
	var f *failure
	if errors.As(err, &f) && f.Code == "CLOUD_API_RATE_LIMITED" {
		s.log(t, fmt.Sprintf("Google Drive API 请求限流，等待 %d 秒后进行第 %d 次备份重试", int(delay.Seconds()), attempt))
	}
	return waitBackupRetry(ctx, delay)
}

func waitBackupRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
