// Pure text builders for the Windows shortcuts. They live in an untagged
// file because none of them touches the Windows API: keeping them here lets
// the tests run on any host instead of only inside a Windows VM.
package install

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
)

// windowsIconPath is where CreateShortcuts drops the .ico on Windows. It
// joins with a backslash itself rather than filepath.Join, so the text
// builders produce real Windows paths when the tests run on Linux.
func windowsIconPath(o Options) string {
	return strings.TrimRight(o.InstallDir, `\/`) + `\kids-shelf.ico`
}

// windowsShortcutScript is the PowerShell body that writes one .lnk through
// the WScript.Shell COM object. Every path arrives in an environment
// variable (KS_LNK, KS_TARGET, KS_ICON, KS_WORKDIR), never interpolated
// into the script text, so install paths full of quotes or PowerShell
// punctuation cannot alter what the script does. The IconLocation suffix
// ",0" picks the first icon inside the .ico.
const windowsShortcutScript = `$shell = New-Object -ComObject WScript.Shell
$link = $shell.CreateShortcut($env:KS_LNK)
$link.TargetPath = $env:KS_TARGET
$link.Arguments = $env:KS_ARGS
$link.IconLocation = "$env:KS_ICON,0"
$link.WorkingDirectory = $env:KS_WORKDIR
$link.Description = "kids-shelf"
$link.Save()`

// windowsLnkEnv returns the environment entries the shortcut script reads
// for one .lnk file. KS_ARGS carries the launcher's command line, notably
// the --data-dir of an installation made with an explicit data directory,
// so a shortcut never opens another household (review round 4 finding 9).
// It stays empty when the launcher takes its default data directory.
func windowsLnkEnv(o Options, lnkPath string) []string {
	return []string{
		"KS_LNK=" + lnkPath,
		"KS_TARGET=" + o.Launcher,
		"KS_ARGS=" + windowsLnkArgs(o),
		"KS_ICON=" + windowsIconPath(o),
		"KS_WORKDIR=" + o.InstallDir,
	}
}

// windowsLnkArgs renders the .lnk Arguments string: --data-dir with the
// directory quoted the way the Windows command line parses it. The quote
// is always present so a path with spaces survives; embedded quotes are
// doubled per CommandLineToArgvW.
func windowsLnkArgs(o Options) string {
	if o.DataDir == "" {
		return ""
	}
	return "--data-dir " + `"` + strings.ReplaceAll(o.DataDir, `"`, `""`) + `"`
}

// regValue is one value of the per-user uninstall key: a string (REG_SZ) or
// a DWORD.
type regValue struct {
	name    string
	str     string
	dword   uint32
	isDWord bool
}

// windowsRegistryValues lists the uninstall entry values in the order they
// are written, so Settings > Apps shows kids-shelf with a working
// UninstallString, icon and size.
func windowsRegistryValues(o Options, estimatedSizeKB uint32) []regValue {
	return []regValue{
		{name: "DisplayName", str: "kids-shelf"},
		{name: "DisplayVersion", str: o.Version},
		{name: "Publisher", str: "kids-shelf"},
		{name: "DisplayIcon", str: windowsIconPath(o)},
		{name: "InstallLocation", str: o.InstallDir},
		{name: "UninstallString", str: o.Uninstall},
		{name: "NoModify", dword: 1, isDWord: true},
		{name: "NoRepair", dword: 1, isDWord: true},
		{name: "EstimatedSize", dword: estimatedSizeKB, isDWord: true},
	}
}

// dirSizeKB sums the sizes of every file under dir and rounds up to KiB,
// the unit the registry's EstimatedSize DWORD stores.
func dirSizeKB(dir string) (uint32, error) {
	var bytes int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		bytes += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	kb := uint64((bytes + 1023) / 1024)
	const maxDWORD = uint64(^uint32(0))
	if kb > maxDWORD {
		return 0, fmt.Errorf("%s is larger than the 4 TiB a registry DWORD can describe", dir)
	}
	return uint32(kb), nil
}
