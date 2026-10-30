// The /admin/content area of the parent dashboard: add videos from YouTube,
// register folder and NAS sources, watch conversions (ingest) and manage the
// Kiwix libraries (catalog, downloads, installed ZIMs).
//
// Parent PIN: the /admin area owns the gate (admin.go: requireAdminPIN,
// the ks_admin_pin cookie and <DataDir>/dashboard-pin.json). Every data
// route here calls that gate first, so the parent PIN (always required)
// gates /admin/content exactly like /admin; the page shell is served to
// everyone and the page asks for the PIN when its first data call comes
// back 401, or sends the parent to /admin to make one when the 401 says
// setup. This file therefore builds only together with admin.go.
package web

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/kiwix"
	"github.com/edementhon/kids-shelf/internal/library"
	"github.com/edementhon/kids-shelf/internal/youtube"
)

//go:embed static/content
var contentFiles embed.FS

// Plain-words service-down and failure messages; internal details never
// reach a page (the writeError contract).
const (
	msgContentYouTubeDown = "This copy is not running the YouTube service."
	msgContentLibraryDown = "This copy is not running the library service."
	msgContentIngestDown  = "This copy is not running the conversions service."
	msgContentKiwixDown   = "This copy is not running the libraries service."
	msgContentSaveFailed  = "That could not be saved."
	msgContentLoadFailed  = "This list couldn't be loaded. Reload the page to try again."
	msgContentBadLink     = "That doesn't look like a YouTube link. On YouTube, press Share, then Copy link, and paste it here."
	msgContentBadBody     = "That request couldn't be read."
	msgContentCatalogDown = "The library catalog couldn't be reached. Check that this computer is online, then try again."
)

// contentDoneShown is how long a finished conversion stays in the
// conversions list: long enough to see that today's videos arrived.
const contentDoneShown = 24 * time.Hour

// contentFreeSpace probes the free bytes on the media drive for the download
// confirmation and the pre-download space check. Tests stub it to a fixed
// number; the default is disk.Free on the media directory.
var contentFreeSpace = disk.Free

func init() {
	register(func(s *Server) {
		m := s.mux
		m.HandleFunc("GET /admin/content", s.handleContentPage)
		m.HandleFunc("GET /admin/content/static/", s.handleContentStatic)
		m.HandleFunc("GET /api/admin/content/youtube", s.handleContentYouTubeList)
		m.HandleFunc("POST /api/admin/content/youtube", s.handleContentYouTubeAdd)
		m.HandleFunc("POST /api/admin/content/youtube/retry", s.handleContentYouTubeRetry)
		m.HandleFunc("GET /api/admin/content/sources", s.handleContentSourcesList)
		m.HandleFunc("POST /api/admin/content/sources", s.handleContentSourcesAdd)
		m.HandleFunc("POST /api/admin/content/sources/remove", s.handleContentSourcesRemove)
		m.HandleFunc("POST /api/admin/content/scan", s.handleContentScan)
		m.HandleFunc("POST /api/admin/content/ingest/retry", s.handleContentIngestRetry)
		m.HandleFunc("GET /api/admin/content/ingest", s.handleContentIngestList)
		m.HandleFunc("GET /api/admin/content/fixes", s.handleContentFixes)
		m.HandleFunc("GET /api/admin/content/kiwix/catalog", s.handleContentKiwixCatalog)
		m.HandleFunc("GET /api/admin/content/kiwix/categories", s.handleContentKiwixCategories)
		m.HandleFunc("GET /api/admin/content/kiwix/languages", s.handleContentKiwixLanguages)
		m.HandleFunc("GET /api/admin/content/kiwix", s.handleContentKiwixState)
		m.HandleFunc("POST /api/admin/content/kiwix/download", s.handleContentKiwixDownload)
		m.HandleFunc("POST /api/admin/content/kiwix/remove", s.handleContentKiwixRemove)
		m.HandleFunc("GET /api/admin/content/kiwix/folders", s.handleContentKiwixFolders)
		m.HandleFunc("POST /api/admin/content/kiwix/folders", s.handleContentKiwixFolderAdd)
		m.HandleFunc("POST /api/admin/content/kiwix/folders/remove", s.handleContentKiwixFolderRemove)
		m.HandleFunc("POST /api/admin/content/kiwix/folders/rescan", s.handleContentKiwixFolderRescan)
	})
}

// ─── page and static files ─────────────────────────────────────────────────

// handleContentPage serves the page shell to everyone; the page shows the
// PIN form when its first data call comes back 401 (all data is behind
// requireAdminPIN, never the shell), or goes to /admin to make a PIN first.
func (s *Server) handleContentPage(w http.ResponseWriter, r *http.Request) {
	page, err := contentPage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "This page is missing from this copy.")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, page)
}

// contentPage is the page shell with this OS's folder-path examples filled
// in and its asset URLs versioned (see versionAssets).
var contentPage = sync.OnceValues(func() (string, error) {
	b, err := contentFiles.ReadFile("static/content/index.html")
	if err != nil {
		return "", err
	}
	page := strings.Replace(string(b), "__KS_OS_PATH__", exampleMediaPath(), 1)
	page = strings.Replace(page, "__KS_OS_LIBRARY_PATH__", exampleLibraryPath(), 1)
	return versionAssets(page, contentFiles, "static/content", "/admin/content/static/")
})

