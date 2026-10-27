//go:build windows

package update

// syncParent is a no-op on Windows: directories are not fsyncable through
// the standard library, and MoveFileEx already makes the rename durable.
func syncParent(path string) error { return nil }
