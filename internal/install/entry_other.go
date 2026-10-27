//go:build !darwin

package install

import "errors"

// EntryMain exists only on macOS, where a kids-shelf.app entry bundle can
// be double-clicked; on every other platform the role does not exist.
func EntryMain() error {
	return errors.New("this copy is not the macOS entry bundle")
}
