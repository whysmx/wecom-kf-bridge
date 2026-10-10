package state

import (
	"sync"
	"testing"
)

func TestKeyedLocksReclaimed(t *testing.T) {
	var k keyedLocks
	var wg sync.WaitGroup
	counter := 0
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			un := k.acquire("same")
			counter++
			un()
			un() // idempotent
		}()
	}
	wg.Wait()
	if counter != 200 {
		t.Fatalf("counter=%d", counter)
	}
	for i := 0; i < 1000; i++ {
		k.acquire(string(rune('a'+i%26)) + "x")()
	}
	if n := k.size(); n != 0 {
		t.Fatalf("locks not reclaimed: %d", n)
	}
}

func TestStoreLocksReclaimedAfterUse(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 50; i++ {
		_ = s.WithCustomerLock(string(rune('A'+i)), func() error { return nil })
	}
	if n := s.locks.size(); n != 0 {
		t.Fatalf("store locks leaked: %d", n)
	}
}
