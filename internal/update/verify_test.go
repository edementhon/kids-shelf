package update

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifySumsSignature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("abc123  kids-shelf_linux_amd64.zip\n")
	good := hex.EncodeToString(ed25519.Sign(priv, sums))
	old := PublicKeyHex

	t.Run("good signature", func(t *testing.T) {
		PublicKeyHex = hex.EncodeToString(pub)
		if err := verifySumsSignature(sums, good); err != nil {
			t.Fatalf("verifySumsSignature = %v, want nil", err)
		}
	})
	t.Run("wrong key", func(t *testing.T) {
		PublicKeyHex = hex.EncodeToString(pub)
		_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
		bad := hex.EncodeToString(ed25519.Sign(otherPriv, sums))
		if err := verifySumsSignature(sums, bad); err == nil {
			t.Fatal("verifySumsSignature accepted a signature from another key")
		}
	})
	t.Run("no compiled-in key", func(t *testing.T) {
		PublicKeyHex = ""
		if err := verifySumsSignature(sums, good); err == nil {
			t.Fatal("verifySumsSignature accepted a signature with no key compiled in")
		}
	})
	t.Run("garbage signature", func(t *testing.T) {
		PublicKeyHex = hex.EncodeToString(pub)
		if err := verifySumsSignature(sums, "zzz"); err == nil {
			t.Fatal("verifySumsSignature accepted a garbage signature")
		}
	})
	PublicKeyHex = old
}

func TestFindSum(t *testing.T) {
	zipSum := strings.Repeat("b", 64)
	sums := []byte("# comment\n" +
		zipSum + "  " + zipAssetName() + "\n" +
		strings.Repeat("a", 64) + "  some-other-asset.txt\n")
	got, err := findSum(sums, zipAssetName())
	if err != nil || got != zipSum {
		t.Fatalf("findSum = %q, %v", got, err)
	}
	if _, err := findSum(sums, "missing"); err == nil {
		t.Fatal("findSum for a missing name succeeded")
	}
	if _, err := findSum([]byte("nothex  "+zipAssetName()+"\n"), zipAssetName()); err == nil {
		t.Fatal("findSum accepted a non-hex checksum")
	}
}
func TestSignedVersion(t *testing.T) {
	zipSum := strings.Repeat("b", 64)
	good := []byte("version: v1.2.3\n" + zipSum + "  " + zipAssetName() + "\n")
	if v, err := signedVersion(good); err != nil || v != "v1.2.3" {
		t.Fatalf("signedVersion = %q, %v; want v1.2.3", v, err)
	}
	// CRLF line endings are tolerated.
	if v, err := signedVersion([]byte("version: v1.2.3\r\n" + zipSum + "  " + zipAssetName() + "\r\n")); err != nil || v != "v1.2.3" {
		t.Fatalf("signedVersion with CRLF = %q, %v", v, err)
	}
	for _, bad := range [][]byte{
		nil,
		[]byte(zipSum + "  " + zipAssetName() + "\n"),      // no version line
		[]byte("version: not-a-version\n" + zipSum + "\n"), // not canonical
		[]byte("version: v02.0.0\n" + zipSum + "\n"),       // leading zero
	} {
		if _, err := signedVersion(bad); err == nil {
			t.Errorf("signedVersion accepted %q", bad)
		}
	}
}

