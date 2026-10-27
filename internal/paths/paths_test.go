package paths

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDataDirFor(t *testing.T) {
	tests := []struct {
		name     string
		goos     string
		appData  string
		xdg      string
		home     string
		override string
		want     string
		wantErr  bool
	}{
		{
			name:    "windows uses AppData",
			goos:    "windows",
			appData: filepath.Join(`C:\`, "Users", "Ann", "AppData", "Roaming"),
			want:    filepath.Join(`C:\`, "Users", "Ann", "AppData", "Roaming", "kids-shelf"),
		},
		{
			name:    "windows without AppData errors",
			goos:    "windows",
			appData: "",
			wantErr: true,
		},
		{
			name: "macos uses Application Support",
			goos: "darwin",
			home: "/Users/Ann",
			want: filepath.Join("/Users/Ann", "Library", "Application Support", "kids-shelf"),
		},
		{
			name:    "macos without home errors",
			goos:    "darwin",
			home:    "",
			wantErr: true,
		},
		{
			name: "linux uses XDG_DATA_HOME when set",
			goos: "linux",
			xdg:  "/var/data",
			home: "/home/ann",
			want: filepath.Join("/var/data", "kids-shelf"),
		},
		{
			name: "linux falls back to .local/share",
			goos: "linux",
			xdg:  "",
			home: "/home/ann",
			want: filepath.Join("/home/ann", ".local", "share", "kids-shelf"),
		},
		{
			name:    "linux without home and XDG errors",
			goos:    "linux",
			xdg:     "",
			home:    "",
			wantErr: true,
		},
		{
			name:     "override wins over every OS rule",
			goos:     "windows",
			appData:  `C:\ignored`,
			override: "/srv/kids-shelf",
			want:     "/srv/kids-shelf",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dataDirFor(tt.goos, tt.appData, tt.xdg, tt.home, tt.override)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("dataDirFor(%q) = %q, want an error", tt.goos, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("dataDirFor(%q): %v", tt.goos, err)
			}
			if got != tt.want {
				t.Fatalf("dataDirFor(%q) = %q, want %q", tt.goos, got, tt.want)
			}
		})
	}
}

// TestDataDirEnv exercises the exported DataDir against the live
// environment on whatever OS the test runs on.
func TestDataDirEnv(t *testing.T) {
	home := filepath.Join("tmp", "home")
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", filepath.Join("tmp", "appdata"))
	t.Setenv("XDG_DATA_HOME", filepath.Join("tmp", "xdg"))

	want := filepath.Join("tmp", "xdg", "kids-shelf")
	switch runtime.GOOS {
	case "windows":
		want = filepath.Join("tmp", "appdata", "kids-shelf")
	case "darwin":
		want = filepath.Join(home, "Library", "Application Support", "kids-shelf")
	}
	got, err := DataDir()
	if err != nil {
		t.Fatalf("DataDir(): %v", err)
	}
	if got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}

	if runtime.GOOS != "windows" && runtime.GOOS != "darwin" {
		t.Setenv("XDG_DATA_HOME", "")
		got, err = DataDir()
		if err != nil {
			t.Fatalf("DataDir() without XDG_DATA_HOME: %v", err)
		}
		want = filepath.Join(home, ".local", "share", "kids-shelf")
		if got != want {
			t.Fatalf("DataDir() = %q, want %q", got, want)
		}
	}

	got, err = DataDir("/srv/kids-shelf")
	if err != nil {
		t.Fatalf("DataDir(override): %v", err)
	}
	if got != "/srv/kids-shelf" {
		t.Fatalf("DataDir(override) = %q, want the override", got)
	}
}

func TestMediaDirFor(t *testing.T) {
	const fallback = "/data/dir"

	home := t.TempDir()

	// No Videos/Movies folder in the home: fall back to the data dir.
	if got := mediaDirFor("linux", home, fallback, ""); got != fallback {
		t.Fatalf("mediaDirFor without Videos = %q, want fallback %q", got, fallback)
	}

	// Linux/Windows use the Videos folder.
	if err := os.MkdirAll(filepath.Join(home, "Videos"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Videos", "kids-shelf media")
	if got := mediaDirFor("linux", home, fallback, ""); got != want {
		t.Fatalf("mediaDirFor(linux) = %q, want %q", got, want)
	}
	if got := mediaDirFor("windows", home, fallback, ""); got != want {
		t.Fatalf("mediaDirFor(windows) = %q, want %q", got, want)
	}

	// macOS uses the Movies folder.
	home2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home2, "Movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	want = filepath.Join(home2, "Movies", "kids-shelf media")
	if got := mediaDirFor("darwin", home2, fallback, ""); got != want {
		t.Fatalf("mediaDirFor(darwin) = %q, want %q", got, want)
	}

	// An override wins even when the well-known folder is missing.
	if got := mediaDirFor("linux", home, fallback, "/custom/media"); got != "/custom/media" {
		t.Fatalf("mediaDirFor with override = %q, want the override", got)
	}

	// An unknown home falls back.
	if got := mediaDirFor("linux", "", fallback, ""); got != fallback {
		t.Fatalf("mediaDirFor with empty home = %q, want fallback %q", got, fallback)
	}
}

// TestDefaultMediaDirEnv exercises the exported DefaultMediaDir against the
// live environment on whatever OS the test runs on.
func TestDefaultMediaDirEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
	}
	dataDir := filepath.Join(t.TempDir(), "custom-data")

	// No Videos/Movies folder: the data dir in use is the fallback.
	if got := DefaultMediaDir(dataDir, ""); got != dataDir {
		t.Fatalf("DefaultMediaDir = %q, want the data dir fallback %q", got, dataDir)
	}

	// With the well-known folder present, media lives inside it.
	folder := "Videos"
	if runtime.GOOS == "darwin" {
		folder = "Movies"
	}
	if err := os.MkdirAll(filepath.Join(home, folder), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, folder, "kids-shelf media")
	if got := DefaultMediaDir(dataDir, ""); got != want {
		t.Fatalf("DefaultMediaDir = %q, want %q", got, want)
	}

	if got := DefaultMediaDir(dataDir, "/srv/media"); got != "/srv/media" {
		t.Fatalf("DefaultMediaDir(override) = %q, want the override", got)
	}
}

// TestDataDirWithoutHome checks that DataDir resolves without a home
// directory whenever an explicit override or an environment variable
// (APPDATA on Windows, XDG_DATA_HOME on Linux) already decides the
// location: the override wins outright, the environment variable comes
// next, and only the remaining cases consult the home directory.
func TestDataDirWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	}
	t.Setenv("APPDATA", "")
	t.Setenv("XDG_DATA_HOME", "")

	// The override is returned before anything is resolved.
	got, err := DataDir("/srv/kids-shelf")
	if err != nil {
		t.Fatalf("DataDir(override): %v", err)
	}
	if got != "/srv/kids-shelf" {
		t.Fatalf("DataDir(override) = %q, want the override", got)
	}

	if runtime.GOOS == "darwin" {
		// macOS has no environment shortcut: the home directory is always
		// needed, so nothing more can be asserted without one.
		return
	}
	envVar, envVal := "XDG_DATA_HOME", filepath.Join("tmp", "xdg")
	if runtime.GOOS == "windows" {
		envVar, envVal = "APPDATA", filepath.Join("tmp", "appdata")
	}
	t.Setenv(envVar, envVal)
	want := filepath.Join(envVal, "kids-shelf")
	got, err = DataDir()
	if err != nil {
		t.Fatalf("DataDir() with %s set and no home: %v", envVar, err)
	}
	if got != want {
		t.Fatalf("DataDir() with %s set = %q, want %q", envVar, got, want)
	}
}

// TestDefaultMediaDirWithoutHome checks that DefaultMediaDir honors an
// override before resolving anything (data dir, home directory, well-known
// folders).
func TestDefaultMediaDirWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	}
	if got := DefaultMediaDir("/srv/data", "/srv/media"); got != "/srv/media" {
		t.Fatalf("DefaultMediaDir(override) = %q, want the override", got)
	}
	if got := DefaultMediaDir("/srv/data", ""); got != "/srv/data" {
		t.Fatalf("DefaultMediaDir without home = %q, want the data dir", got)
	}
}
