package update

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestValidVersion(t *testing.T) {
	valid := []string{"v0.0.0", "v1.0.0", "v1.2.3", "v10.20.30"}
	for _, v := range valid {
		if !validVersion(v) {
			t.Errorf("validVersion(%q) = false, want true", v)
		}
	}
	invalid := []string{
		"", "1.0.0", "v1", "v1.0", "v1.0.0-beta", "v1.0.0.1", "v1.0.0 ",
		"V1.0.0", "v1.0.0\n", "../../etc/passwd", "v1.0.0/..", "v..",
		"v02.0.0", "v2.00.1", "v2.0.01", "v00.0.0",
	}
	for _, v := range invalid {
		if validVersion(v) {
			t.Errorf("validVersion(%q) = true, want false", v)
		}
	}
}

func TestVersionAbove(t *testing.T) {
	cases := []struct {
		v, high string
		want    bool
	}{
		{"v1.0.0", "", true},         // empty high-water: any version qualifies
		{"v1.0.1", "v1.0.0", true},   // patch
		{"v1.1.0", "v1.0.9", true},   // minor beats patch
		{"v2.0.0", "v1.9.9", true},   // major beats minor
		{"v1.0.0", "v1.0.0", false},  // equal is not above
		{"v1.0.0", "v1.0.1", false},  // older
		{"v1.2.0", "v1.10.0", false}, // numeric, not lexicographic
		{"v10.0.0", "v9.9.9", true},  // numeric, not lexicographic
		{"not-a-version", "v1.0.0", false},
		{"v1.0.0", "not-a-version", true}, // a corrupt high-water cannot block a real version
	}
	for _, c := range cases {
		if got := versionAbove(c.v, c.high); got != c.want {
			t.Errorf("versionAbove(%q, %q) = %v, want %v", c.v, c.high, got, c.want)
		}
	}
}

func TestReplaceFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := replaceFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(path, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "two" {
		t.Fatalf("content = %q, %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}

func TestInstallDirFromEnv(t *testing.T) {
	old := os.Getenv(envInstall)
	os.Setenv(envInstall, "/some/install")
	t.Cleanup(func() {
		if old == "" {
			os.Unsetenv(envInstall)
		} else {
			os.Setenv(envInstall, old)
		}
	})
	dir, err := InstallDir()
	if err != nil || dir != "/some/install" {
		t.Fatalf("InstallDir = %q, %v; want /some/install", dir, err)
	}
}

func TestVersionLayoutPaths(t *testing.T) {
	install := "/install"
	if got := EntryPath(install); got != filepath.Join(install, EntryName()) {
		t.Errorf("EntryPath = %q", got)
	}
	if got := VersionDir(install, "v1.0.0"); got != filepath.Join(install, "versions", "v1.0.0") {
		t.Errorf("VersionDir = %q", got)
	}
	if got := VersionBinary(install, "v1.0.0"); got != filepath.Join(install, "versions", "v1.0.0", EntryName()) {
		t.Errorf("VersionBinary = %q", got)
	}
	if got := DatabasePath("/data"); got != filepath.Join("/data", "kids-shelf.db") {
		t.Errorf("DatabasePath = %q", got)
	}
}

func TestZipEntryName(t *testing.T) {
	cases := []struct {
		goos string
		want string
	}{
		{"darwin", "kids-shelf.app/Contents/MacOS/kids-shelf"},
		{"linux", "kids-shelf"},
		{"windows", "kids-shelf.exe"},
	}
	for _, c := range cases {
		if got := zipEntryName(c.goos); got != c.want {
			t.Errorf("zipEntryName(%q) = %q, want %q", c.goos, got, c.want)
		}
	}
	// The runtime entry must agree with the mapping for this machine.
	if got := ZipEntryName(); got != zipEntryName(runtime.GOOS) {
		t.Errorf("ZipEntryName() = %q, want %q", got, zipEntryName(runtime.GOOS))
	}
}
