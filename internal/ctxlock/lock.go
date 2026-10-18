// Package ctxlock provides zero-value locks with cancellable acquisition.
package ctxlock

import (
	"context"
	"sync"
)

// Mutex must not be copied after first use.
type Mutex struct {
	once  sync.Once
	token chan struct{}
}

func (m *Mutex) LockContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.once.Do(func() { m.token = make(chan struct{}, 1) })
	select {
	case m.token <- struct{}{}:
		if err := ctx.Err(); err != nil {
			m.Unlock()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Mutex) Lock() { _ = m.LockContext(context.Background()) }

func (m *Mutex) Unlock() {
	select {
	case <-m.token:
	default:
		panic("ctxlock: unlock of unlocked mutex")
	}
}

// RWMutex allows concurrent readers. A writer holding admission prevents new
// readers from entering while existing readers drain. It cannot be upgraded.
// Like sync.RWMutex, it must not be copied after first use.
type RWMutex struct {
	admission Mutex
	room      Mutex
	mu        sync.Mutex
	readers   int
}

func (m *RWMutex) LockContext(ctx context.Context) error {
	if err := m.admission.LockContext(ctx); err != nil {
		return err
	}
	if err := m.room.LockContext(ctx); err != nil {
		m.admission.Unlock()
		return err
	}
	return nil
}

func (m *RWMutex) Lock() { _ = m.LockContext(context.Background()) }

func (m *RWMutex) Unlock() {
	m.room.Unlock()
	m.admission.Unlock()
}

func (m *RWMutex) RLockContext(ctx context.Context) error {
	if err := m.admission.LockContext(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	// Holding admission excludes writers. With no readers, room is free, so
	// this acquisition cannot wait on another operation while mu is held.
	if m.readers == 0 {
		if err := m.room.LockContext(ctx); err != nil {
			m.mu.Unlock()
			m.admission.Unlock()
			return err
		}
	}
	m.readers++
	m.mu.Unlock()
	m.admission.Unlock()
	if err := ctx.Err(); err != nil {
		m.RUnlock()
		return err
	}
	return nil
}

func (m *RWMutex) RLock() { _ = m.RLockContext(context.Background()) }

func (m *RWMutex) RUnlock() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.readers == 0 {
		panic("ctxlock: runlock of unlocked rwmutex")
	}
	m.readers--
	if m.readers == 0 {
		m.room.Unlock()
	}
}
