//go:build windows

package firewall

import (
	"context"

	"errors"
	"fmt"
	"os"
	"strings"

	"time"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/edementhon/kids-shelf/internal/proc"
)

// utf16Ptr converts a string to the *uint16 Windows APIs expect (a
// terminating NUL included, so the result can be handed straight to a
// struct field).
func utf16Ptr(s string) *uint16 {
	u, err := windows.UTF16PtrFromString(s)
	if err != nil {
		return nil
	}
	return u
}

// statusQuery is the PowerShell probe the status check runs: the network
// connection profiles with their firewall category, the firewall state per
// profile, and the firewall rules bound to the program (whose path arrives
// in the KS_FW_PROGRAM environment variable, never as inline text). One
// run, one JSON document: a single object whose arrays parseStatus reads,
// never an array wrapper (the decoder unmarshals into a struct, so an
// array makes every probe Unknown). CIM properties arrive as enums and
// numbers, so every field is cast to the string or boolean type the
// decoder expects: "Public", "Block" and true, not 0, 4 and 1. The
// connection category "DomainAuthenticated" is normalized to "Domain",
// the firewall profile that governs it; -Depth 5 covers the object, the
// arrays and the per-entry hashtables (the default depth of 2 flattens
// those hashtables to type names like "System.Collections.Hashtable").
const statusQuery = `$p = @(Get-NetConnectionProfile -ErrorAction SilentlyContinue);
$f = @(Get-NetFirewallProfile -ErrorAction SilentlyContinue);
$r = @(Get-NetFirewallApplicationFilter -Program $env:KS_FW_PROGRAM -ErrorAction SilentlyContinue | ForEach-Object { Get-NetFirewallRule -AssociatedNetFirewallApplicationFilter $_ -ErrorAction SilentlyContinue });
[Console]::OutputEncoding = [System.Text.Encoding]::UTF8;
$categories = @($p | ForEach-Object { ([string]$_.NetworkCategory -replace 'DomainAuthenticated', 'Domain') });
$profiles = @($p | ForEach-Object {
  @{ name = [string]$_.InterfaceAlias; category = ([string]$_.NetworkCategory -replace 'DomainAuthenticated', 'Domain'); active = $true }
});
if ($profiles.Count -eq 0) { $profiles = @(@{ name = ''; category = ''; active = $false }) };
$firewalls = @($f | ForEach-Object {
  @{ profile = [string]$_.Profile; enabled = ([string]$_.Enabled -eq 'True'); active = ($categories -contains [string]$_.Profile) }
});
$rules = @($r | ForEach-Object {
  @{ enabled = ([string]$_.Enabled -eq 'True'); action = [string]$_.Action; direction = [string]$_.Direction; profile = [string]$_.Profile }
});
@{ profiles = $profiles; firewalls = $firewalls; rules = $rules } | ConvertTo-Json -Compress -Depth 5`

// envProgram is the environment variable that carries the program path to
// the status probe. A path in the command line would need quoting (and
// Windows quoting is a swamp); an environment variable arrives verbatim.
const envProgram = "KS_FW_PROGRAM"

// newController builds the Windows implementation: the program whose path
// the status probe and the allow rule key on.
func newController(program string) Control {
	return &controller{program: program}
}

// query runs the PowerShell probe, or the test hook when one is installed
// (the hook takes over the whole launch so tests never spawn a process).
func (c *controller) query(ctx context.Context) ([]byte, error) {
	if out, err, ok := c.hookProbe(ctx); ok {
		return out, err
	}
	cmd := proc.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-Command", statusQuery)
	cmd.Env = append(os.Environ(), envProgram+"="+c.program)
	return cmd.Output()
}

// runElevated runs cmd through ShellExecuteExW with the runas verb (the UAC
// prompt) and waits for the process to finish. Declining the prompt returns
// an error wrapping windows.ERROR_CANCELLED ("the operation was cancelled
// by the user").
func runElevated(ctx context.Context, cmd string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	sei := &shellExecuteInfo{
		cbSize:       uint32(unsafe.Sizeof(shellExecuteInfo{})),
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		nShow:        windows.SW_HIDE,
		lpVerb:       utf16Ptr("runas"),
		lpFile:       utf16Ptr("powershell.exe"),
		lpParameters: utf16Ptr("-NoProfile -NonInteractive -EncodedCommand " + encodedCommand(cmd)),
	}
	if err := execute(sei); err != nil {
		return err
	}
	// ShellExecuteExW filled hProcess because of SEE_MASK_NOCLOSEPROCESS;
	// return the handle once its exit code is read, or it leaks for the
	// life of the parent.
	defer windows.CloseHandle(sei.hProcess)
	exit, err := waitElevated(ctx, sei.hProcess)
	if err != nil {
		return err
	}
	if exit != 0 {
		return fmt.Errorf("the firewall rule could not be added (exit %d)", exit)
	}
	return nil
}

