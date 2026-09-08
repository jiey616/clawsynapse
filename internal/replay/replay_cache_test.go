package replay

import (
	"testing"
	"time"

	"clawsynapse/internal/store"
)

func newTestGuard(t *testing.T, dir string, maxEntries int, ttl time.Duration) *ReplayGuard {
	t.Helper()
	fs := store.NewFSStore(dir)
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	rg, err := NewReplayGuard(fs, maxEntries, ttl)
	if err != nil {
		t.Fatalf("NewReplayGuard: %v", err)
	}
	return rg
}

func TestReplayGuardDetectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	rg := newTestGuard(t, dir, 100, 10*time.Minute)

	if !rg.CheckAndRemember("k1", 0) {
		t.Fatal("first CheckAndRemember(k1) = duplicate, want fresh")
	}
	if rg.CheckAndRemember("k1", 0) {
		t.Fatal("second CheckAndRemember(k1) = fresh, want duplicate")
	}
	if !rg.CheckAndRemember("k2", 0) {
		t.Fatal("first CheckAndRemember(k2) = duplicate, want fresh")
	}
}

func TestReplayGuardPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	rg := newTestGuard(t, dir, 100, 10*time.Minute)
	if !rg.CheckAndRemember("nonce-1", 0) {
		t.Fatal("first CheckAndRemember(nonce-1) = duplicate, want fresh")
	}

	rg2 := newTestGuard(t, dir, 100, 10*time.Minute)
	if rg2.CheckAndRemember("nonce-1", 0) {
		t.Fatal("after reload CheckAndRemember(nonce-1) = fresh, want duplicate")
	}
}

func TestReplayGuardTTLExpiryAllowsReentry(t *testing.T) {
	dir := t.TempDir()
	rg := newTestGuard(t, dir, 100, 30*time.Millisecond)

	if !rg.CheckAndRemember("msg-x", 30*time.Millisecond) {
		t.Fatal("first CheckAndRemember(msg-x) = duplicate, want fresh")
	}
	if rg.CheckAndRemember("msg-x", 30*time.Millisecond) {
		t.Fatal("immediate repeat = fresh, want duplicate")
	}

	time.Sleep(60 * time.Millisecond)
	if !rg.CheckAndRemember("msg-x", 30*time.Millisecond) {
		t.Fatal("after TTL expiry = duplicate, want fresh (re-entry allowed)")
	}
}

func TestReplayGuardEvictsOldestAtCapacity(t *testing.T) {
	dir := t.TempDir()
	rg := newTestGuard(t, dir, 2, time.Minute)

	rg.CheckAndRemember("a", 0)
	rg.CheckAndRemember("b", 0)
	rg.CheckAndRemember("c", 0) // evicts "a" (earliest expiry)

	if rg.CheckAndRemember("b", 0) || rg.CheckAndRemember("c", 0) {
		t.Fatal("b/c should still be remembered")
	}
	if !rg.CheckAndRemember("a", 0) {
		t.Fatal("a should have been evicted, want fresh")
	}
}