// exampleMediaPath is the folder-path example the sources form shows in its
// input placeholder: the path shape of the OS this copy runs on, so a Mac
// parent never sees a Linux path and vice versa.
func exampleMediaPath() string {
	switch runtime.GOOS {
	case "windows":
		return `C:\Users\you\Videos\Kids`
	case "darwin":
		return "/Users/you/Movies/Kids"
	default:
		return "/home/you/Videos/Kids"
	}
}

// exampleLibraryPath is the same kind of example for the library folder
// form: a folder of library files (.zim) on this OS.
func exampleLibraryPath() string {
	switch runtime.GOOS {
	case "windows":
		return `D:\Kiwix`
	case "darwin":
		return "/Users/you/Documents/Kiwix"
	default:
		return "/home/you/Kiwix"
	}
}

// handleContentStatic serves the area's embedded files (content.js,
// content.css) with no directory listing. The page asks for them with a
// content hash in the query (see versionAssets), which is ignored here.
func (s *Server) handleContentStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/content/static/")
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := contentFiles.ReadFile("static/content/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctype := "application/octet-stream"
	switch {
	case strings.HasSuffix(name, ".js"):
		ctype = "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		ctype = "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".html"):
		ctype = "text/html; charset=utf-8"
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write(b)
}

// ─── YouTube ────────────────────────────────────────────────────────────────

type contentYouTubeAddRequest struct {
	URL string `json:"url"`
}

// contentYouTubeJob is one YouTube-list row: the download job plus, once its
// video went to the shelf, the show it landed in (the channel name, which a
// parent would not guess).
type contentYouTubeJob struct {
	youtube.JobView
	Show string `json:"show,omitempty"`
}

// handleContentYouTubeList lists the YouTube jobs, newest first. A failed
// job that a later job for the same link replaced (a Retry, or the same link
// pasted again) is left out, so failures do not pile up after a retry.
func (s *Server) handleContentYouTubeList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.YouTube == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentYouTubeDown)
		return
	}
	list, err := s.deps.YouTube.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	// The ingest job of a downloaded video carries its watch URL as the
	// source path and the show it was filed under.
	shows := map[string]string{}
	if s.deps.Ingest != nil {
		if ing, err := s.deps.Ingest.Jobs(); err == nil {
			for _, j := range ing {
				if j.Source.Kind == core.SourceYouTube && j.Source.Show != "" {
					if _, ok := shows[j.Source.Path]; !ok {
						shows[j.Source.Path] = j.Source.Show
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	out := make([]contentYouTubeJob, 0, len(list))
	for _, j := range list {
		if j.State == jobs.StateFailed && seen[j.URL] {
			continue // replaced by a newer job for the same link
		}
		seen[j.URL] = true
		row := contentYouTubeJob{JobView: j}
		if j.State == jobs.StateDone {
			row.Show = shows[j.URL]
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// handleContentYouTubeAdd enqueues a YouTube download for a video, playlist
// or channel URL. Only real YouTube addresses pass: the URL is parsed and
// rebuilt into its canonical https://www.youtube.com form, so yt-dlp never
// sees a foreign host or a client-chosen URL shape.
func (s *Server) handleContentYouTubeAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.YouTube == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentYouTubeDown)
		return
	}
	var req contentYouTubeAddRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	link := strings.TrimSpace(req.URL)
	if link == "" {
		writeError(w, http.StatusBadRequest, "Paste the web address of the video, playlist or channel first.")
		return
	}
	canonical, err := contentYouTubeCanonical(link)
	if err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadLink)
		return
	}
	id, err := s.deps.YouTube.Enqueue(r.Context(), canonical)
	if err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadLink)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// youtubeHosts are the YouTube properties the download endpoint accepts;
// anything else is refused before yt-dlp ever sees it.
var youtubeHosts = map[string]bool{
	"youtube.com":       true,
	"www.youtube.com":   true,
	"m.youtube.com":     true,
	"youtu.be":          true,
	"music.youtube.com": true,
}

// contentYouTubeCanonical validates raw against the YouTube URL shapes the
// shelf can download (watch, shorts, playlist, @handle, channel, c, user)
// and returns the canonical https://www.youtube.com URL rebuilt from the
// parsed id. Anything else is an error, so a paste can never smuggle a
// foreign address or URL parameter into the downloader.
func contentYouTubeCanonical(raw string) (string, error) {
	// A link typed or pasted without its "https://" (youtube.com/watch?v=...)
	// is still a YouTube link; the host check below still applies.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("not a youtube url")
	}
	host := strings.ToLower(u.Host)
	if !youtubeHosts[host] {
		return "", errors.New("not a youtube url")
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if host == "youtu.be" {
		if len(segs) == 0 || segs[0] == "" {
			return "", errors.New("not a youtube url")
		}
		if !isYouTubeVideoID(segs[0]) {
			return "", errors.New("not a youtube url")
		}
		return watchURLFromID(segs[0]), nil
	}
	head := ""
	if len(segs) > 0 {
		head = segs[0]
	}
	switch head {
	case "watch":
		if id := u.Query().Get("v"); isYouTubeVideoID(id) {
			return watchURLFromID(id), nil
		}
	case "shorts":
		if len(segs) > 1 && isYouTubeVideoID(segs[1]) {
			return watchURLFromID(segs[1]), nil
		}
	case "playlist":
		if id := u.Query().Get("list"); validYouTubeID(id) {
			return "https://www.youtube.com/playlist?list=" + id, nil
		}
	case "channel":
		if len(segs) > 1 && validYouTubeID(segs[1]) {
			return "https://www.youtube.com/channel/" + segs[1], nil
		}
	case "c":
		if len(segs) > 1 && validYouTubeID(segs[1]) {
			return "https://www.youtube.com/c/" + segs[1], nil
		}
	case "user":
		if len(segs) > 1 && validYouTubeID(segs[1]) {
			return "https://www.youtube.com/user/" + segs[1], nil
		}
	default:
		if strings.HasPrefix(head, "@") && validYouTubeHandle(head) {
			return "https://www.youtube.com/" + head, nil
		}
	}
	return "", errors.New("not a youtube url")
}

// watchURLFromID is the canonical per-video URL, the one the youtube package
// itself uses as the ingest source path.
func watchURLFromID(id string) string {
	return "https://www.youtube.com/watch?v=" + id
}

// isYouTubeVideoID reports whether s is a video id: exactly 11 characters
// of the URL-safe alphabet, like every real YouTube video id.
func isYouTubeVideoID(s string) bool {
	return len(s) == 11 && urlSafeID(s)
}

// validYouTubeID reports whether s is a plausible playlist list id or
// channel id: up to 64 characters of the URL-safe alphabet.
func validYouTubeID(s string) bool {
	return len(s) >= 1 && len(s) <= 64 && urlSafeID(s)
}

func urlSafeID(s string) bool {
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			continue
		}
		return false
	}
	return true
}

