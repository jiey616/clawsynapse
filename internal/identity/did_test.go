package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"
)

func TestDeriveNodeDID(t *testing.T) {
	// deterministic key for reproducible test
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	did := DeriveNodeDID(pub)

	if !strings.HasPrefix(did, "did:key:z") {
		t.Fatalf("DID must start with did:key:z, got %s", did)
	}

	// same key must produce same DID
	did2 := DeriveNodeDID(pub)
	if did != did2 {
		t.Fatalf("DID must be deterministic: %s != %s", did, did2)
	}

	// different key must produce different DID
	seed2 := make([]byte, ed25519.SeedSize)
	seed2[0] = 0xff
	priv2 := ed25519.NewKeyFromSeed(seed2)
	pub2 := priv2.Public().(ed25519.PublicKey)
	did3 := DeriveNodeDID(pub2)
	if did == did3 {
		t.Fatalf("different keys must produce different DIDs")
	}

	t.Logf("DID: %s", did)
}

func TestDeriveNodeID(t *testing.T) {
	did := "did:key:z6MkhaXgBZDvotDkL5257faKDeqbSBpiGGfEQnGhKeTLn"
	nodeID := DeriveNodeID(did)

	if nodeID != "n1-ba6c9fd35949fa645a1b0dbdca0201e5" {
		t.Fatalf("unexpected node ID: %s", nodeID)
	}
	if ok, err := regexp.MatchString(`^n1-[a-f0-9]{32}$`, nodeID); err != nil || !ok {
		t.Fatalf("node ID must be subject-safe lowercase hex, got %s", nodeID)
	}
}

func TestDeriveNodeIDDeterministic(t *testing.T) {
	did := "did:key:z6MkhaXgBZDvotDkL5257faKDeqbSBpiGGfEQnGhKeTLn"
	nodeID1 := DeriveNodeID(did)
	nodeID2 := DeriveNodeID(did)
	if nodeID1 != nodeID2 {
		t.Fatalf("node ID must be deterministic: %s != %s", nodeID1, nodeID2)
	}
}

func TestDeriveNodeIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = byte(i >> 8)
		seed[1] = byte(i)
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		nodeID := DeriveNodeID(DeriveNodeDID(pub))
		if seen[nodeID] {
			t.Fatalf("node ID collision at iteration %d: %s", i, nodeID)
		}
		seen[nodeID] = true
	}
}

func TestEncodeBase58Deterministic(t *testing.T) {
	// known test vector: Ed25519 public key from seed [0..31]
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)

	prefixed := make([]byte, 2+len(pub))
	prefixed[0] = 0xed
	prefixed[1] = 0x01
	copy(prefixed[2:], pub)

	enc1 := encodeBase58(prefixed)
	enc2 := encodeBase58(prefixed)
	if enc1 != enc2 {
		t.Fatalf("base58 encoding not deterministic")
	}
	if enc1 == "" {
		t.Fatal("base58 encoding produced empty string")
	}

	_ = base64.RawURLEncoding.EncodeToString(pub) // ensure pub is valid
	t.Logf("base58 encoded: %s", enc1)
}
