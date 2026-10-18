package ctxlock

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func receive(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("lock acquisition did not finish")
		return nil
	}
}

func TestMutexCancellationAndReuse(t *testing.T) {
	var m Mutex
	m.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- m.LockContext(ctx) }()
	cancel()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiting acquisition: %v", err)
	}
	m.Unlock()
	for i := 0; i < 100; i++ {
		if err := m.LockContext(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled context acquired idle lock: %v", err)
		}
		if err := m.LockContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.Unlock()
	}
}

func TestMutexSerializesConcurrentUpdates(t *testing.T) {
	var m Mutex
	var wg sync.WaitGroup
	value := 0
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Lock()
				value++
				m.Unlock()
			}
		}()
	}
	wg.Wait()
	if value != 2000 {
		t.Fatalf("lost updates: %d", value)
	}
}

func TestRWConcurrentReadersAndCancelledWriter(t *testing.T) {
	var m RWMutex
	m.RLock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := m.RLockContext(ctx); err != nil {
		t.Fatalf("reader blocked another reader: %v", err)
	}
	m.RUnlock()
	writerCtx, cancelWriter := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- m.LockContext(writerCtx) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(m.admission.token) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer did not reach admission")
		}
		time.Sleep(time.Millisecond)
	}
	cancelWriter()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("writer cancellation: %v", err)
	}
	// The original reader is still active: cancelling the writer must release
	// admission so browsing does not wait for that reader to finish.
	if err := m.RLockContext(ctx); err != nil {
		t.Fatalf("cancelled writer stranded admission: %v", err)
	}
	m.RUnlock()
	m.RUnlock()
	if err := m.LockContext(ctx); err != nil {
		t.Fatalf("last reader stranded room: %v", err)
	}
	m.Unlock()
}

func TestRWCancelledReaderDoesNotAffectWriter(t *testing.T) {
	var m RWMutex
	m.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- m.RLockContext(ctx) }()
	cancel()
	if err := receive(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("reader cancellation: %v", err)
	}
	m.Unlock()
	m.RLock()
	m.RUnlock()
	m.Lock()
	m.Unlock()
}

func TestRWCompetingReadersAndWriters(t *testing.T) {
	var m RWMutex
	var wg sync.WaitGroup
	value := 0
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.Lock()
				value++
				m.Unlock()
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.RLock()
				if value < 0 {
					t.Error("invalid protected value")
				}
				m.RUnlock()
			}
		}()
	}
	wg.Wait()
	if value != 1000 {
		t.Fatalf("lost writes: %d", value)
	}
}