// validYouTubeHandle reports whether s is a plausible @handle.
func validYouTubeHandle(s string) bool {
	if len(s) < 2 || len(s) > 65 {
		return false
	}
	return validYouTubeID(s[1:])
}

// handleContentYouTubeRetry starts a fresh download job for a failed one,
// re-using the URL the failed job was enqueued with.
func (s *Server) handleContentYouTubeRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.YouTube == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentYouTubeDown)
		return
	}
	var req contentJobRefRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	jobs, err := s.deps.YouTube.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	url := ""
	for i := range jobs {
		if jobs[i].ID == req.ID && jobs[i].State == "failed" {
			url = jobs[i].URL
			break
		}
	}
	if url == "" {
		writeError(w, http.StatusBadRequest, "That download can't be retried.")
		return
	}
	id, err := s.deps.YouTube.Enqueue(r.Context(), url)
	if err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadLink)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

type contentJobRefRequest struct {
	ID string `json:"id"`
}

// ─── folder and NAS sources ─────────────────────────────────────────────────

type contentSource struct {
	ID        int64    `json:"id"`
	Path      string   `json:"path"`
	Kind      string   `json:"kind"`
	Allowlist []string `json:"allowlist,omitempty"`
	// Missing counts the videos on the shelf from this folder whose
	// original file cannot be found.
	Missing int `json:"missing,omitempty"`
	// Problem is set when the folder cannot be scanned right now or videos
	// from it cannot be found, in plain words saying what to do.
	Problem string `json:"problem,omitempty"`
}

type contentSourcesAddRequest struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "folder" or "nas" (core.SourceFolder / core.SourceNAS)
}

// contentVideos is "1 video" or "N videos".
func contentVideos(n int) string {
	if n == 1 {
		return "1 video"
	}
	return strconv.Itoa(n) + " videos"
}

// contentSourceProblem says in plain words what is wrong with a registered
// folder, or "" when it is fine. statErr and fi are the result of one stat
// of the folder; missing is how many of the shelf's in-place videos from it
// cannot be found (kids only see "This one can't play right now", so the
// parent page has to say why).
func contentSourceProblem(kind core.SourceKind, fi os.FileInfo, statErr error, missing int) string {
	switch {
	case statErr != nil && kind == core.SourceNAS && missing > 0:
		return "kids-shelf can't find this NAS share right now, so " + contentVideos(missing) + " from it can't play. Check that the NAS is on and connected to this computer."
	case statErr != nil && kind == core.SourceNAS:
		return "kids-shelf can't find this NAS share right now. Check that the NAS is on and connected to this computer."
	case statErr != nil && missing > 0:
		return "kids-shelf can't find this folder right now, so " + contentVideos(missing) + " from it can't play. If it is on a USB drive, plug the drive in."
	case statErr != nil:
		return "kids-shelf can't find this folder right now. If it is on a USB drive, plug the drive in."
	case !fi.IsDir():
		return "This is a file, not a folder. Remove it and add the folder that holds your videos."
	case missing == 1:
		return "1 video from this folder can't be found, so kids can't play it. Is the drive or NAS connected? If you moved or renamed it, put it back."
	case missing > 1:
		return contentVideos(missing) + " from this folder can't be found, so kids can't play them. Is the drive or NAS connected? If you moved or renamed them, put them back."
	}
	return ""
}

// contentMissingStatCap bounds how many original files one folder-list
// request checks in folders that are there; a folder that is gone is
// counted without looking at its files.
const contentMissingStatCap = 5000

