//go:build windows

package update

import "golang.org/x/sys/windows"

// replace atomically replaces dst with src: MoveFileEx with
// MOVEFILE_REPLACE_EXISTING performs the swap in one call, the same
// mechanism Go's os.Rename uses on Windows.
func replace(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING)
}
