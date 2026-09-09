package library

import "sync"

// keyLocks hands out one lock per key, created on demand and dropped when the last
// waiter is gone. Keys are info hashes (exclusive) and show names, where a season pack
// takes the write side because it writes episodes the request never named, while a
// single-episode add takes the read side and its own episode key.
type keyLocks struct {
	mu      sync.Mutex
	locks   map[string]*sync.RWMutex
	waiters map[string]int
}

func newKeyLocks() *keyLocks {
	return &keyLocks{locks: map[string]*sync.RWMutex{}, waiters: map[string]int{}}
}

func (k *keyLocks) acquire(key string) *sync.RWMutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	lock, ok := k.locks[key]
	if !ok {
		lock = &sync.RWMutex{}
		k.locks[key] = lock
	}
	k.waiters[key]++
	return lock
}

func (k *keyLocks) done(key string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.waiters[key]--
	if k.waiters[key] <= 0 {
		delete(k.waiters, key)
		delete(k.locks, key)
	}
}

// Lock blocks until the key is exclusively held; the returned function releases it.
func (k *keyLocks) Lock(key string) func() {
	lock := k.acquire(key)
	lock.Lock()
	return func() {
		lock.Unlock()
		k.done(key)
	}
}

// RLock takes the shared side of the key.
func (k *keyLocks) RLock(key string) func() {
	lock := k.acquire(key)
	lock.RLock()
	return func() {
		lock.RUnlock()
		k.done(key)
	}
}

// TryLock returns false instead of waiting. Cleanup paths use it: whoever holds the key
// is working on that same hash right now, and their own cleanup covers it.
func (k *keyLocks) TryLock(key string) (func(), bool) {
	lock := k.acquire(key)
	if !lock.TryLock() {
		k.done(key)
		return nil, false
	}
	return func() {
		lock.Unlock()
		k.done(key)
	}, true
}