// contentMissingOriginals counts, per source (index into srcs), the
// in-place episodes whose original file cannot be found. Each episode
// belongs to the source whose root holds it (the deepest one when roots
// nest). When the root itself is gone (rootOK false) all of its episodes
// count as missing and no file is touched; otherwise each original is
// stat'ed, at most contentMissingStatCap in all.
func contentMissingOriginals(srcs []library.MediaSource, rootOK []bool, files []library.InPlaceFile) []int {
	prefixes := make([]string, len(srcs))
	for i, src := range srcs {
		canon, err := core.CanonicalPath(src.Path)
		if err != nil {
			continue
		}
		if !strings.HasSuffix(canon, "/") {
			canon += "/"
		}
		prefixes[i] = canon
	}
	missing := make([]int, len(srcs))
	budget := contentMissingStatCap
	for _, f := range files {
		best := -1
		for i, p := range prefixes {
			if p != "" && strings.HasPrefix(f.SourcePath, p) && (best < 0 || len(p) > len(prefixes[best])) {
				best = i
			}
		}
		switch {
		case best < 0:
		case !rootOK[best]:
			missing[best]++
		case budget > 0:
			budget--
			if _, err := os.Stat(f.File); err != nil {
				missing[best]++
			}
		}
	}
	return missing
}

// handleContentSourcesList lists the registered folders, each with a plain
// warning when it cannot be scanned or when videos on the shelf from it
// cannot be found.
func (s *Server) handleContentSourcesList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentLibraryDown)
		return
	}
	srcs, err := s.deps.Library.Sources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	files, err := s.deps.Library.InPlaceFiles(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	infos := make([]os.FileInfo, len(srcs))
	errs := make([]error, len(srcs))
	rootOK := make([]bool, len(srcs))
	for i, src := range srcs {
		infos[i], errs[i] = os.Stat(src.Path)
		rootOK[i] = errs[i] == nil && infos[i].IsDir()
	}
	missing := contentMissingOriginals(srcs, rootOK, files)
	out := make([]contentSource, 0, len(srcs))
	for i, src := range srcs {
		out = append(out, contentSource{
			ID: src.ID, Path: src.Path, Kind: string(src.Kind), Allowlist: src.Allowlist,
			Missing: missing[i],
			Problem: contentSourceProblem(src.Kind, infos[i], errs[i], missing[i]),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// contentSamePath reports whether two folder paths name the same folder,
// ignoring case where the OS does.
func contentSamePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// contentCleanSourcePath tidies a pasted folder path: surrounding spaces and
// the quotes Windows Explorer's "Copy as path" adds go, and the path is
// cleaned so "/media/" and "/media" are one folder.
func contentCleanSourcePath(p string) string {
	p = strings.TrimSpace(p)
	for len(p) >= 2 && (p[0] == '"' && p[len(p)-1] == '"' || p[0] == '\'' && p[len(p)-1] == '\'') {
		p = strings.TrimSpace(p[1 : len(p)-1])
	}
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// contentCountTimeout bounds the video count Add folder reports; a very
// large NAS share is added without a count rather than keeping the parent
// waiting.
const contentCountTimeout = 15 * time.Second

// handleContentSourcesAdd registers a media root. The path must be a folder
// that exists now and holds at least one video: a parent who typos a path,
// picks a file or an empty folder gets told at once, and a NAS share has to
// be connected before it can be scanned anyway (the periodic scan tolerates
// a root that vanishes later). A folder already on the list is refused. The
// answer carries how many videos the folder holds (-1 when counting took
// too long).
func (s *Server) handleContentSourcesAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentLibraryDown)
		return
	}
	var req contentSourcesAddRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	path := contentCleanSourcePath(req.Path)
	if path == "" {
		writeError(w, http.StatusBadRequest, "Enter the path to the folder first.")
		return
	}
	if req.Kind != string(core.SourceFolder) && req.Kind != string(core.SourceNAS) {
		writeError(w, http.StatusBadRequest, "Choose 'A folder on this computer' or 'A network drive (NAS)'.")
		return
	}
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, "Enter the whole path to the folder, like "+exampleMediaPath()+".")
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "We couldn't find that folder on this computer. Check the path. A NAS share has to be connected to this machine first.")
		return
	}
	if !fi.IsDir() {
		writeError(w, http.StatusBadRequest, "That is a file, not a folder. Enter the folder that holds your videos.")
		return
	}
	srcs, err := s.deps.Library.Sources(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	for _, src := range srcs {
		if contentSamePath(src.Path, path) {
			writeError(w, http.StatusBadRequest, "That folder is already on the list.")
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), contentCountTimeout)
	videos, err := library.CountVideos(ctx, path)
	cancel()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		videos = -1
	case err != nil:
		writeError(w, http.StatusBadRequest, "kids-shelf can't open that folder. Check that this computer is allowed to read it.")
		return
	case videos == 0:
		writeError(w, http.StatusBadRequest, "There are no videos in that folder. Put your videos in it (each show in its own folder inside), then add it again. kids-shelf finds .mp4, .mkv, .webm, .avi, .mov, .m4v and .ts files.")
		return
	}
	if err := s.deps.Library.AddSource(r.Context(), path, core.SourceKind(req.Kind), nil); err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "videos": videos})
}

type contentSourcesRemoveRequest struct {
	Path string `json:"path"`
}

// handleContentSourcesRemove takes a folder off the list. Its videos already
// on the shelf stay there; only the scanning stops (the page says so).
func (s *Server) handleContentSourcesRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentLibraryDown)
		return
	}
	var req contentSourcesRemoveRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	if err := s.deps.Library.RemoveSource(r.Context(), req.Path); err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleContentScan walks every registered source and hands new files (and
// changed files whose conversion failed) to the ingest queue. The answer
// says how many it handed over, so the page can say when nothing was new.
func (s *Server) handleContentScan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentLibraryDown)
		return
	}
	if s.deps.Ingest == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentIngestDown)
		return
	}
	added, err := s.deps.Library.ScanNew(r.Context(), s.deps.Ingest)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Scanning couldn't finish. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "added": added})
}

