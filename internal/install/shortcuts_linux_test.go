//go:build linux

package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// disableLinuxTools points the external-tool vars at nothing so tests are
// deterministic on machines with or without xdg-user-dir and gio installed.
func disableLinuxTools() {
	gioCmd, xdgUserDir = "", ""
}

func TestDesktopEntryQuotesLauncher(t *testing.T) {
	o := Options{
		Launcher:   `/opt/My Shelf/kids-shelf"x`,
		InstallDir: `/opt/kids-shelf`,
	}
	got := desktopEntry(o)
	for _, want := range []string{
		"[Desktop Entry]\n",
		"Type=Application\n",
		"Name=kids-shelf\n",
		"Comment=Kids' internet server\n",
		`Exec="/opt/My Shelf/kids-shelf\"x"` + "\n",
		"Icon=/opt/kids-shelf/kids-shelf.png\n",
		"Terminal=false\n",
		"Categories=Education;\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("desktop entry missing %q:\n%s", want, got)
		}
	}
}

func TestDesktopEntryCleanPathUnquoted(t *testing.T) {
	got := desktopEntry(Options{Launcher: "/opt/kids/kids-shelf", InstallDir: "/opt/kids"})
	if !strings.Contains(got, "Exec=/opt/kids/kids-shelf\n") {
		t.Errorf("clean launcher should pass through unquoted:\n%s", got)
	}
}

func TestDesktopExecQuoteEscapesDollarAndPercent(t *testing.T) {
	got := desktopExecQuote(`/opt/$HOME 100%/kids`)
	want := `"/opt/\$HOME 100%%/kids"`
	if got != want {
		t.Errorf("desktopExecQuote = %q, want %q", got, want)
	}
}

func TestLinuxApplicationsDirFollowsXDG(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("XDG_DATA_HOME", "")
	got, err := linuxApplicationsDir()
	if err != nil {
		t.Fatalf("linuxApplicationsDir: %v", err)
	}
	want := filepath.Join(home, ".local", "share", "applications")
	if got != want {
		t.Errorf("unset XDG_DATA_HOME: got %s, want %s", got, want)
	}

	data := filepath.Join(home, "data-home")
	t.Setenv("XDG_DATA_HOME", data)
	got, err = linuxApplicationsDir()
	if err != nil {
		t.Fatalf("linuxApplicationsDir: %v", err)
	}
	if got != filepath.Join(data, "applications") {
		t.Errorf("set XDG_DATA_HOME: got %s, want %s/applications", got, data)
	}

	// The spec requires an absolute XDG_DATA_HOME; a relative one falls
	// back to the home directory, matching spec-respecting launchers.
	t.Setenv("XDG_DATA_HOME", "relative/data")
	got, err = linuxApplicationsDir()
	if err != nil {
		t.Fatalf("linuxApplicationsDir: %v", err)
	}
	if got != want {
		t.Errorf("relative XDG_DATA_HOME: got %s, want %s", got, want)
	}
}

func TestLinuxDesktopDirFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	disableLinuxTools()

	if got := linuxDesktopDir(); got != "" {
		t.Errorf("no Desktop folder: got %q, want \"\"", got)
	}
	if err := os.Mkdir(filepath.Join(home, "Desktop"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, "Desktop")
	if got := linuxDesktopDir(); got != want {
		t.Errorf("Desktop exists: got %q, want %q", got, want)
	}
}

