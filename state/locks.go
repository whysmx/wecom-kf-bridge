package state

import "sync"

// keyedLocks serializes work per key and reclaims the entry once no goroutine
// holds or waits for it, so the map size is bounded by in-flight keys rather
// than by every customer/scope ever seen.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*refLock
}
type refLock struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedLocks) acquire(key string) func() {
	k.mu.Lock()
	if k.m == nil {
		k.m = map[string]*refLock{}
	}
	l := k.m[key]
	if l == nil {
		l = &refLock{}
		k.m[key] = l
	}
	l.refs++
	k.mu.Unlock()
	l.mu.Lock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Unlock()
			k.mu.Lock()
			l.refs--
			if l.refs == 0 {
				delete(k.m, key)
			}
			k.mu.Unlock()
		})
	}
}
func (k *keyedLocks) size() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}
