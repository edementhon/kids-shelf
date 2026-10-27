//go:build !windows

package update

import "os"

// syncParent fsyncs the directory containing path so a rename into it
// survives a crash on the same filesystem.
func syncParent(path string) error {
	d, err := os.Open(parentDir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func parentDir(path string) string {
	i := len(path) - 1
	for i >= 0 && (path[i] == '/' || path[i] == '\\') {
		i--
	}
	for i >= 0 && path[i] != '/' && path[i] != '\\' {
		i--
	}
	if i < 0 {
		return "."
	}
	return path[:i]
}