func TestCreateShortcutsLinux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	disableLinuxTools()
	desktop := filepath.Join(home, "Desktop")
	if err := os.Mkdir(desktop, 0o755); err != nil {
		t.Fatal(err)
	}

	installDir := t.TempDir()
	o := Options{Launcher: filepath.Join(installDir, "kids-shelf"), InstallDir: installDir}
	created, err := CreateShortcuts(o)
	if err != nil {
		t.Fatalf("CreateShortcuts: %v", err)
	}

	menuPath := filepath.Join(home, ".local", "share", "applications", "kids-shelf.desktop")
	desktopPath := filepath.Join(desktop, "kids-shelf.desktop")
	pngPath := filepath.Join(installDir, "kids-shelf.png")
	for _, want := range []string{menuPath, desktopPath, pngPath} {
		found := false
		for _, p := range created {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("created = %v, missing %s", created, want)
		}
	}

	for path, mode := range map[string]os.FileMode{menuPath: 0o644, desktopPath: 0o755} {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if st.Mode().Perm() != mode {
			t.Errorf("%s mode = %v, want %v", path, st.Mode().Perm(), mode)
		}
		if st.Mode()&os.ModeSymlink != 0 {
			t.Errorf("%s is a symlink; the .desktop file must be a regular file", path)
		}
	}

	menu, err := os.ReadFile(menuPath)
	if err != nil {
		t.Fatalf("reading %s: %v", menuPath, err)
	}
	desktopFile, err := os.ReadFile(desktopPath)
	if err != nil {
		t.Fatalf("reading %s: %v", desktopPath, err)
	}
	if string(desktopFile) != string(menu) {
		t.Error("desktop copy differs from the menu entry")
	}
	if !strings.Contains(string(menu), "Exec="+desktopExecQuote(o.Launcher)+"\n") {
		t.Errorf("menu entry does not point Exec at the launcher:\n%s", menu)
	}

	if st, err := os.Stat(pngPath); err != nil {
		t.Fatalf("icon not written: %v", err)
	} else if st.Size() == 0 {
		t.Error("icon file is empty")
	}
}

func TestCreateShortcutsLinuxWithoutDesktop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	disableLinuxTools()

	installDir := t.TempDir()
	o := Options{Launcher: filepath.Join(installDir, "kids-shelf"), InstallDir: installDir}
	created, err := CreateShortcuts(o)
	if err != nil {
		t.Fatalf("CreateShortcuts: %v", err)
	}
	homeDesktop := filepath.Join(home, "Desktop")
	for _, p := range created {
		if strings.HasSuffix(p, "kids-shelf.desktop") && filepath.Dir(p) == homeDesktop {
			t.Errorf("created a desktop copy %s while no Desktop exists", p)
		}
	}
	menuPath := filepath.Join(home, ".local", "share", "applications", "kids-shelf.desktop")
	if _, err := os.Stat(menuPath); err != nil {
		t.Errorf("menu entry not created: %v", err)
	}
}

func TestRemoveShortcutsLinux(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", "")
	disableLinuxTools()
	desktop := filepath.Join(home, "Desktop")
	if err := os.Mkdir(desktop, 0o755); err != nil {
		t.Fatal(err)
	}

	installDir := t.TempDir()
	o := Options{Launcher: filepath.Join(installDir, "kids-shelf"), InstallDir: installDir}
	if _, err := CreateShortcuts(o); err != nil {
		t.Fatalf("CreateShortcuts: %v", err)
	}
	if err := RemoveShortcuts(o); err != nil {
		t.Fatalf("RemoveShortcuts: %v", err)
	}

	menuPath := filepath.Join(home, ".local", "share", "applications", "kids-shelf.desktop")
	desktopPath := filepath.Join(desktop, "kids-shelf.desktop")
	for _, gone := range []string{menuPath, desktopPath} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("%s still exists after RemoveShortcuts (stat err %v)", gone, err)
		}
	}
	// The icon stays: the uninstall flow removes the whole InstallDir.
	if _, err := os.Stat(filepath.Join(installDir, "kids-shelf.png")); err != nil {
		t.Errorf("icon removed by RemoveShortcuts, want it left in place: %v", err)
	}

	// Removing again is harmless.
	if err := RemoveShortcuts(o); err != nil {
		t.Errorf("second RemoveShortcuts: %v", err)
	}
}

// TestDesktopEntryCarriesDataDir pins review round 4 finding 9 on Linux:
// the .desktop Exec line passes the installation's explicit data directory
// through, quoted per the Desktop Entry spec like the launcher itself.
func TestDesktopEntryCarriesDataDir(t *testing.T) {
	launcher := filepath.Join("/opt", "kids", "kids-shelf")
	dataDir := filepath.Join("/var", "data space", "household")
	got := desktopEntry(Options{Launcher: launcher, InstallDir: "/opt/kids", DataDir: dataDir})
	want := "Exec=" + desktopExecQuote(launcher) + " --data-dir " + desktopExecQuote(dataDir) + "\n"
	if !strings.Contains(got, want) {
		t.Errorf("Exec line = %q, want it to contain %q", got, want)
	}
	// Without an explicit data directory nothing is appended.
	if plain := desktopEntry(Options{Launcher: launcher, InstallDir: "/opt/kids"}); !strings.Contains(plain, "Exec="+launcher+"\n") {
		t.Errorf("clean launcher should pass through without a data argument:\n%s", plain)
	}
}
