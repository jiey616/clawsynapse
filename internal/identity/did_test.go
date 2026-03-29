package identity

import (
	"crypto/ed25519"
	"encoding/base64"
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

func TestShortID(t *testing.T) {
	did := "did:key:z6MkhaXgBZDvotDkL5257faKDeqbSBpiGGfEQnGhKeTLn"
	short := ShortID(did)

	if len(short) != 12 {
		t.Fatalf("ShortID must be 12 chars, got %d: %s", len(short), short)
	}
	if short != "z6MkhaXgBZDv" {
		t.Fatalf("unexpected ShortID: %s", short)
	}

	// non did:key input returns as-is
	plain := ShortID("node-alpha")
	if plain != "node-alpha" {
		t.Fatalf("non-DID input should pass through, got %s", plain)
	}
}

func TestShortIDUniqueness(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		seed := make([]byte, ed25519.SeedSize)
		seed[0] = byte(i >> 8)
		seed[1] = byte(i)
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		short := ShortID(DeriveNodeDID(pub))
		if seen[short] {
			t.Fatalf("ShortID collision at iteration %d: %s", i, short)
		}
		seen[short] = true
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
