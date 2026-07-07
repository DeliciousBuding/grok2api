package statsig

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

// TestGenerateValid proves Generate yields a well-formed 70-byte statsig that is
// internally self-consistent — exactly what grok recomputes and validates.
func TestGenerateValid(t *testing.T) {
	mu.RLock()
	seed := append([]byte(nil), curSeed...)
	hex := curHEX
	mu.RUnlock()

	out, err := Generate("/rest/app-chat/conversations/new", "POST", time.Now().Unix())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	t.Logf("x-statsig-id = %s", out)

	raw, err := base64.RawStdEncoding.DecodeString(out)
	if err != nil || len(raw) != 70 {
		t.Fatalf("bad statsig (%d bytes): %v", len(raw), err)
	}
	key := raw[0]
	for i := 0; i < 48; i++ {
		if raw[1+i]^key != seed[i] {
			t.Fatalf("seed byte %d mismatch", i)
		}
	}
	number := uint32(raw[49]^key) | uint32(raw[50]^key)<<8 | uint32(raw[51]^key)<<16 | uint32(raw[52]^key)<<24
	input := "POST!/rest/app-chat/conversations/new!" + strconv.FormatUint(uint64(number), 10) + statsigSalt + hex
	sum := sha256.Sum256([]byte(input))
	for i := 0; i < 16; i++ {
		if raw[53+i]^key != sum[i] {
			t.Fatalf("sha byte %d mismatch — would be code:7", i)
		}
	}
	if raw[69]^key != statsigMark {
		t.Fatalf("tail marker = %d want 3", raw[69]^key)
	}
	t.Logf("OK: valid & SHA-consistent (number=%d, key=%d)", number, key)
}

// TestBuildOutputDependsOnKey verifies the encoded statsig changes with the
// one-byte XOR key without relying on probabilistic collision-prone randomness.
func TestBuildOutputDependsOnKey(t *testing.T) {
	mu.RLock()
	seed := append([]byte(nil), curSeed...)
	hex := curHEX
	mu.RUnlock()
	now := time.Now().Unix()
	a, err := buildWithKey(seed, hex, "/rest/app-chat/conversations/new", "POST", now, 1)
	if err != nil {
		t.Fatalf("build with key 1: %v", err)
	}
	b, err := buildWithKey(seed, hex, "/rest/app-chat/conversations/new", "POST", now, 2)
	if err != nil {
		t.Fatalf("build with key 2: %v", err)
	}
	if a == b {
		t.Fatal("different XOR keys produced identical statsig")
	}
}

func TestRotatePairGeneratesFreshValidPair(t *testing.T) {
	mu.RLock()
	oldSeed := append([]byte(nil), curSeed...)
	oldHEX := curHEX
	mu.RUnlock()
	t.Cleanup(func() {
		mu.Lock()
		curSeed = oldSeed
		curHEX = oldHEX
		mu.Unlock()
	})

	RotatePair()

	mu.RLock()
	seed := append([]byte(nil), curSeed...)
	hex := curHEX
	mu.RUnlock()
	if bytes.Equal(seed, oldSeed) {
		t.Fatal("RotatePair reused the previous seed")
	}
	if hex == "" || hex == oldHEX {
		t.Fatalf("RotatePair did not compute a fresh HEX fingerprint, got %q", hex)
	}
	out, err := buildWithKey(seed, hex, "/rest/app-chat/conversations/new", "POST", time.Now().Unix(), 7)
	if err != nil {
		t.Fatalf("fresh pair should build a statsig: %v", err)
	}
	raw, err := base64.RawStdEncoding.DecodeString(out)
	if err != nil || len(raw) != 70 {
		t.Fatalf("fresh pair produced invalid statsig length=%d err=%v", len(raw), err)
	}
}
