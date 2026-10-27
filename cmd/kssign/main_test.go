package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI runs run with fresh output buffers.
func runCLI(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// hexKey writes a key file holding the hex seed of a fresh key and returns
// the seed and the derived public key.
func hexKey(t *testing.T, path string) (seed []byte, pub ed25519.PublicKey) {
	t.Helper()
	seed = make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, hex.EncodeToString(seed))
	return seed, ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

func TestSignVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	seed, pub := hexKey(t, keyFile)

	// The seed on disk must be usable by a fresh ed25519 key: pubkey
	// prints the same public key the seed derives.
	code, out, errOut := runCLI(t, "pubkey", keyFile)
	if code != 0 {
		t.Fatalf("pubkey = %d, want 0; stderr: %s", code, errOut)
	}
	if want := hex.EncodeToString(pub); out != want+"\n" {
		t.Fatalf("pubkey printed %q, want %q", out, want+"\n")
	}
	if got := len(seed); got != ed25519.SeedSize {
		t.Fatalf("seed = %d bytes, want %d", got, ed25519.SeedSize)
	}

	// Sign and verify must round-trip.
	dataFile := filepath.Join(dir, "SHA256SUMS")
	writeFile(t, dataFile, "version: v1.2.3\nabc  kids-shelf_linux_amd64.zip\n")
	sigFile := filepath.Join(dir, "SHA256SUMS.sig")
	code, sig, errOut := runCLI(t, "sign", keyFile, dataFile)
	if code != 0 {
		t.Fatalf("sign = %d, want 0; stderr: %s", code, errOut)
	}
	if len(strings.TrimSpace(sig)) != 2*ed25519.SignatureSize {
		t.Fatalf("signature = %d hex digits, want %d", len(strings.TrimSpace(sig)), 2*ed25519.SignatureSize)
	}
	writeFile(t, sigFile, strings.TrimSpace(sig)+"\n") // newline must be tolerated

	code, _, errOut = runCLI(t, "verify", hex.EncodeToString(pub), dataFile, sigFile)
	if code != 0 {
		t.Fatalf("verify = %d, want 0; stderr: %s", code, errOut)
	}

	// Tampering with the signed file must fail verification.
	tampered := dataFile + ".tampered"
	writeFile(t, tampered, "version: v1.2.3\nffff  kids-shelf_linux_amd64.zip\n")
	if code, _, _ := runCLI(t, "verify", hex.EncodeToString(pub), tampered, sigFile); code == 0 {
		t.Fatal("verify accepted a signature over tampered data")
	}

	// Tampering with the signature file must fail verification.
	badSig := filepath.Join(dir, "bad.sig")
	if sigHex, _ := hex.DecodeString(strings.TrimSpace(sig)); len(sigHex) == ed25519.SignatureSize {
		sigHex[0] ^= 0xFF
		writeFile(t, badSig, hex.EncodeToString(sigHex))
		if code, _, _ := runCLI(t, "verify", hex.EncodeToString(pub), dataFile, badSig); code == 0 {
			t.Fatal("verify accepted a corrupted signature")
		}
	}

	// A wrong-length or non-hex public key is refused, not silently false.
	if code, _, _ := runCLI(t, "verify", "abcd", dataFile, sigFile); code == 0 {
		t.Fatal("verify accepted a non-key public key")
	}
}

func TestWrongKeyFails(t *testing.T) {
	dir := t.TempDir()
	keyA := filepath.Join(dir, "keyA")
	keyB := filepath.Join(dir, "keyB")
	hexKey(t, keyA)
	_, pubB := hexKey(t, keyB)

	dataFile := filepath.Join(dir, "data")
	writeFile(t, dataFile, "some release checksums\n")
	sigFile := filepath.Join(dir, "sig")
	code, sig, errOut := runCLI(t, "sign", keyA, dataFile)
	if code != 0 {
		t.Fatalf("sign = %d, want 0; stderr: %s", code, errOut)
	}
	writeFile(t, sigFile, strings.TrimSpace(sig))

	// The same signature under the other key must not verify.
	if code, _, _ := runCLI(t, "verify", hex.EncodeToString(pubB), dataFile, sigFile); code == 0 {
		t.Fatal("verify accepted a signature made with a different key")
	}
}

