//go:build windows

package firewall

import (
	"testing"
	"unsafe"
)

// TestShellExecuteInfoLayout pins the SHELLEXECUTEINFOW ABI the SDK
// documents: 112 bytes on 64-bit Windows with hProcess at offset 104 (60
// and 56 on 32-bit). ShellExecuteExW reads the structure through the raw
// pointer execute passes it, so a missing or reordered member shifts every
// field after it and the process handle lands on the wrong offset.
func TestShellExecuteInfoLayout(t *testing.T) {
	ptrSize := unsafe.Sizeof(uintptr(0))
	var wantSize, wantProcess uintptr
	if ptrSize == 8 {
		wantSize, wantProcess = 112, 104
	} else {
		wantSize, wantProcess = 60, 56
	}
	if got := unsafe.Sizeof(shellExecuteInfo{}); got != wantSize {
		t.Errorf("unsafe.Sizeof(shellExecuteInfo{}) = %d, want %d", got, wantSize)
	}
	if got := unsafe.Offsetof(shellExecuteInfo{}.hProcess); got != wantProcess {
		t.Errorf("unsafe.Offsetof(shellExecuteInfo{}.hProcess) = %d, want %d", got, wantProcess)
	}
}
