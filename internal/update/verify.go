package update

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// downloadBytes fetches url and returns up to max bytes. The manifest and
// signature are tiny; the cap keeps a misbehaving server from filling the
// disk before the checksum is even verified.
func downloadBytes(ctx context.Context, client *http.Client, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("update: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("update: %s answered %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, fmt.Errorf("update: reading %s: %w", url, err)
	}
	if int64(len(b)) > max {
		return nil, errTooMuchData
	}
	return b, nil
}

// downloadFile streams url into path, failing when the body exceeds max
// bytes; the partial file is removed on any error.
func downloadFile(ctx context.Context, client *http.Client, url, path string, max int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("update: downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("update: %s answered %s", url, resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, max+1))
	if err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("update: writing %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return err
	}
	if n > max {
		os.Remove(path)
		return errTooLarge
	}
	return nil
}

// verifySumsSignature checks the hex ed25519 signature over the SHA256SUMS
// bytes with the compiled-in public key.
func verifySumsSignature(sums []byte, sigHex string) error {
	if PublicKey() == "" {
		return errors.New("update: no public key compiled in; refusing to verify any release")
	}
	pub, err := hex.DecodeString(strings.TrimSpace(PublicKey()))
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return errors.New("update: the compiled-in public key is not 64 hex characters")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("update: the signature in SHA256SUMS.sig is not %d hex characters", ed25519.SignatureSize*2)
	}
	if !ed25519.Verify(pub, sums, sig) {
		return errBadSignature
	}
	return nil
}

// signedVersion extracts the version a SHA256SUMS file was signed for. The
// first line is `version: vX.Y.Z` and the signature covers the whole file,
// so a release can never be relabelled after signing: the signed version
// is what the checksums belong to.
func signedVersion(sums []byte) (string, error) {
	line := strings.TrimSpace(strings.SplitN(string(sums), "\n", 2)[0])
	rest, ok := strings.CutPrefix(line, "version:")
	if !ok {
		return "", errNoSignedVersion
	}
	v := strings.TrimSpace(rest)
	if !validVersion(v) {
		return "", fmt.Errorf("%w: %q", errNoSignedVersion, v)
	}
	return v, nil
}

// findSum returns the sha256 for name from SHA256SUMS content in sha256sum
// format ("<64 hex>  <name>", one per line). Non-conforming lines are
// ignored; a missing entry is an error, because an unsigned release must not
// silence the missing line into a skip.
func findSum(sums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(sums), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != name {
			continue
		}
		if len(fields[0]) != 64 {
			return "", fmt.Errorf("update: SHA256SUMS entry for %s is not 64 hex characters", name)
		}
		if _, err := hex.DecodeString(fields[0]); err != nil {
			return "", fmt.Errorf("update: SHA256SUMS entry for %s is not hex: %w", name, err)
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("update: SHA256SUMS has no entry for %s", name)
}

// extractBinary writes exactly the release binary for the running OS
// (ZipEntryName: inside the kids-shelf.app bundle on darwin, the plain
// executable name elsewhere) from the already-checksummed zip to dst,
// capping the extracted bytes at max. A zip that carries the member as a
// directory or symlink, or more than once, is refused; any other entry
// (including `../` and absolute names) is never written, so the
// extraction cannot escape dst. A zip without the expected member is
// refused with an error naming it.
func extractBinary(zipPath, dst string, max int64) error {
	return extractBinaryMember(zipPath, dst, max, ZipEntryName())
}

// extractBinaryMember is extractBinary for a named zip member instead of
// the running OS's; tests use it to exercise the darwin bundle layout on
// any host.
func extractBinaryMember(zipPath, dst string, max int64, member string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("update: opening %s: %w", zipPath, err)
	}
	defer r.Close()
	want := member
	// Pass 1: exactly one plain file may carry the binary's name. The
	// duplicate, directory and symlink checks run before anything is
	// written.
	var match *zip.File
	for _, f := range r.File {
		if filepath.ToSlash(f.Name) != want {
			continue
		}
		if match != nil {
			return fmt.Errorf("update: %s contains more than one %s entry", zipPath, want)
		}
		if f.FileInfo().IsDir() {
			return fmt.Errorf("update: %s in the release is a directory, not a file", want)
		}
		if f.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("update: %s in the release is a symlink; refusing to extract it", want)
		}
		if f.UncompressedSize64 > uint64(max) {
			return errTooMuchData
		}
		match = f
	}
	if match == nil {
		return fmt.Errorf("update: %s contains no %s entry", zipPath, want)
	}
	// Pass 2: write the verified entry.
	rc, err := match.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(rc, max+1))
	// Flush the writable handle that wrote the bytes before closing it, so
	// the extracted binary survives a crash; never reopen the file
	// read-only to Sync (Windows cannot flush a read-only handle).
	if serr := out.Sync(); err == nil {
		err = serr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	if n > max {
		os.Remove(dst)
		return errTooMuchData
	}
	return nil
}
