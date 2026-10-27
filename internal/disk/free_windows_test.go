//go:build windows

package disk

import (
	"fmt"
	"testing"
)

// TestFSKeyQueriesTheActualVolume pins the junction/mounted-folder fix:
// FSKey must ask GetVolumeInformation on the volume that actually backs
// path (what GetVolumePathName resolves), not the drive letter of the path
// itself, so a path reached through a junction onto another volume is
// attributed to that volume's serial.
func TestFSKeyQueriesTheActualVolume(t *testing.T) {
	oldRoot := volumeRootOf
	oldSerial := volumeSerialOf
	t.Cleanup(func() {
		volumeRootOf = oldRoot
		volumeSerialOf = oldSerial
	})

	// The path comes in through C:\ but the actual volume is D:\ (a
	// junction or mounted folder onto another volume).
	var queried string
	volumeRootOf = func(string) (string, error) { return `D:\`, nil }
	volumeSerialOf = func(root string) (uint32, error) {
		queried = root
		return 0x12ab, nil
	}
	key, err := FSKey(`C:\Program Files\junction\kids-shelf`)
	if err != nil {
		t.Fatalf("FSKey: %v", err)
	}
	if queried != `D:\` {
		t.Errorf("serial queried on volume %q, want the actual volume root %q", queried, `D:\`)
	}
	if want := fmt.Sprintf("%x", 0x12ab); key != want {
		t.Errorf("FSKey = %q, want %q (the actual volume's serial)", key, want)
	}
}
