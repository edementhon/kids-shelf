package web

import (
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/edementhon/kids-shelf/internal/paths"
)

// The "save new videos in" folder (Add from YouTube on the content page):
// YouTube downloads and converted copies made from now on go to a "kids-shelf
// media" folder inside the folder a parent chose, for example an external
// drive; "" is the standard media folder. Videos already saved stay where
// they are and keep playing (episodes store absolute paths); offline
// libraries and thumbnails stay in the standard media folder.

func init() {
	register(func(s *Server) {
		s.mux.HandleFunc("POST /api/admin/content/videofolder", s.handleContentVideoFolder)
	})
}

// contentVideoFolder tells the page where new videos go.
type contentVideoFolder struct {
	// Path is the folder new videos are saved in: the standard media
	// folder, or the "kids-shelf media" folder inside the chosen one.
	Path string `json:"path"`
	// Chosen is the folder the parent chose, "" for the standard one.
	Chosen string `json:"chosen"`
	// Free is the free space on Path's drive in bytes, -1 when unknown.
	Free int64 `json:"free"`
	// Missing: the chosen folder is not there right now (its drive is
	// unplugged); new videos wait for it.
	Missing bool `json:"missing"`
}

func (s *Server) videoFolderView(ctx context.Context) (contentVideoFolder, error) {
	v := contentVideoFolder{Path: s.deps.MediaDir, Free: -1}
	if s.deps.Budget == nil {
		return v, nil
	}
	chosen, err := s.deps.Budget.VideoFolder(ctx)
	if err != nil {
		return v, err
	}
	probe := s.deps.MediaDir
	if chosen != "" {
		v.Chosen, v.Path, probe = chosen, paths.MediaIn(chosen), chosen
		if st, err := os.Stat(chosen); err != nil || !st.IsDir() {
			v.Missing = true
			return v, nil
		}
	}
	if probe != "" {
		if free, err := contentFreeSpace(probe); err == nil && free <= math.MaxInt64 {
			v.Free = int64(free)
		}
	}
	return v, nil
}

// exampleDriveFolder is an example path for the folder form: a drive
// plugged into this computer, as this OS names it.
func exampleDriveFolder() string {
	switch runtime.GOOS {
	case "windows":
		return `E:\`
	case "darwin":
		return "/Volumes/MyDrive"
	default:
		return "/media/you/MyDrive"
	}
}

// handleContentVideoFolder saves the folder for new videos. An empty path
// returns to the standard folder; any other path must be the whole path of
// a folder that is there now and that Kids Shelf can write to (it makes its
// "kids-shelf media" folder inside and writes a test file there).
func (s *Server) handleContentVideoFolder(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Budget == nil {
		writeError(w, http.StatusServiceUnavailable, msgAdminServiceDown)
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	path := strings.TrimSpace(req.Path)
	if path != "" {
		if !filepath.IsAbs(path) {
			writeError(w, http.StatusBadRequest, "Enter the whole path to the folder, like "+exampleDriveFolder()+".")
			return
		}
		path = filepath.Clean(path)
		for _, part := range strings.FieldsFunc(path, func(c rune) bool { return c == '/' || c == '\\' }) {
			if part == paths.MediaDirName {
				writeError(w, http.StatusBadRequest, "Choose the folder that should hold the videos. Kids Shelf makes its own “"+paths.MediaDirName+"” folder inside it.")
				return
			}
		}
		st, err := os.Stat(path)
		if err != nil {
			writeError(w, http.StatusBadRequest, "We couldn't find that folder on this computer. Check the path. A drive has to be plugged in first.")
			return
		}
		if !st.IsDir() {
			writeError(w, http.StatusBadRequest, "That is a file, not a folder. Enter the folder that should hold the videos.")
			return
		}
		if !canWriteIn(filepath.Join(paths.MediaIn(path), "library")) {
			writeError(w, http.StatusBadRequest, "Kids Shelf can't save files in that folder. Choose another one, or check that the drive isn't read-only.")
			return
		}
		// The folder that already holds the standard media folder is the
		// standard choice, not a new one.
		if s.deps.MediaDir != "" && filepath.Clean(paths.MediaIn(path)) == filepath.Clean(s.deps.MediaDir) {
			path = ""
		}
	}
	if err := s.deps.Budget.SetVideoFolder(r.Context(), path); err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	if s.deps.VideoFolderChanged != nil {
		s.deps.VideoFolderChanged()
	}
	v, err := s.videoFolderView(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "folder": v})
}

// canWriteIn makes dir (and the folders above it that are missing) and
// reports whether a file can be written there. On a Mac this is also the
// moment macOS asks whether Kids Shelf may use the drive.
func canWriteIn(dir string) bool {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	return os.Remove(name) == nil
}
