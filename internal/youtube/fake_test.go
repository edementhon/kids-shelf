package youtube

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain doubles as the fake yt-dlp: when KS_FAKE_YTDLP names a mode, the
// test binary acts as yt-dlp for that mode instead of running tests. The
// manager and updater exec this same binary as yt-dlp; the env var travels
// with the process, so one setting drives every helper instance in a test.
//
//	single     - resolve returns one video; downloads succeed
//	nojsresolve - resolve returns one video, but fails any resolve run
//	            - without --js-runtimes (current yt-dlp refuses YouTube
//	            - without a JavaScript runtime)
//	playlist3  - resolve returns a playlist of three videos; downloads succeed
//	channel    - resolve returns a channel whose Videos/Shorts tabs are
//	            - playlists of two videos each (nested -J calls)
//	update     - downloads fail until the binary lives in the version
//	            - directory named by KS_FAKE_YTDLP_NEWTAG (the "after the
//	            - updater installed the new release" state)
//	fail       - resolve succeeds; downloads always fail
//	nospace    - downloads fail printing "No space left on device"
//	nospace-part - the download writes a 2.5 GiB sparse .part file, then
//	            - fails printing "No space left on device"
//	badresolve - resolve fails
//
// A "single", "nospace-part" or "resume" invocation that finds an existing
// .part file for the target completes it (the resume case).
//
// When a mode would download: a -J run without --flat-playlist is the
// download's size probe and answers with the resolved format selection's
// sizes; the other -J runs are video/playlist resolves.
//
// Every invocation appends its command line to $KS_FAKE_YTDLP_LOG when set,
// so tests can count download attempts and see which binary ran.
func TestMain(m *testing.M) {
	if mode := os.Getenv("KS_FAKE_YTDLP"); mode != "" {
		os.Exit(fakeYtDlp(mode))
	}
	os.Exit(m.Run())
}

func fakeYtDlp(mode string) int {
	logCall()
	args := os.Args[1:]
	if hasArg(args, "--version") || hasArg(args, "-V") {
		// --version must print the release tag; the tag is the name of
		// the directory the binary was installed into.
		exe, err := os.Executable()
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
			return 1
		}
		fmt.Println(filepath.Base(filepath.Dir(exe)))
		return 0
	}
	if hasArg(args, "-J") && !hasArg(args, "--flat-playlist") {
		// The download's size probe: no flat listing, just the format
		// selection's filesizes.
		return fakeSize(mode)
	}
	if hasArg(args, "-J") || hasArg(args, "--flat-playlist") {
		return fakeResolve(mode, args)
	}
	return fakeDownload(mode, args)
}

func fakeResolve(mode string, args []string) int {
	if mode == "nojsresolve" && !hasArg(args, "--js-runtimes") {
		// Current yt-dlp refuses to extract YouTube without a JS runtime;
		// its error makes the video look unavailable, and only the warning
		// above reveals the real cause.
		fmt.Fprintln(os.Stderr, "WARNING: [youtube] No supported JavaScript runtime could be found. Only deno is enabled by default; to use another runtime add --js-runtimes RUNTIME[:PATH] to your command/config.")
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] "+videoIDFromURL(lastArg(args))+": This video is not available")
		return 1
	}
	switch mode {
	case "badresolve":
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] Unsupported URL: https://example.com/nope")
		return 1
	case "playlist3":
		fmt.Println(`{"id":"PLfake","title":"Fake Playlist","uploader":"Fake Channel","channel":"Fake Channel","entries":[` +
			`{"id":"vid-aaa","title":"Fake A"},` +
			`{"id":"vid-bbb","title":"Fake B"},` +
			`{"id":"vid-ccc","title":"Fake C"}]}`)
		return 0
	case "channel":
		// The channel root lists its two tab playlists (flat output:
		// no entries inside the tabs); each tab URL serves its videos.
		link := lastArg(args)
		switch {
		case strings.Contains(link, "/videos"):
			fmt.Println(`{"id":"tab-videos","title":"Videos","uploader":"Fake Channel","channel":"Fake Channel","_type":"playlist","entries":[` +
				`{"_type":"url","id":"chan-1a","title":"Channel Video 1A","url":"https://www.youtube.com/watch?v=chan-1a"},` +
				`{"_type":"url","id":"chan-1b","title":"Channel Video 1B","url":"https://www.youtube.com/watch?v=chan-1b"}]}`)
		case strings.Contains(link, "/shorts"):
			fmt.Println(`{"id":"tab-shorts","title":"Shorts","uploader":"Fake Channel","channel":"Fake Channel","_type":"playlist","entries":[` +
				`{"_type":"url","id":"chan-2a","title":"Channel Video 2A","url":"https://www.youtube.com/watch?v=chan-2a"},` +
				`{"_type":"url","id":"chan-2b","title":"Channel Video 2B","url":"https://www.youtube.com/watch?v=chan-2b"}]}`)
		default:
			fmt.Println(`{"id":"channel-1","title":"Fake Channel","uploader":"Fake Channel","channel":"Fake Channel","_type":"channel","entries":[` +
				`{"_type":"playlist","id":"tab-videos","title":"Videos","ie_key":"YoutubeTab","url":"https://www.youtube.com/@fakelab/videos"},` +
				`{"_type":"playlist","id":"tab-shorts","title":"Shorts","ie_key":"YoutubeTab","url":"https://www.youtube.com/@fakelab/shorts"}]}`)
		}
		return 0
	}
	id := videoIDFromURL(lastArg(args))
	fmt.Printf(`{"id":%q,"title":%q,"uploader":"Fake Channel","channel":"Fake Channel"}`, id, "Fake Title "+id)
	fmt.Println()
	return 0
}

