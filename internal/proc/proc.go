// Package proc starts helper programs (ffmpeg, ffprobe, qjs, yt-dlp,
// kiwix-serve, PowerShell) without a console window on Windows. kids-shelf
// is a -H=windowsgui build there, and Windows gives every console program
// that a GUI process starts its own console window unless the start asks
// for none; a parent closing one of those windows kills the work inside it.
// On other systems these are exec.Command and exec.CommandContext.
package proc

import (
	"context"
	"os/exec"
)

// Command is exec.Command with Hide applied. Use it for processes whose
// lifetime the caller manages itself; nothing kills them on a context.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	Hide(cmd)
	return cmd
}

// CommandContext is exec.CommandContext with Hide applied: the process is
// killed when ctx ends.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	Hide(cmd)
	return cmd
}