func TestMalformedKeyFileRefused(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct{ name, content string }{
		{"short", "deadbeef"},
		{"not-hex", strings.Repeat("z", 64)},
		{"trailing-junk", hex.EncodeToString(make([]byte, ed25519.SeedSize)) + " extra"},
		{"empty", ""},
	} {
		keyFile := filepath.Join(dir, tc.name)
		writeFile(t, keyFile, tc.content)
		for _, args := range [][]string{
			{"pubkey", keyFile},
			{"sign", keyFile, filepath.Join(dir, "absent")},
		} {
			if code, _, errOut := runCLI(t, args...); code == 0 || errOut == "" {
				t.Fatalf("%s: %v on a malformed key file: exit %d, stderr %q; want a failure that says why", tc.name, args, code, errOut)
			}
		}
	}
}

func TestPubkeyToleratesTrailingNewline(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	writeFile(t, keyFile, hex.EncodeToString(seed)+"\n") // as an editor would leave it

	code, out, errOut := runCLI(t, "pubkey", keyFile)
	if code != 0 {
		t.Fatalf("pubkey = %d, want 0; stderr: %s", code, errOut)
	}
	want := hex.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	if strings.TrimSpace(out) != want {
		t.Fatalf("pubkey printed %q, want %q", out, want)
	}
}

func TestGenkey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")

	code, _, errOut := runCLI(t, "genkey", keyFile)
	if code != 0 {
		t.Fatalf("genkey = %d, want 0; stderr: %s", code, errOut)
	}
	raw, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2*ed25519.SeedSize {
		t.Fatalf("key file = %d bytes, want a bare %d-hex-digit seed", len(raw), 2*ed25519.SeedSize)
	}
	if _, err := hex.DecodeString(string(raw)); err != nil {
		t.Fatalf("key file is not hex: %v", err)
	}
	fi, err := os.Stat(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %o, want 0600", fi.Mode().Perm())
	}

	// A fresh key must be able to sign and verify.
	dataFile := filepath.Join(dir, "data")
	writeFile(t, dataFile, "checksums\n")
	_, sig, _ := runCLI(t, "sign", keyFile, dataFile)
	_, pub, _ := runCLI(t, "pubkey", keyFile)
	sigFile := filepath.Join(dir, "sig")
	writeFile(t, sigFile, strings.TrimSpace(sig))
	if code, _, errOut := runCLI(t, "verify", strings.TrimSpace(pub), dataFile, sigFile); code != 0 {
		t.Fatalf("verify = %d, want 0; stderr: %s", code, errOut)
	}
}

func TestGenkeyRefusesExistingPath(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	writeFile(t, keyFile, strings.Repeat("a", 64))

	before, _ := os.ReadFile(keyFile)
	code, _, errOut := runCLI(t, "genkey", keyFile)
	if code == 0 {
		t.Fatal("genkey overwrote an existing key file")
	}
	if !strings.Contains(errOut, "refusing to overwrite") {
		t.Fatalf("stderr %q does not explain the refusal", errOut)
	}
	after, _ := os.ReadFile(keyFile)
	if !bytes.Equal(before, after) {
		t.Fatal("genkey changed the existing key file")
	}
}

func TestUsage(t *testing.T) {
	if code, _, _ := runCLI(t); code == 0 {
		t.Fatal("no arguments must be a usage error")
	}
	if code, _, _ := runCLI(t, "bogus"); code == 0 {
		t.Fatal("an unknown command must be a usage error")
	}
	for _, args := range [][]string{
		{"genkey"},
		{"pubkey"},
		{"sign", "only-one"},
		{"verify", "a", "b"},
	} {
		if code, _, _ := runCLI(t, args...); code != 2 {
			t.Fatalf("%v = %d, want usage error 2", args, code)
		}
	}
	if code, _, _ := runCLI(t, "--help"); code != 0 {
		t.Fatal("--help must exit 0")
	}
}
