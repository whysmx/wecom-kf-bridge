package runtime

import (
	"errors"
	"fmt"
	"os"
)

// ErrAlreadyRunning: another gateway process holds the database lock.
// Two processes on one SQLite database would both run sync and delivery
// workers and break per-customer ordering and the outbox fences (#26).
var ErrAlreadyRunning = errors.New("runtime: another instance is using this database")

// InstanceLock is an exclusive OS file lock held for the process lifetime.
// The OS releases it automatically if the process dies, so there is no
// stale-lock cleanup.
type InstanceLock struct{ f *os.File }

// AcquireInstanceLock locks dbPath+".lock" without blocking.
func AcquireInstanceLock(dbPath string) (*InstanceLock, error) {
	f, err := os.OpenFile(dbPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("runtime: instance lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("%w (%s.lock)", ErrAlreadyRunning, dbPath)
	}
	return &InstanceLock{f: f}, nil
}

func (l *InstanceLock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	_ = unlockFile(l.f)
	err := l.f.Close()
	l.f = nil
	return err
}
