package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSStore(dir)
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}

	in := SessionState{
		Adapter:     "opencode",
		SessionKey:  "task/abc 123",
		SessionID:   "ses_123",
		CWD:         "/tmp/work",
		CreatedAtMs: 1000,
		UpdatedAtMs: 2000,
	}
	if err := fs.SaveSessionState(in); err != nil {
		t.Fatalf("SaveSessionState failed: %v", err)
	}

	path, err := fs.SessionPath("opencode", "task/abc 123")
	if err != nil {
		t.Fatalf("SessionPath failed: %v", err)
	}
	if !strings.HasPrefix(path, filepath.Join(dir, "sessions", "opencode")) {
		t.Fatalf("path = %q, want under sessions/opencode", path)
	}

	out, ok, err := fs.LoadSessionState("opencode", "task/abc 123")
	if err != nil {
		t.Fatalf("LoadSessionState failed: %v", err)
	}
	if !ok {
		t.Fatal("expected session state to exist")
	}
	if out.SessionID != "ses_123" {
		t.Fatalf("SessionID = %q, want ses_123", out.SessionID)
	}
	if out.SessionKey != "task/abc 123" {
		t.Fatalf("SessionKey = %q, want task/abc 123", out.SessionKey)
	}
	if out.Adapter != "opencode" {
		t.Fatalf("Adapter = %q, want opencode", out.Adapter)
	}
}

func TestLoadSessionStateMissing(t *testing.T) {
	fs := NewFSStore(t.TempDir())

	_, ok, err := fs.LoadSessionState("opencode", "missing")
	if err != nil {
		t.Fatalf("LoadSessionState failed: %v", err)
	}
	if ok {
		t.Fatal("expected missing session state")
	}
}

func TestDeleteSessionState(t *testing.T) {
	dir := t.TempDir()
	fs := NewFSStore(dir)
	if err := fs.EnsureLayout(); err != nil {
		t.Fatalf("EnsureLayout failed: %v", err)
	}

	if err := fs.SaveSessionState(SessionState{
		Adapter:     "opencode",
		SessionKey:  "session-1",
		SessionID:   "ses_1",
		CreatedAtMs: 1,
		UpdatedAtMs: 1,
	}); err != nil {
		t.Fatalf("SaveSessionState failed: %v", err)
	}

	if err := fs.DeleteSessionState("opencode", "session-1"); err != nil {
		t.Fatalf("DeleteSessionState failed: %v", err)
	}

	_, ok, err := fs.LoadSessionState("opencode", "session-1")
	if err != nil {
		t.Fatalf("LoadSessionState failed: %v", err)
	}
	if ok {
		t.Fatal("expected session state to be deleted")
	}
}
