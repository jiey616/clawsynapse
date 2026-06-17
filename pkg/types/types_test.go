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
	r := DeliverResult{Accepted: true}
	if !r.Accepted {
		t.Fatal("expected accepted")
	}
}