// ─── conversions (ingest jobs) ──────────────────────────────────────────────

// contentIngestJob is one conversions-list row: the ingest job as the queue
// reports it plus the name the page should lead with. Internal paths and
// URLs stay in File (rendered small underneath); Title is the episode or
// show title the enqueueing producer recorded, or the path when unknown.
type contentIngestJob struct {
	ingest.JobView
	Title string `json:"title"`
	// Retry is set on a failed conversion that can be tried again.
	Retry bool `json:"retry,omitempty"`
	// Hint says, for a failed conversion that cannot be retried, why not
	// and what to do instead.
	Hint string `json:"hint,omitempty"`
}

// contentIngestKey identifies the file a job converts, so a newer job for
// the same file is known to replace an older failed one. Jobs whose payload
// did not decode have no source and never match.
func contentIngestKey(j ingest.JobView) string {
	if j.Source.Path == "" {
		return ""
	}
	return string(j.Source.Kind) + "\x00" + j.Source.Path
}

// handleContentIngestList lists the conversions, newest first: everything
// waiting, running, paused or failed, and what finished in the last day. A
// failed job that a newer job for the same file replaced (a Retry, or a
// scan that found the file changed) is left out.
func (s *Server) handleContentIngestList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Ingest == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentIngestDown)
		return
	}
	list, err := s.deps.Ingest.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	cutoff := s.now().Add(-contentDoneShown)
	seen := map[string]bool{}
	out := make([]contentIngestJob, 0, len(list))
	for _, j := range list {
		key := contentIngestKey(j)
		replaced := key != "" && seen[key]
		seen[key] = true
		if j.State == jobs.StateFailed && replaced {
			continue
		}
		if j.State == jobs.StateDone && j.Updated.Before(cutoff) {
			continue
		}
		row := contentIngestJob{JobView: j, Title: contentIngestTitle(j)}
		if j.State == jobs.StateFailed {
			why, err := s.contentIngestRetryBlock(r.Context(), j)
			if err != nil {
				writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
				return
			}
			row.Retry, row.Hint = why == "", why
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

// contentIngestRetryBlock says why a failed conversion cannot be tried
// again, in plain words, or "" when it can: its video is not on the shelf
// (a failed improvement leaves the first copy playing) and its file is
// still there to convert.
func (s *Server) contentIngestRetryBlock(ctx context.Context, j ingest.JobView) (string, error) {
	if j.File == "" || j.Source.Path == "" {
		return "This conversion can't be tried again. Add the video again.", nil
	}
	if s.deps.Library != nil {
		_, found, err := s.deps.Library.BySource(ctx, j.Source)
		if err != nil {
			return "", err
		}
		if found {
			return "This video is on the shelf and plays as it is.", nil
		}
	}
	if fi, err := os.Stat(j.File); err != nil || !fi.Mode().IsRegular() {
		if j.Source.Kind == core.SourceYouTube {
			return "To try again, add it again from its YouTube link above.", nil
		}
		return "The file is not in its folder any more. Put it back to try again.", nil
	}
	return "", nil
}

// handleContentIngestRetry gives a failed conversion a new try: the same
// file and source go back to the ingest queue as a new job, which replaces
// the failed one in the list. Only the newest job for a file can be retried,
// and only when contentIngestRetryBlock allows it.
func (s *Server) handleContentIngestRetry(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Ingest == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentIngestDown)
		return
	}
	var req contentJobRefRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	list, err := s.deps.Ingest.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	idx := -1
	for i := range list {
		if list[i].ID == req.ID {
			idx = i
			break
		}
	}
	if idx < 0 || list[idx].State != jobs.StateFailed {
		writeError(w, http.StatusBadRequest, "That conversion isn't stuck any more. The list shows how it is doing.")
		return
	}
	j := list[idx]
	if key := contentIngestKey(j); key != "" {
		for _, newer := range list[:idx] { // the list is newest first
			if contentIngestKey(newer) == key {
				writeError(w, http.StatusBadRequest, "That video is already being tried again. The list shows how it is doing.")
				return
			}
		}
	}
	why, err := s.contentIngestRetryBlock(r.Context(), j)
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	if why != "" {
		writeError(w, http.StatusBadRequest, why)
		return
	}
	id, err := s.deps.Ingest.Enqueue(r.Context(), j.File, j.Source)
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// contentIngestTitle picks the name a parent recognises for an ingest job:
// the episode title the producer recorded (for folder sources the file name
// without its extension, for YouTube the video title), then the show name,
// and only as a last resort the full source path.
func contentIngestTitle(j ingest.JobView) string {
	if t := strings.TrimSpace(j.Source.Title); t != "" {
		return t
	}
	if s := strings.TrimSpace(j.Source.Show); s != "" {
		return s
	}
	return j.File
}

// ─── fixing videos ───────────────────────────────────────────────────────────

// contentFailedEpisode is one episode whose better copy failed, as the
// "Fixing videos" card lists it. The page renders Issue into plain words.
type contentFailedEpisode struct {
	Show  string `json:"show"`
	Title string `json:"title"`
	Issue string `json:"issue"` // "sound", "picture" or "format"
}

// handleContentFixes reports the state behind the "Fixing videos" card:
// how many episodes wait for a better copy per issue, how many could not
// be fixed, and the newest failed ones (at most library.FixingListCap,
// with FailedMore counting the rest for the "and N more" line).
func (s *Server) handleContentFixes(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Library == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentLibraryDown)
		return
	}
	sum, err := s.deps.Library.FixingSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	out := struct {
		Pending        map[string]int         `json:"pending"` // issue -> episodes waiting
		Failed         int                    `json:"failed"`
		FailedEpisodes []contentFailedEpisode `json:"failedEpisodes"`
		FailedMore     int                    `json:"failedMore"`
	}{
		Pending: map[string]int{
			core.IssueSound:   sum.PendingSound,
			core.IssuePicture: sum.PendingPicture,
			core.IssueFormat:  sum.PendingFormat,
		},
		Failed:     sum.Failed,
		FailedMore: sum.FailedMore,
	}
	for _, ep := range sum.FailedEpisodes {
		out.FailedEpisodes = append(out.FailedEpisodes, contentFailedEpisode{
			Show: ep.Show, Title: ep.Title, Issue: ep.Issue,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── Kiwix libraries ────────────────────────────────────────────────────────

// contentZIM is the catalog shape the page renders and sends back to start a
// download. Illustration URLs are deliberately not exposed: the page loads
// nothing from the internet.
type contentZIM struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	Language     string `json:"language"`
	Category     string `json:"category"`
	Flavour      string `json:"flavour,omitempty"`
	ArticleCount int    `json:"article_count"`
	MediaCount   int    `json:"media_count"`
	Size         int64  `json:"size"`
	Meta4URL     string `json:"meta4_url"`
	// Installed marks an edition already on the shelf, so the page offers
	// no Download for it.
	Installed bool `json:"installed,omitempty"`
}

func contentZIMFromEntry(e kiwix.Entry) contentZIM {
	return contentZIM{
		ID: e.ID, Name: e.Name, Title: e.Title, Summary: e.Summary,
		Language: e.Language, Category: e.Category, Flavour: e.Flavour,
		ArticleCount: e.ArticleCount, MediaCount: e.MediaCount,
		Size: e.Size, Meta4URL: e.Meta4URL,
	}
}

type contentZIMInstalled struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Title       string    `json:"title"`
	Flavour     string    `json:"flavour,omitempty"`
	Language    string    `json:"language"`
	File        string    `json:"file"`
	Size        int64     `json:"size"`
	InstalledAt time.Time `json:"installed_at"`
	// Missing is set when the library's file cannot be found (a drive that
	// is not plugged in, a NAS that is off, a file moved by hand): kids
	// cannot open it until it is back.
	Missing bool `json:"missing,omitempty"`
	// Folder is the library folder the library's file is in, for a library
	// kids-shelf uses where it is; "" for a download. Such a library has no
	// Remove: it leaves the shelf with its folder.
	Folder string `json:"folder,omitempty"`
}

type contentKiwixState struct {
	Installed []contentZIMInstalled `json:"installed"`
	Downloads []kiwix.JobView       `json:"downloads"`
	FreeBytes int64                 `json:"free_bytes"` // -1 when unknown
}

// contentInstalledFlavour recovers an installed library's edition from its
// file name, which kiwix stores as <name>_<flavour>_<YYYY-MM>.zim (the Kiwix
// downloads in a library folder are usually named the same way); "" when
// the name does not have that shape.
func contentInstalledFlavour(in kiwix.Installed) string {
	f, ok := strings.CutPrefix(in.FileBase(), in.Name+"_")
	if !ok {
		return ""
	}
	i := strings.LastIndexByte(f, '_')
	if i < 0 {
		return ""
	}
	return f[:i]
}

// contentSameEdition reports whether a catalog entry (or a download job's
// entry) is the installed library in: the same catalog id, or the same
// name and edition (a newer release of it has a new id).
func contentSameEdition(id, name, flavour string, in kiwix.Installed) bool {
	return id == in.ID || (name == in.Name && flavour == contentInstalledFlavour(in))
}

// contentKiwixDownloadRequest carries the catalog identifier of the library
// the parent wants: the entry id, or the name+flavour pair. Sizes and meta4
// URLs are never accepted from the page; the server looks the entry up in
// Deps.Catalog and uses its own copy of every field.
type contentKiwixDownloadRequest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Flavour string `json:"flavour"`
}

