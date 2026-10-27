// Command kssign generates and uses the ed25519 key pair that signs
// kids-shelf releases and verifies the SHA256SUMS the updater downloads
// against (DECISIONS.md "Updates stay signed", internal/update). The
// private key is stored as the 32-byte seed in the 64-hex-character form
// that crypto/ed25519's NewKeyFromSeed consumes; it lives only on the
// release machine and is never shown.
//
// Usage:
//
//	kssign genkey <path>          write a fresh key file, mode 0600
//	kssign pubkey <keyfile>       print the public key (64 hex digits)
//	kssign sign <keyfile> <file>  print the signature over <file> (128 hex digits)
//	kssign verify <pubhex> <file> <sighexfile>
//	                              exit 0 only if the signature is valid
//
// The public key travels as hex because that is the form compiled into
// release builds (-X internal/update.PublicKeyHex=...); the signature
// travels as a file because SHA256SUMS.sig is a file.
//
// Format checks are strict on purpose: a release signed with a corrupted
// key would break updates on every family machine, so a key file that is
// not exactly 64 hex characters is refused rather than guessed at.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the CLI and returns the process exit code: 0 on success, 1
// on an invalid key, signature or file, 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "genkey":
		return cmdGenkey(args[1:], stderr)
	case "pubkey":
		return cmdPubkey(args[1:], stdout, stderr)
	case "sign":
		return cmdSign(args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(args[1:], stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "kssign: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

const usage = `kssign signs the kids-shelf release checksums with an ed25519 key.

Usage:
  kssign genkey <path>          write a fresh key file, mode 0600 (refuses to overwrite)
  kssign pubkey <keyfile>       print the public key (64 hex digits)
  kssign sign <keyfile> <file>  print the signature over <file> (128 hex digits)
  kssign verify <pubhex> <file> <sighexfile>
                                exit 0 only if the signature is valid
`

func cmdGenkey(args []string, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "kssign: genkey needs exactly one argument: the key file path\n\n%s", usage)
		return 2
	}
	path := args[0]
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		fmt.Fprintf(stderr, "kssign: generating the key: %v\n", err)
		return 1
	}
	// O_EXCL turns "refuses to overwrite" into an atomic check rather than
	// a check-then-write race.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			fmt.Fprintf(stderr, "kssign: %s already exists; refusing to overwrite it\n", path)
		} else {
			fmt.Fprintf(stderr, "kssign: creating %s: %v\n", path, err)
		}
		return 1
	}
	if _, err := f.WriteString(hex.EncodeToString(seed)); err != nil {
		f.Close()
		os.Remove(path)
		fmt.Fprintf(stderr, "kssign: writing %s: %v\n", path, err)
		return 1
	}
	if err := f.Close(); err != nil {
		fmt.Fprintf(stderr, "kssign: closing %s: %v\n", path, err)
		return 1
	}
	return 0
}

func cmdPubkey(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "kssign: pubkey needs exactly one argument: the key file path\n\n%s", usage)
		return 2
	}
	key, err := readKey(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "kssign: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, hex.EncodeToString(key.Public().(ed25519.PublicKey)))
	return 0
}

func cmdSign(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintf(stderr, "kssign: sign needs two arguments: the key file path and the file to sign\n\n%s", usage)
		return 2
	}
	key, err := readKey(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "kssign: %v\n", err)
		return 1
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "kssign: reading %s: %v\n", args[1], err)
		return 1
	}
	fmt.Fprintln(stdout, hex.EncodeToString(ed25519.Sign(key, data)))
	return 0
}

func cmdVerify(args []string, stderr io.Writer) int {
	if len(args) != 3 {
		fmt.Fprintf(stderr, "kssign: verify needs three arguments: the public key hex, the file and the signature file\n\n%s", usage)
		return 2
	}
	pub, err := hex.DecodeString(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "kssign: the public key is not hex: %v\n", err)
		return 1
	}
	if len(pub) != ed25519.PublicKeySize {
		fmt.Fprintf(stderr, "kssign: the public key must be %d hex digits (32 bytes), got %d\n", 2*ed25519.PublicKeySize, len(args[0]))
		return 1
	}
	data, err := os.ReadFile(args[1])
	if err != nil {
		fmt.Fprintf(stderr, "kssign: reading %s: %v\n", args[1], err)
		return 1
	}
	sigHex, err := os.ReadFile(args[2])
	if err != nil {
		fmt.Fprintf(stderr, "kssign: reading %s: %v\n", args[2], err)
		return 1
	}
	sig, err := hex.DecodeString(trimTrailingNewline(string(sigHex)))
	if err != nil || len(sig) != ed25519.SignatureSize {
		fmt.Fprintf(stderr, "kssign: %s is not a %d-hex-digit signature\n", args[2], 2*ed25519.SignatureSize)
		return 1
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), data, sig) {
		fmt.Fprintln(stderr, "kssign: the signature is not valid")
		return 1
	}
	return 0
}

// readKey loads the key file and refuses anything that is not exactly 64
// hex digits (one trailing newline is tolerated, since editors add one).
func readKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the key file: %w", err)
	}
	hexed := trimTrailingNewline(string(raw))
	if len(hexed) != 2*ed25519.SeedSize {
		return nil, fmt.Errorf("%s is not a key file: want exactly %d hex digits, got %d", path, 2*ed25519.SeedSize, len(hexed))
	}
	seed, err := hex.DecodeString(hexed)
	if err != nil {
		return nil, fmt.Errorf("%s is not a key file: %v", path, err)
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func trimTrailingNewline(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}
