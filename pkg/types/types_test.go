package types

import "testing"

func TestPeerJSONTags(t *testing.T) {
	p := Peer{NodeID: "test-node", AuthStatus: AuthAuthenticated}
	if p.NodeID != "test-node" {
		t.Fatalf("expected nodeID 'test-node', got %q", p.NodeID)
	}
	if p.AuthStatus != AuthAuthenticated {
		t.Fatalf("expected auth status %q, got %q", AuthAuthenticated, p.AuthStatus)
	}
}

func TestResultSuccess(t *testing.T) {
	r := APIResult{OK: true, Code: "ok"}
	if !r.OK {
		t.Fatal("expected ok")
	}
	if r.Code != "ok" {
		t.Fatalf("expected code 'ok', got %q", r.Code)
	}
}