// waitElevated waits for the elevated process to exit, honouring ctx. A
// deadline is applied even without one in ctx: a permission dialog left
// open cannot tie up the parent page forever.
func waitElevated(ctx context.Context, h windows.Handle) (uint32, error) {
	wait := 5 * time.Minute
	if dl, ok := ctx.Deadline(); ok {
		if d := time.Until(dl); d < wait {
			wait = d
		}
	}
	if wait <= 0 {
		return 0, ctx.Err()
	}
	ms := uint32(wait / time.Millisecond)
	if ms == 0 {
		ms = 1
	}
	rv, err := windows.WaitForSingleObject(h, ms)
	if err != nil {
		return 0, err
	}
	if rv == uint32(windows.WAIT_TIMEOUT) {
		return 0, errors.New("the Windows permission prompt is still waiting for an answer")
	}
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return 0, err
	}
	return code, nil
}

// trimBOM drops the UTF-8 byte-order mark Windows PowerShell 5.1 puts in
// front of redirected output, which would otherwise break the JSON decoder.
func trimBOM(b []byte) []byte {
	return []byte(strings.TrimPrefix(string(b), "\ufeff"))
}

// ─── ShellExecuteExW binding ──────────────────────────────────────────────
// golang.org/x/sys does not expose ShellExecuteExW, so the call is bound
// here through the lazy system DLL loader.

const (
	// seeMaskNocloseProcess keeps the elevated process's handle so the
	// caller can wait for it and read its exit code.
	seeMaskNoCloseProcess = 0x00000040
	// seeMaskNoAsync makes ShellExecuteExW return only after the call has
	// dispatched, so the elevated window is up before we wait on it.
	seeMaskNoAsync = 0x00000100
)

// shellExecuteInfo is the SHELLEXECUTEINFOW structure with every SDK member
// in the SDK's order: cbSize, flags, an HWND, five string pointers, the
// show state, the hInstApp pointer the caller reads the outcome from, then
// lpIDList, lpClass, hkeyClass, dwHotKey, the hIcon/hMonitor union and the
// process handle ShellExecuteExW fills when asked to keep it. A member
// skipped here shifts every offset after it, so ShellExecuteExW would read
// or write the wrong memory.
type shellExecuteInfo struct {
	cbSize       uint32
	fMask        uint32
	hwnd         windows.Handle
	lpVerb       *uint16
	lpFile       *uint16
	lpParameters *uint16
	lpDirectory  *uint16
	nShow        int32
	hInstApp     windows.Handle
	lpIDList     uintptr
	lpClass      *uint16
	hkeyClass    windows.Handle
	dwHotKey     uint32
	hIconMonitor windows.Handle // the SDK's hIcon/hMonitor union; both are HANDLEs
	hProcess     windows.Handle
}

// execute invokes ShellExecuteExW and reports failure, with a declined UAC
// prompt surfacing as ErrDeclined, the error the dashboard translates into
// its own plain message.
func execute(sei *shellExecuteInfo) error {
	ok, _, err := shellExecuteExW.Call(uintptr(unsafe.Pointer(sei)))
	if ok != 0 {
		// The call started something; the process handle and its exit
		// code decide the rest.
		return nil
	}
	// A 0 return means "the user cancelled or the launch failed"; the
	// detail sits in the last-error value or, on some systems, in
	// hInstApp. Declining the UAC prompt reads exactly like the
	// cancellation error, and surfaces as the package's ErrDeclined.
	if code := uintptr(sei.hInstApp); code > 32 {
		return windows.Errno(code)
	}
	if errno, ok := err.(windows.Errno); ok {
		if errno == windows.ERROR_CANCELLED {
			return ErrDeclined
		}
		if errno != 0 {
			return errno
		}
	}
	return ErrDeclined
}

var shellExecuteExW = windows.NewLazySystemDLL("shell32.dll").
	NewProc("ShellExecuteExW")