// fakeSize answers the download's size probe: the -J run without
// --flat-playlist that asks what the sorted selection will cost. Two
// requested formats (a video and an audio stream) keep the merge's second
// copy in the reservation. Sizes here are *selected* sizes; the manager
// reserves 2.2x.
func fakeSize(mode string) int {
	vid, aud := int64(500<<20), int64(64<<20) // 500 MiB video + 64 MiB audio
	if mode == "nospace" || mode == "nospace-part" {
		// These modes stress the unknown-size path of the reservation
		// monitor: everything but the monitor's own check.
		vid, aud = 0, 0
	}
	fmt.Printf(`{"id":"probe","filesize":%d,"filesize_approx":%d,`+
		`"requested_formats":[{"format_id":"137","ext":"mp4","filesize":%d,"filesize_approx":%d},`+
		`{"format_id":"140","ext":"m4a","filesize":%d,"filesize_approx":%d}]}`,
		vid+aud, vid+aud, vid, vid, aud, aud)
	fmt.Println()
	return 0
}

const (
	// fakePartVideo = 2.5 GiB: bigger than the 2 GiB unknown-size
	// reservation, so the download monitor must try to extend it.
	fakePartVideo = 5 << 29
)

func fakeDownload(mode string, args []string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
		return 1
	}
	if mode == "update" && !strings.Contains(exe, os.Getenv("KS_FAKE_YTDLP_NEWTAG")) {
		fmt.Fprintln(os.Stderr, "ERROR: unable to download video data: HTTP Error 403: Forbidden")
		return 1
	}
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "ERROR: unable to download video data: HTTP Error 403: Forbidden")
		return 1
	}
	id := videoIDFromURL(lastArg(args))
	out := outputPath(args, id)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
		return 1
	}
	// A leftover .part file means the manager resumed this job: continue
	// the partial file instead of restarting it, so the download ends up
	// complete either way.
	part := out + ".part"
	if _, err := os.Stat(part); err == nil {
		if err := appendFile(part, []byte("rest of "+id+"\n")); err != nil {
			fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
			return 1
		}
		if err := os.Rename(part, out); err != nil {
			fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
			return 1
		}
		fmt.Printf("[download] Destination: %s\n", out)
		fmt.Println("[download] 100% of 10.00MiB at 5.00MiB/s ETA 00:00")
		return 0
	}
	if mode == "nospace-part" {
		// Write the partial download the monitor must not throw away,
		// as a sparse file so tests stay fast and small.
		f, err := os.Create(part)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
			return 1
		}
		if err := f.Truncate(fakePartVideo); err != nil {
			f.Close()
			fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
			return 1
		}
		if err := f.Close(); err != nil {
			fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
			return 1
		}
		fmt.Printf("[download] Destination: %s\n", out)
		fmt.Println("[download]  50.0% of 10.00MiB at 5.00MiB/s ETA 00:01")
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] abc123: No space left on device")
		return 1
	}
	if mode == "nospace" {
		fmt.Fprintln(os.Stderr, "ERROR: [youtube] abc123: No space left on device")
		return 1
	}
	fmt.Printf("[download] Destination: %s\n", out)
	fmt.Println("[download]  12.5% of 10.00MiB at 5.00MiB/s ETA 00:02")
	fmt.Println("[download]  50.0% of 10.00MiB at 5.00MiB/s ETA 00:01")
	fmt.Println("[download] 100% of 10.00MiB at 5.00MiB/s ETA 00:00")
	if err := os.WriteFile(out, []byte("fake video data for "+id+"\n"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fake yt-dlp:", err)
		return 1
	}
	return 0
}

// appendFile appends b to path, creating it if necessary.
func appendFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(b)
	return err
}

// outputPath substitutes the video id into the -o template: the manager
// passes "<tmp>/%(id)s.%(ext)s", and the fake always produces an mp4.
func outputPath(args []string, id string) string {
	tmpl := ""
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			tmpl = args[i+1]
		}
	}
	if tmpl == "" {
		return id + ".mp4"
	}
	base := filepath.Base(tmpl)
	base = strings.ReplaceAll(base, "%(id)s", id)
	base = strings.ReplaceAll(base, "%(ext)s", "mp4")
	return filepath.Join(filepath.Dir(tmpl), base)
}

func logCall() {
	path := os.Getenv("KS_FAKE_YTDLP_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake yt-dlp: log:", err)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s: %s\n", os.Getenv("KS_FAKE_YTDLP"), strings.Join(os.Args, " "))
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}