func (s *Server) handleContentKiwixCatalog(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	q := r.URL.Query()
	count := 20
	if v := q.Get("count"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
			count = n
		}
	}
	start := 0
	if v := q.Get("start"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			start = n
		}
	}
	res, err := s.deps.Catalog.Entries(r.Context(), kiwix.Query{
		Lang: q.Get("lang"), Category: q.Get("category"), Q: q.Get("q"),
		Start: start, Count: count,
	})
	if err != nil {
		writeError(w, http.StatusBadGateway, msgContentCatalogDown)
		return
	}
	var installed []kiwix.Installed
	if s.deps.Kiwix != nil {
		installed, _ = s.deps.Kiwix.Installed() // unknown: every edition keeps its Download button
	}
	entries := make([]contentZIM, 0, len(res.Entries))
	for _, e := range res.Entries {
		z := contentZIMFromEntry(e)
		for _, in := range installed {
			if contentSameEdition(e.ID, e.Name, e.Flavour, in) {
				z.Installed = true
				break
			}
		}
		entries = append(entries, z)
	}
	contentSortFlavours(entries)
	writeJSON(w, http.StatusOK, map[string]any{
		"total": res.Total, "start": res.Start, "count": res.Count, "entries": entries,
	})
}

// contentFlavourRank orders one library's flavour editions for display, the
// with-pictures (maxi) edition first, then text-only and short versions.
func contentFlavourRank(f string) int {
	switch f {
	case "maxi":
		return 0
	case "nopic":
		return 1
	case "mini":
		return 2
	}
	return 3
}

