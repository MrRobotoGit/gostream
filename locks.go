package main

import (
	"sync"
	"time"
)

// lockEntry holds the mutex and a usage counter
type lockEntry struct {
	mu       sync.Mutex
	refCount int
}

// LockManager provides per-file locking to prevent metadata fetch stampedes.
// V140: Uses reference counting to prevent memory leaks (unbounded map growth).
// WARNING: Locks are NOT recursive. Calling Lock() twice on the same path within 
// the same goroutine call stack will cause a DEADLOCK.
type LockManager struct {
	locks map[string]*lockEntry // Per-file active locks
	mu    sync.Mutex            // Protects locks map
}

// NewLockManager creates a new lock manager.
// ttl parameter is kept for backward compatibility but ignored (locks are cleaned up immediately when unused).
func NewLockManager(ttl time.Duration) *LockManager {
	lm := &LockManager{
		locks: make(map[string]*lockEntry),
	}
	return lm
}

// Lock acquires a lock for the given path.
// If no lock exists for this path, creates one.
// Returns an unlock function that must be called when done.
func (lm *LockManager) Lock(path string) func() {
	lm.mu.Lock()
	entry, exists := lm.locks[path]
	if !exists {
		entry = &lockEntry{}
		lm.locks[path] = entry
	}
	entry.refCount++
	lm.mu.Unlock()

	// Acquire the per-file lock (may block)
	entry.mu.Lock()

	// Return unlock function
	return func() {
		entry.mu.Unlock()

		lm.mu.Lock()
		entry.refCount--
		if entry.refCount <= 0 {
			delete(lm.locks, path)
		}
		lm.mu.Unlock()
	}
}

// TryLock attempts to acquire a lock for the given path without blocking.
// Returns an unlock function and true if successful, or nil and false if already locked.
func (lm *LockManager) TryLock(path string) (func(), bool) {
	lm.mu.Lock()
	entry, exists := lm.locks[path]
	if !exists {
		entry = &lockEntry{}
		lm.locks[path] = entry
	}
	entry.refCount++
	lm.mu.Unlock()

	if entry.mu.TryLock() {
		return func() {
			entry.mu.Unlock()

			lm.mu.Lock()
			entry.refCount--
			if entry.refCount <= 0 {
				delete(lm.locks, path)
			}
			lm.mu.Unlock()
		}, true
	}

	// Failed to acquire lock, decrement refCount immediately
	lm.mu.Lock()
	entry.refCount--
	if entry.refCount <= 0 {
		delete(lm.locks, path)
	}
	lm.mu.Unlock()

	return nil, false
}

// Stop is a no-op
func (lm *LockManager) Stop() {
	// No-op
}

// LockStats structure
type LockStats struct {
	TotalLocks    int
}

// Stats returns current active lock statistics
func (lm *LockManager) Stats() LockStats {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	return LockStats{
		TotalLocks: len(lm.locks),
	}
}
