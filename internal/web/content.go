// The /admin/content area of the parent dashboard: add videos from YouTube,
// register folder and NAS sources, watch conversions (ingest) and manage the
// Kiwix libraries (catalog, downloads, installed ZIMs).
//
// Dashboard PIN: the /admin area owns the gate (admin.go: requireAdminPIN,
// the ks_admin_pin cookie and <DataDir>/dashboard-pin.json). Every data
// route here calls that gate first, so the optional dashboard PIN set in
// Settings gates /admin/content exactly like /admin; the page shell is
// served to everyone and the page asks for the PIN when its first data call
// comes back 401. This file therefore builds only together with admin.go.
package web

import (
	"embed"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/ingest"
	"github.com/edementhon/kids-shelf/internal/kiwix"
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
	msgContentBadLink     = "That doesn't look like a YouTube link."
	msgContentBadBody     = "That request couldn't be read."
	msgContentCatalogDown = "The library catalog couldn't be reached. Check that this computer is online, then try again."
)

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
		m.HandleFunc("GET /api/admin/content/ingest", s.handleContentIngestList)
		m.HandleFunc("GET /api/admin/content/kiwix/catalog", s.handleContentKiwixCatalog)
		m.HandleFunc("GET /api/admin/content/kiwix/categories", s.handleContentKiwixCategories)
		m.HandleFunc("GET /api/admin/content/kiwix/languages", s.handleContentKiwixLanguages)
		m.HandleFunc("GET /api/admin/content/kiwix", s.handleContentKiwixState)
		m.HandleFunc("POST /api/admin/content/kiwix/download", s.handleContentKiwixDownload)
		m.HandleFunc("POST /api/admin/content/kiwix/remove", s.handleContentKiwixRemove)
	})
}

// ─── page and static files ─────────────────────────────────────────────────

// handleContentPage serves the page shell to everyone; with the optional
// dashboard PIN set, the page shows the PIN form when its first data call
// comes back 401 (all data is behind requireAdminPIN, never the shell).
func (s *Server) handleContentPage(w http.ResponseWriter, r *http.Request) {
	b, err := contentFiles.ReadFile("static/content/index.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "This page is missing from this copy.")
		return
	}
	page := strings.Replace(string(b), "__KS_OS_PATH__", exampleMediaPath(), 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, page)
}

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

// handleContentStatic serves the area's embedded files (content.js,
// content.css) with no directory listing.
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

func (s *Server) handleContentYouTubeList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.YouTube == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentYouTubeDown)
		return
	}
	jobs, err := s.deps.YouTube.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
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
}

type contentSourcesAddRequest struct {
	Path string `json:"path"`
	Kind string `json:"kind"` // "folder" or "nas" (core.SourceFolder / core.SourceNAS)
}

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
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	out := make([]contentSource, 0, len(srcs))
	for _, src := range srcs {
		out = append(out, contentSource{ID: src.ID, Path: src.Path, Kind: string(src.Kind), Allowlist: src.Allowlist})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": out})
}

// handleContentSourcesAdd registers a media root. The path must exist now:
// a parent who typos a folder gets told at once, and a NAS share has to be
// connected before it can be scanned anyway (the periodic scan tolerates a
// root that vanishes later).
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
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" {
		writeError(w, http.StatusBadRequest, "Enter the path to the folder first.")
		return
	}
	if req.Kind != string(core.SourceFolder) && req.Kind != string(core.SourceNAS) {
		writeError(w, http.StatusBadRequest, "Choose 'A folder on this computer' or 'A network drive (NAS)'.")
		return
	}
	if _, err := os.Stat(req.Path); err != nil {
		writeError(w, http.StatusBadRequest, "We couldn't find that folder on this computer. Check the path. A NAS share has to be connected to this machine first.")
		return
	}
	if err := s.deps.Library.AddSource(r.Context(), req.Path, core.SourceKind(req.Kind), nil); err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

type contentSourcesRemoveRequest struct {
	Path string `json:"path"`
}

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

// handleContentScan walks every registered source and hands new files to the
// ingest queue.
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
	if err := s.deps.Library.Scan(r.Context(), s.deps.Ingest); err != nil {
		writeError(w, http.StatusInternalServerError, "Scanning couldn't finish. Try again.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── conversions (ingest jobs) ──────────────────────────────────────────────

// contentIngestJob is one conversions-list row: the ingest job as the queue
// reports it plus the name the page should lead with. Internal paths and
// URLs stay in File (rendered small underneath); Title is the episode or
// show title the enqueueing producer recorded, or the path when unknown.
type contentIngestJob struct {
	ingest.JobView
	Title string `json:"title"`
}

func (s *Server) handleContentIngestList(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminPIN(w, r) {
		return
	}
	if s.deps.Ingest == nil {
		writeError(w, http.StatusServiceUnavailable, msgContentIngestDown)
		return
	}
	jobs, err := s.deps.Ingest.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	out := make([]contentIngestJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, contentIngestJob{JobView: j, Title: contentIngestTitle(j)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
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
	Language    string    `json:"language"`
	File        string    `json:"file"`
	Size        int64     `json:"size"`
	InstalledAt time.Time `json:"installed_at"`
}

type contentKiwixState struct {
	Installed []contentZIMInstalled `json:"installed"`
	Downloads []kiwix.JobView       `json:"downloads"`
	FreeBytes int64                 `json:"free_bytes"` // -1 when unknown
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
	entries := make([]contentZIM, 0, len(res.Entries))
	for _, e := range res.Entries {
		entries = append(entries, contentZIMFromEntry(e))
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
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	jobs, err := s.deps.Kiwix.Jobs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	out := contentKiwixState{Downloads: jobs, FreeBytes: -1, Installed: []contentZIMInstalled{}}
	for _, in := range installed {
		out.Installed = append(out.Installed, contentZIMInstalled{
			ID: in.ID, Name: in.Name, Title: in.Title, Language: in.Language,
			File: in.File, Size: in.Size, InstalledAt: in.InstalledAt,
		})
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
		if errors.Is(err, kiwix.ErrNotInstalled) {
			writeError(w, http.StatusNotFound, "That library is not installed.")
			return
		}
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