// contentZIMBaseName strips a known flavour suffix from a ZIM name, so the
// editions of one library ("wikipedia_en_100_maxi", "..._nopic") group
// together; unknown names are returned unchanged.
func contentZIMBaseName(name string) string {
	for _, suffix := range []string{"_maxi", "_nopic", "_mini"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// contentSortFlavours reorders one catalog page so that among the editions of
// one library the with-pictures one comes first; libraries themselves keep
// the feed's relative order.
func contentSortFlavours(entries []contentZIM) {
	first := make(map[string]int, len(entries))
	for i, z := range entries {
		if _, seen := first[contentZIMBaseName(z.Name)]; !seen {
			first[contentZIMBaseName(z.Name)] = i
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		bi, bj := contentZIMBaseName(entries[i].Name), contentZIMBaseName(entries[j].Name)
		if bi != bj {
			return first[bi] < first[bj]
		}
		return contentFlavourRank(entries[i].Flavour) < contentFlavourRank(entries[j].Flavour)
	})
}

func (s *Server) handleContentKiwixCategories(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	cs, err := s.deps.Catalog.Categories(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, msgContentCatalogDown)
		return
	}
	names := make([]string, 0, len(cs))
	for _, c := range cs {
		names = append(names, c.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"categories": names})
}

func (s *Server) handleContentKiwixLanguages(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	ls, err := s.deps.Catalog.Languages(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, msgContentCatalogDown)
		return
	}
	out := make([]contentLanguage, 0, len(ls))
	for _, l := range ls {
		out = append(out, contentLanguage{Code: l.Code, Name: l.Name, Count: l.Count})
	}
	writeJSON(w, http.StatusOK, map[string]any{"languages": out})
}

