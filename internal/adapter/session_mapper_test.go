package adapter

import (
	"log/slog"
	"testing"

	"clawsynapse/internal/store"
)

func newTestSessionMapper(t *testing.T) (*sessionMapper, *store.FSStore) {
	t.Helper()
	fs := store.NewFSStore(t.TempDir())
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	return newSessionMapper("hermes", fs, slog.Default()), fs
}

func diskSession(t *testing.T, fs *store.FSStore, key string) (store.SessionState, bool) {
	t.Helper()
	st, ok, err := fs.LoadSessionState("hermes", key)
	if err != nil {
		t.Fatalf("LoadSessionState(%s): %v", key, err)
	}
	return st, ok
}

// TestSessionMapper_LoadHitsMemoryBeforeFlush: a save is immediately
// visible to loads without any disk write (the whole point of T2.3).
func TestSessionMapper_LoadHitsMemoryBeforeFlush(t *testing.T) {
	m, fs := newTestSessionMapper(t)

	m.save("chat:n1", "sess-1")
	if got := m.load("chat:n1"); got != "sess-1" {
		t.Fatalf("load after save = %q, want sess-1 (memory visibility)", got)
	}
	if _, ok := diskSession(t, fs, "chat:n1"); ok {
		t.Fatal("disk entry exists before Flush, want deferred persistence")
	}
}

// TestSessionMapper_FlushPersistsAndKeepsDirtyOnFailurePath: Flush writes
// dirty entries to disk; a second Flush without mutations is a no-op
// (UpdatedAt stays).
func TestSessionMapper_FlushPersists(t *testing.T) {
	m, fs := newTestSessionMapper(t)

	m.save("chat:n1", "sess-1")
	m.Flush()
	st, ok := diskSession(t, fs, "chat:n1")
	if !ok || st.SessionID != "sess-1" {
		t.Fatalf("after Flush disk = %+v ok=%v, want sess-1", st, ok)
	}
	firstUpdated := st.UpdatedAtMs
	if firstUpdated == 0 {
		t.Fatal("UpdatedAtMs not set")
	}

	m.Flush() // clean entry: no rewrite
	st2, _ := diskSession(t, fs, "chat:n1")
	if st2.UpdatedAtMs != firstUpdated {
		t.Fatalf("clean Flush rewrote the file: %d -> %d", firstUpdated, st2.UpdatedAtMs)
	}
}

// TestSessionMapper_CreatedAtPreservedAcrossUpdate: updating a session id
// keeps the original creation timestamp (legacy save() behavior).
func TestSessionMapper_CreatedAtPreservedAcrossUpdate(t *testing.T) {
	m, fs := newTestSessionMapper(t)

	m.save("task:n1", "sess-a")
	m.Flush()
	first, _ := diskSession(t, fs, "task:n1")

	m.save("task:n1", "sess-b")
	m.Flush()
	second, ok := diskSession(t, fs, "task:n1")
	if !ok || second.SessionID != "sess-b" {
		t.Fatalf("after update disk = %+v ok=%v, want sess-b", second, ok)
	}
	if second.CreatedAtMs != first.CreatedAtMs {
		t.Fatalf("CreatedAtMs drifted: %d -> %d", first.CreatedAtMs, second.CreatedAtMs)
	}
}

// TestSessionMapper_DeleteTombstone: delete hides the id immediately,
// removes the file on flush, and stays invisible afterwards.
func TestSessionMapper_DeleteTombstone(t *testing.T) {
	m, fs := newTestSessionMapper(t)

	m.save("task:n1", "sess-a")
	m.Flush()
	if _, ok := diskSession(t, fs, "task:n1"); !ok {
		t.Fatal("precondition: disk entry missing after Flush")
	}

	m.delete("task:n1")
	if got := m.load("task:n1"); got != "" {
		t.Fatalf("load after delete = %q, want empty (immediate hide)", got)
	}
	if _, ok := diskSession(t, fs, "task:n1"); !ok {
		t.Fatal("disk file removed before Flush, want tombstoned until flush")
	}

	m.Flush()
	if _, ok := diskSession(t, fs, "task:n1"); ok {
		t.Fatal("disk file still present after Flush, want removed")
	}
	if got := m.load("task:n1"); got != "" {
		t.Fatalf("load after flushed delete = %q, want empty", got)
	}
}

// TestSessionMapper_CloseFlushesPending: process exit writes out mutations
// that were still inside the 30s window.
func TestSessionMapper_CloseFlushesPending(t *testing.T) {
	m, fs := newTestSessionMapper(t)

	m.save("chat:n1", "sess-late")
	m.Close()
	if _, ok := diskSession(t, fs, "chat:n1"); !ok {
		t.Fatal("Close did not flush pending save")
	}
}

// TestSessionMapper_LoadsCacheDiskValue: a disk-backed load is cached as a
// clean entry and repeated loads keep hitting memory.
func TestSessionMapper_LoadsCacheDiskValue(t *testing.T) {
	m, fs := newTestSessionMapper(t)

	m.save("chat:n1", "sess-1")
	m.Flush()

	m2 := newSessionMapper("hermes", fs, slog.Default())
	if got := m2.load("chat:n1"); got != "sess-1" {
		t.Fatalf("cold load = %q, want sess-1", got)
	}
	// Overwrite the disk file behind the mapper's back; the cached value
	// must keep winning (memory is the source of truth within a process).
	if err := fs.SaveSessionState(store.SessionState{Adapter: "hermes", SessionKey: "chat:n1", SessionID: "sess-hijack"}); err != nil {
		t.Fatalf("seed disk: %v", err)
	}
	if got := m2.load("chat:n1"); got != "sess-1" {
		t.Fatalf("warm load = %q, want cached sess-1", got)
	}
}

// TestHermesAdapter_SessionMappingIntegration: the adapter delegates its
// three mapping helpers to the sessionMapper and Close persists pending
// writes (T2.3 acceptance at the adapter level).
func TestHermesAdapter_SessionMappingIntegration(t *testing.T) {
	fs := store.NewFSStore(t.TempDir())
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout: %v", err)
	}
	a, err := NewHermesAdapter(HermesConfig{
		NodeID:       "n1",
		SessionStore: fs,
		Logger:       slog.Default(),
	})
	if err != nil {
		t.Fatalf("NewHermesAdapter: %v", err)
	}

	a.saveMappedSession("task:abc", "sess-9")
	if got := a.loadMappedSessionID("task:abc"); got != "sess-9" {
		t.Fatalf("loadMappedSessionID = %q, want sess-9", got)
	}
	a.Close()

	st, ok, err := fs.LoadSessionState("hermes", "task:abc")
	if err != nil || !ok || st.SessionID != "sess-9" {
		t.Fatalf("after Close disk = %+v ok=%v err=%v, want sess-9", st, ok, err)
	}
}
