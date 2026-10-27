//go:build !windows

package firewall

import (
	"context"
	"errors"
)

// newController builds a controller on every platform: the type and its
// caching are shared, and the stubs below make its checks fail cleanly
// outside Windows (a failed probe reports Unknown, and RulesExist false).
// Callers that only matter on Windows check runtime.GOOS themselves.
func newController(program string) Control {
	return &controller{program: program}
}

// The stubs below make the shared controller compile and fail honestly on
// platforms where there is no Windows Firewall to talk to.

func (c *controller) query(ctx context.Context) ([]byte, error) {
	// The test hook (when installed) answers instead of the platform
	// stub; otherwise this platform has no meaningful check.
	if out, err, ok := c.hookProbe(ctx); ok {
		return out, err
	}
	return nil, errors.New("firewall checks only exist on Windows")
}

func trimBOM(b []byte) []byte {
	return b
}

func runElevated(ctx context.Context, cmd string) error {
	return errors.New("firewall rules only exist on Windows")
}