type contentLanguage struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// handleContentKiwixState reports what the page renders: the installed ZIMs,
// the download jobs and the free space left on the media drive.
func (s *Server) handleContentKiwixState(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	installed, err := s.deps.Kiwix.Installed()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	list, err := s.deps.Kiwix.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	out := contentKiwixState{Downloads: []kiwix.JobView{}, FreeBytes: -1, Installed: []contentZIMInstalled{}}
	for _, in := range installed {
		out.Installed = append(out.Installed, contentZIMInstalled{
			ID: in.ID, Name: in.Name, Title: in.Title, Flavour: contentInstalledFlavour(in),
			Language: in.Language, File: in.File, Size: in.Size, InstalledAt: in.InstalledAt,
			Missing: !s.deps.Kiwix.OnDisk(in), Folder: in.Folder,
		})
	}
	// A finished download stays listed for a day, and only while its
	// library is still on the shelf.
	cutoff := s.now().Add(-contentDoneShown)
	for _, j := range list {
		if j.State == jobs.StateDone {
			kept := false
			for _, in := range installed {
				if contentSameEdition(j.Entry.ID, j.Entry.Name, j.Entry.Flavour, in) {
					kept = true
					break
				}
			}
			if !kept || j.Updated.Before(cutoff) {
				continue
			}
		}
		out.Downloads = append(out.Downloads, j)
	}
	if s.deps.MediaDir != "" {
		if free, err := contentFreeSpace(s.deps.MediaDir); err == nil && free <= math.MaxInt64 {
			out.FreeBytes = int64(free)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// handleContentKiwixDownload starts a ZIM download after a server-side space
// check: the parent sees the size and the free space in the confirmation,
// and the download itself is refused with a plain message when the media
// drive cannot hold it (the 2 GiB floor matches the reservations every other
// job keeps aside). The entry comes from Deps.Catalog, never from the page:
// the request names a catalog identifier, and size and meta4 URL are the
// catalog's own, so a crafted body cannot point the downloader anywhere.
func (s *Server) handleContentKiwixDownload(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil || s.deps.Catalog == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	var req contentKiwixDownloadRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	e, err := s.deps.Catalog.Lookup(r.Context(), req.ID, req.Name, req.Flavour)
	if err != nil {
		if errors.Is(err, kiwix.ErrEntryNotFound) {
			writeError(w, http.StatusBadRequest, "That library can't be downloaded. Refresh the list and try again.")
			return
		}
		writeError(w, http.StatusBadGateway, msgContentCatalogDown)
		return
	}
	if e.Size > 0 && s.deps.MediaDir != "" {
		free, err := contentFreeSpace(s.deps.MediaDir)
		if err == nil && free < uint64(e.Size)+disk.DefaultFloor {
			writeError(w, http.StatusBadRequest, fmt.Sprintf(
				"Not enough free space on your media drive: %s needed, only about %s free. Delete some videos or move your media to a bigger drive, then try again.",
				contentSizeLabel(e.Size), contentSizeLabel(int64(free))))
			return
		}
	}
	id, err := s.deps.Kiwix.Enqueue(*e)
	if err != nil {
		if errors.Is(err, kiwix.ErrAlreadyInstalled) {
			writeError(w, http.StatusBadRequest, "This library is already on your shelf.")
			return
		}
		writeError(w, http.StatusBadRequest, "That library couldn't be added. Refresh the list and try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

func (s *Server) handleContentKiwixRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	var req contentJobRefRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	if err := s.deps.Kiwix.Remove(req.ID); err != nil {
		switch {
		case errors.Is(err, kiwix.ErrNotInstalled):
			writeError(w, http.StatusNotFound, "That library is not installed.")
		case errors.Is(err, kiwix.ErrFolderBook):
			writeError(w, http.StatusBadRequest, `This library is a file in a folder on this computer, and kids-shelf never deletes those files. To take it off the shelf, press "Stop using this folder" under "Library files on this computer".`)
		default:
			writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── library folders (library files already on this computer) ───────────────

// contentKiwixFolder is one library folder as the page lists it.
type contentKiwixFolder struct {
	Path      string `json:"path"`
	Libraries int    `json:"libraries"` // libraries on the shelf from it
	Size      int64  `json:"size"`      // their total size in bytes
	// Reachable is false while kids-shelf cannot read the folder (a drive
	// that is not plugged in, a NAS that is off); its libraries then show
	// as missing.
	Reachable bool `json:"reachable"`
}

type contentKiwixFolderRequest struct {
	Path string `json:"path"`
}

// handleContentKiwixFolders lists the library folders, each with how many
// libraries it has on the shelf, their total size and whether kids-shelf
// can reach it right now.
func (s *Server) handleContentKiwixFolders(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	folders, err := s.deps.Kiwix.Folders()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	out := make([]contentKiwixFolder, 0, len(folders))
	for _, f := range folders {
		out = append(out, contentKiwixFolder{
			Path: f.Path, Libraries: f.Books, Size: f.Size, Reachable: f.Reachable(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"folders": out})
}

// handleContentKiwixFolderAdd starts using a folder of library files where
// it is (kiwix.Manager.AddFolder): its .zim files join the shelf at once,
// and the answer says how many there are. The path is tidied like a video
// folder's; a typo, a file, kids-shelf's own library folder, a folder
// already on the list or one inside or around a folder on it is refused in
// plain words.
func (s *Server) handleContentKiwixFolderAdd(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	var req contentKiwixFolderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	path := contentCleanSourcePath(req.Path)
	if path == "" {
		writeError(w, http.StatusBadRequest, "Enter the path to the folder first.")
		return
	}
	if !filepath.IsAbs(path) {
		writeError(w, http.StatusBadRequest, "Enter the whole path to the folder, like "+exampleLibraryPath()+".")
		return
	}
	n, err := s.deps.Kiwix.AddFolder(r.Context(), path)
	switch {
	case errors.Is(err, kiwix.ErrFolderNotFound):
		writeError(w, http.StatusBadRequest, "We couldn't find that folder on this computer. Check the path. A drive or NAS has to be connected to this computer first.")
	case errors.Is(err, kiwix.ErrNotAFolder):
		writeError(w, http.StatusBadRequest, "That is a file, not a folder. Enter the folder that holds your library files (.zim).")
	case errors.Is(err, kiwix.ErrFolderNotReadable):
		writeError(w, http.StatusBadRequest, "kids-shelf can't open that folder. Check that this computer is allowed to read it.")
	case errors.Is(err, kiwix.ErrLibraryFolder):
		writeError(w, http.StatusBadRequest, "kids-shelf keeps the libraries it downloads in that folder, so they are on the shelf already. Enter another folder.")
	case errors.Is(err, kiwix.ErrFolderAdded):
		writeError(w, http.StatusBadRequest, "That folder is already on the list.")
	case errors.Is(err, kiwix.ErrFolderNested):
		writeError(w, http.StatusBadRequest, "That folder is inside a folder on the list, or holds one. kids-shelf looks in every folder inside a folder on the list, so keep just one of the two.")
	case err != nil:
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": path, "libraries": n})
	}
}

// handleContentKiwixFolderRemove stops using a library folder: its
// libraries leave the shelf, and its files stay where they are, untouched.
func (s *Server) handleContentKiwixFolderRemove(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	var req contentKiwixFolderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	if err := s.deps.Kiwix.RemoveFolder(req.Path); err != nil {
		if errors.Is(err, kiwix.ErrNoSuchFolder) {
			writeError(w, http.StatusNotFound, "That folder is not on the list.")
			return
		}
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleContentKiwixFolderRescan looks through one library folder again
// ("Look again"): new files join the shelf, files gone from it leave, and
// a folder kids-shelf cannot reach keeps its libraries. The answer says
// which of these happened.
func (s *Server) handleContentKiwixFolderRescan(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Kiwix == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentKiwixDown)
		return
	}
	var req contentKiwixFolderRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return
	}
	res, err := s.deps.Kiwix.ScanFolder(r.Context(), req.Path)
	if err != nil {
		if errors.Is(err, kiwix.ErrNoSuchFolder) {
			writeError(w, http.StatusNotFound, "That folder is not on the list.")
			return
		}
		writeError(w, http.StatusInternalServerError, "Looking through the folder couldn't finish. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "reachable": res.Reachable, "added": res.Added, "dropped": res.Dropped, "libraries": res.Books,
	})
}

// contentSizeLabel prints a byte count the way parents read it: whole GB
// from 10 GB up, one decimal of GB below that, rounded MB below a GB.
func contentSizeLabel(b int64) string {
	const gb = 1 << 30
	const mb = 1 << 20
	switch {
	case b >= 10*gb:
		return fmt.Sprintf("%d GB", (b+gb/2)/gb)
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(gb))
	case b >= mb:
		return fmt.Sprintf("%.0f MB", math.Ceil(float64(b)/float64(mb)))
	default:
		return fmt.Sprintf("%d KB", (b+512)/(1<<10))
	}
}
