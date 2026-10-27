package install

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestWindowsShortcutScriptReadsOnlyEnvironment(t *testing.T) {
	s := windowsShortcutScript
	for _, want := range []string{
		"WScript.Shell",
		"CreateShortcut",
		"$env:KS_LNK",
		"$env:KS_TARGET",
		"$env:KS_ARGS",
		"$env:KS_ICON",
		"$env:KS_WORKDIR",
		"Save()",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("shortcut script missing %q:\n%s", want, s)
		}
	}
	// No path text may ever be baked into the script; it must stay a fixed
	// template that only reads the environment.
	if strings.Contains(s, "\\") || strings.Contains(s, "Program Files") {
		t.Errorf("shortcut script contains a literal path:\n%s", s)
	}
}

func TestWindowsLnkEnv(t *testing.T) {
	o := Options{
		Launcher:   `C:\Program Files\kids-shelf\kids-shelf.exe`,
		InstallDir: `C:\Program Files\kids-shelf`,
	}
	got := windowsLnkEnv(o, `C:\Users\Kid\Desktop\kids-shelf.lnk`)
	want := map[string]string{
		"KS_TARGET":  `C:\Program Files\kids-shelf\kids-shelf.exe`,
		"KS_LNK":     `C:\Users\Kid\Desktop\kids-shelf.lnk`,
		"KS_ARGS":    "",
		"KS_ICON":    `C:\Program Files\kids-shelf\kids-shelf.ico`,
		"KS_WORKDIR": `C:\Program Files\kids-shelf`,
	}
	env := map[string]string{}
	for _, kv := range got {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry without '=': %q", kv)
		}
		env[k] = v
	}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
}

func TestWindowsLnkEnvCarriesDataDir(t *testing.T) {
	o := Options{
		Launcher:   `C:\Program Files\kids-shelf\kids-shelf.exe`,
		InstallDir: `C:\Program Files\kids-shelf`,
		DataDir:    `C:\Data Space\My Household`,
	}
	got := windowsLnkEnv(o, `C:\Users\Kid\Desktop\kids-shelf.lnk`)
	env := map[string]string{}
	for _, kv := range got {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			t.Fatalf("env entry without '=': %q", kv)
		}
		env[k] = v
	}
	if want := `--data-dir "C:\Data Space\My Household"`; env["KS_ARGS"] != want {
		t.Errorf("KS_ARGS = %q, want %q (a space-filled data dir must be quoted)", env["KS_ARGS"], want)
	}
	if strings.Contains(windowsShortcutScript, "Data Space") {
		t.Error("the data directory leaked into the shortcut script; it must stay in the environment")
	}
}

func TestWindowsRegistryValues(t *testing.T) {
	o := Options{
		Launcher:   `C:\Program Files\kids-shelf\kids-shelf.exe`,
		InstallDir: `C:\Program Files\kids-shelf`,
		Version:    "v1.2.3",
		Uninstall:  `"C:\Program Files\kids-shelf\kids-shelf.exe" --uninstall`,
	}
	str := map[string]string{}
	dword := map[string]uint32{}
	for _, v := range windowsRegistryValues(o, 4096) {
		if v.isDWord {
			dword[v.name] = v.dword
		} else {
			str[v.name] = v.str
		}
	}
	wantStr := map[string]string{
		"DisplayName":     "kids-shelf",
		"DisplayVersion":  "v1.2.3",
		"Publisher":       "kids-shelf",
		"DisplayIcon":     `C:\Program Files\kids-shelf\kids-shelf.ico`,
		"InstallLocation": `C:\Program Files\kids-shelf`,
		"UninstallString": `"C:\Program Files\kids-shelf\kids-shelf.exe" --uninstall`,
	}
	if !reflect.DeepEqual(str, wantStr) {
		t.Errorf("string values = %v, want %v", str, wantStr)
	}
	wantDword := map[string]uint32{"NoModify": 1, "NoRepair": 1, "EstimatedSize": 4096}
	if !reflect.DeepEqual(dword, wantDword) {
		t.Errorf("DWORD values = %v, want %v", dword, wantDword)
	}
}

func TestDirSizeKBRoundsUpToKiB(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a"), make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b"), make([]byte, 1025), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := dirSizeKB(dir)
	if err != nil {
		t.Fatalf("dirSizeKB: %v", err)
	}
	if got != 3 { // 1024 + 1025 bytes rounds up to 3 KiB
		t.Errorf("dirSizeKB = %d, want 3", got)
	}
	if _, err := dirSizeKB(filepath.Join(dir, "missing")); err == nil {
		t.Error("dirSizeKB on a missing directory succeeded, want an error")
	}
}