// zipWith builds a zip whose only entry is ZipEntryName containing data.
func zipWith(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(ZipEntryName())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractBinary(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out", EntryName())
	want := []byte("binary-bytes-v2")

	t.Run("plain release", func(t *testing.T) {
		zipPath := filepath.Join(dir, "good.zip")
		if err := os.WriteFile(zipPath, zipWith(t, want), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := extractBinary(zipPath, dst, 1<<20); err != nil {
			t.Fatalf("extractBinary: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("extracted = %q, %v; want %q", got, err, want)
		}
	})

	t.Run("traversal names never match", func(t *testing.T) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for _, name := range []string{"../evil", "/absolute/path", "a/b/" + EntryName(), "symlink-to-" + EntryName()} {
			w, err := zw.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			w.Write([]byte("junk"))
		}
		w, err := zw.Create(ZipEntryName())
		if err != nil {
			t.Fatal(err)
		}
		w.Write(want)
		zw.Close()
		zipPath := filepath.Join(dir, "traversal.zip")
		os.WriteFile(zipPath, buf.Bytes(), 0o600)
		os.Remove(dst)
		if err := extractBinary(zipPath, dst, 1<<20); err != nil {
			t.Fatalf("extractBinary: %v", err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("extracted = %q, %v; want the exact binary", got, err)
		}
		// Nothing escaped the destination.
		if _, err := os.Stat(filepath.Join(dir, "evil")); !errors.Is(err, os.ErrNotExist) {
			t.Error("a traversal entry was written")
		}
	})

	t.Run("directory and symlink refused", func(t *testing.T) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		// A directory entry named like the binary.
		if _, err := zw.Create(ZipEntryName() + "/"); err != nil {
			t.Fatal(err)
		}
		zw.Close()
		zipPath := filepath.Join(dir, "dir.zip")
		os.WriteFile(zipPath, buf.Bytes(), 0o600)
		if err := extractBinary(zipPath, dst, 1<<20); err == nil {
			t.Fatal("extractBinary accepted a directory named like the binary")
		}

		buf.Reset()
		zw = zip.NewWriter(&buf)
		hdr := &zip.FileHeader{Name: ZipEntryName()}
		hdr.SetMode(os.ModeSymlink | 0o777)
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte("target"))
		zw.Close()
		zipPath = filepath.Join(dir, "link.zip")
		os.WriteFile(zipPath, buf.Bytes(), 0o600)
		if err := extractBinary(zipPath, dst, 1<<20); err == nil {
			t.Fatal("extractBinary accepted a symlink named like the binary")
		}
	})

	t.Run("duplicate binary entry refused", func(t *testing.T) {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		for range 2 {
			w, err := zw.Create(ZipEntryName())
			if err != nil {
				t.Fatal(err)
			}
			w.Write(want)
		}
		zw.Close()
		zipPath := filepath.Join(dir, "dup.zip")
		os.WriteFile(zipPath, buf.Bytes(), 0o600)
		if err := extractBinary(zipPath, dst, 1<<20); err == nil {
			t.Fatal("extractBinary accepted a zip with two binary entries")
		}
	})

	t.Run("oversized entry refused", func(t *testing.T) {
		zipPath := filepath.Join(dir, "big.zip")
		if err := os.WriteFile(zipPath, zipWith(t, bytes.Repeat([]byte("x"), 1024)), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(dst)
		if err := extractBinary(zipPath, dst, 100); !errors.Is(err, errTooMuchData) {
			t.Fatalf("extractBinary oversized = %v, want errTooMuchData", err)
		}
		if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
			t.Error("a partial oversized binary was left behind")
		}
	})
	t.Run("darwin layout without the bundle member refused", func(t *testing.T) {
		// The darwin release contract ships the binary inside
		// kids-shelf.app; a zip carrying only a root kids-shelf (every
		// other OS's layout) must be refused for darwin, with an error
		// naming the member the release has to contain.
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("kids-shelf")
		if err != nil {
			t.Fatal(err)
		}
		w.Write(want)
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		zipPath := filepath.Join(dir, "darwin-no-bundle.zip")
		if err := os.WriteFile(zipPath, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		os.Remove(dst)
		err = extractBinaryMember(zipPath, dst, 1<<20, zipEntryName("darwin"))
		if err == nil {
			t.Fatal("extractBinaryMember accepted a darwin zip without the bundle member")
		}
		if !strings.Contains(err.Error(), "kids-shelf.app/Contents/MacOS/kids-shelf") {
			t.Fatalf("refusal error %q does not name the expected member", err)
		}
		if _, serr := os.Stat(dst); !errors.Is(serr, os.ErrNotExist) {
			t.Error("a refused darwin zip still wrote a binary")
		}
	})
}

func TestRestoreDB(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "kids-shelf.db")
	snapshot := filepath.Join(dir, "snapshot.db")
	if err := os.WriteFile(snapshot, []byte("snapshot-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A live database with WAL/SHM files from the crashed version.
	if err := os.WriteFile(db, []byte("polluted-db"), 0o600); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(db+"-wal", []byte("wal"), 0o600)
	os.WriteFile(db+"-shm", []byte("shm"), 0o600)

	if err := restoreDB(snapshot, db); err != nil {
		t.Fatalf("restoreDB: %v", err)
	}
	got, err := os.ReadFile(db)
	if err != nil || string(got) != "snapshot-bytes" {
		t.Fatalf("restored db = %q, %v", got, err)
	}
	if _, err := os.Stat(db + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Error("-wal survived the restore")
	}
	if _, err := os.Stat(db + "-shm"); !errors.Is(err, os.ErrNotExist) {
		t.Error("-shm survived the restore")
	}
	if _, err := os.Stat(db + ".restore"); !errors.Is(err, os.ErrNotExist) {
		t.Error("the restore temp file survived")
	}
}

func TestDownloadCap(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(big)
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "out.bin")
	client := &http.Client{}
	if err := downloadFile(context.Background(), client, srv.URL+"/asset", path, 1024); !errors.Is(err, errTooLarge) {
		t.Fatalf("downloadFile over cap = %v, want errTooLarge", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Error("the partial download was not removed")
	}
	if _, err := downloadBytes(context.Background(), client, srv.URL+"/asset", 1024); !errors.Is(err, errTooMuchData) {
		t.Fatalf("downloadBytes over cap = %v, want errTooMuchData", err)
	}
}
