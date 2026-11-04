package web

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/edementhon/kids-shelf/internal/budget"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

// TestContentVideoFolder: the parent chooses where new videos go. The
// YouTube list reports the folder in use; a good folder is saved and gets a
// "kids-shelf media" folder; bad paths are refused with plain words and
// change nothing; the folder holding the standard media folder counts as
// the standard choice; an unplugged drive shows as missing; an empty path
// goes back to the standard folder.
func TestContentVideoFolder(t *testing.T) {
	st := contentTestStore(t)
	b, err := budget.New(st)
	if err != nil {
		t.Fatal(err)
	}
	q, err := jobs.New(st)
	if err != nil {
		t.Fatal(err)
	}
	yt := youtube.NewManager(q, ingest.NewQueue(q), nil, nil, t.TempDir())
	home := t.TempDir()
	media := filepath.Join(home, "Videos", "kids-shelf media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatal(err)
	}
	changed := 0
	ts := contentTestServer(t, Deps{Budget: b, YouTube: yt, MediaDir: media,
		VideoFolderChanged: func() { changed++ }})

	folder := func() contentVideoFolder {
		t.Helper()
		code, body := contentTestGet(t, ts, "/api/admin/content/youtube")
		if code != http.StatusOK {
			t.Fatalf("youtube list: %d %s", code, body)
		}
		var v struct {
			Folder contentVideoFolder `json:"folder"`
		}
		contentTestDecode(t, body, &v)
		return v.Folder
	}
	set := func(path string) (int, string) {
		t.Helper()
		code, body := contentTestPost(t, ts, "/api/admin/content/videofolder", map[string]any{"path": path})
		if code != http.StatusOK {
			return code, contentTestError(t, body)
		}
		return code, ""
	}

	if f := folder(); f.Path != media || f.Chosen != "" || f.Missing {
		t.Errorf("standard folder = %+v, want path %s, nothing chosen", f, media)
	}

	drive := filepath.Join(t.TempDir(), "My Drive")
	if err := os.Mkdir(drive, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(drive, "notes.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	refusals := map[string]string{
		"a relative path":            "My Drive",
		"a folder that is not there": filepath.Join(drive, "nope"),
		"a file":                     file,
		"inside a kids-shelf media":  media,
		"deeper in kids-shelf media": filepath.Join(media, "library"),
	}
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		locked := filepath.Join(t.TempDir(), "read-only")
		if err := os.Mkdir(locked, 0o555); err != nil {
			t.Fatal(err)
		}
		refusals["a folder Kids Shelf cannot write to"] = locked
	}
	for name, path := range refusals {
		if code, msg := set(path); code != http.StatusBadRequest || msg == "" {
			t.Errorf("%s (%s): %d %q, want 400 with a message", name, path, code, msg)
		}
	}
	if f := folder(); f.Chosen != "" || changed != 0 {
		t.Fatalf("a refused folder changed the setting: %+v (changed %d)", f, changed)
	}

	if code, msg := set(drive); code != http.StatusOK {
		t.Fatalf("set the drive: %d %q", code, msg)
	}
	want := filepath.Join(drive, "kids-shelf media")
	if f := folder(); f.Chosen != drive || f.Path != want || f.Missing || f.Free < 0 {
		t.Errorf("after choosing the drive = %+v, want chosen %s, path %s, free space known", f, drive, want)
	}
	if st, err := os.Stat(filepath.Join(want, "library")); err != nil || !st.IsDir() {
		t.Errorf("the drive has no kids-shelf media/library folder: %v", err)
	}
	if changed != 1 {
		t.Errorf("VideoFolderChanged ran %d times, want 1", changed)
	}

	// The drive is unplugged: its folder is reported missing, not created.
	if err := os.RemoveAll(drive); err != nil {
		t.Fatal(err)
	}
	if f := folder(); !f.Missing || f.Chosen != drive {
		t.Errorf("unplugged drive = %+v, want missing", f)
	}
	if _, err := os.Stat(drive); !os.IsNotExist(err) {
		t.Errorf("reading the setting recreated the drive folder (stat %v)", err)
	}

	// The folder that holds the standard media folder is the standard choice.
	if code, msg := set(filepath.Dir(media)); code != http.StatusOK {
		t.Fatalf("set the Videos folder: %d %q", code, msg)
	}
	if f := folder(); f.Chosen != "" || f.Path != media {
		t.Errorf("choosing the Videos folder = %+v, want the standard folder", f)
	}

	other := t.TempDir()
	if code, msg := set(other); code != http.StatusOK {
		t.Fatalf("set another folder: %d %q", code, msg)
	}
	if code, msg := set(""); code != http.StatusOK {
		t.Fatalf("back to the standard folder: %d %q", code, msg)
	}
	if f := folder(); f.Chosen != "" || f.Path != media {
		t.Errorf("after going back = %+v, want the standard folder", f)
	}
}
