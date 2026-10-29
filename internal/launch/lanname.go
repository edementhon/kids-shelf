package launch

import (
	"fmt"
	"os"

	"github.com/edementhon/kids-shelf/internal/mdns"
)

// envLANName tells the app the name tablets can use to open the shelf
// (mdns.Name), or "" when the launcher is not answering to it. The parent
// page shows the name only then.
const envLANName = "KS_LAN_NAME"

// announceName starts answering to mdns.Name on the home network when
// opts.Announce is set. It returns the name for the app ("" when tablets
// cannot use it: no home network, the port could not be opened, or another
// device already answers to the name) and the function that stops
// answering. The shelf works by address either way.
func announceName(opts Options) (string, func()) {
	if !opts.Announce {
		return "", func() {}
	}
	logf := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "kids-shelf: "+format+"\n", args...)
	}
	r, err := mdns.Start(logf)
	if err != nil {
		logf("tablets cannot open kids-shelf by the name %s, only by its address: %v", mdns.Name, err)
		return "", func() {}
	}
	return mdns.Name, func() { r.Close() }
}
