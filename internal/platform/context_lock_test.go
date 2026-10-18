package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAccountConfigurationContextCancellationPreservesLegacyLock(t *testing.T) {
	var rt Runtime
	release := rt.LockAccountConfiguration()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		unlock, err := rt.LockAccountConfigurationContext(ctx)
		if unlock != nil {
			unlock()
		}
		done <- err
	}()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait: %v", err)
		}
	case <-time.After(time.Second):
		release()
		t.Fatal("cancelled configuration wait did not return")
	}
	release()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unlock, err := rt.LockAccountConfigurationContext(ctx)
	if err != nil {
		t.Fatalf("legacy lock was leaked: %v", err)
	}
	unlock()
}

func TestContextConfigurationLockExcludesLegacyCaller(t *testing.T) {
	var rt Runtime
	release, err := rt.LockAccountConfigurationContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		unlock := rt.LockAccountConfiguration()
		unlock()
		close(done)
	}()
	select {
	case <-done:
		release()
		t.Fatal("legacy caller bypassed the context lock")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("legacy caller did not resume")
	}
}
