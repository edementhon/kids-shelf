//go:build !windows

package update

import "os"

// replace atomically replaces dst with src: on unix, rename(2) replaces an
// existing destination in one step.
func replace(src, dst string) error {
	return os.Rename(src, dst)
}
