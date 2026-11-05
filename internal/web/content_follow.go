// The Following part of /admin/content: the preview of a pasted playlist or
// channel link, the followed sources and the new videos that wait for the
// parent's yes. The work itself lives in internal/youtube (follow.go); the
// handlers here only check the parent PIN, read the request and put the
// result into plain words. Every route sits under /api/admin/, behind
// requireAdminPIN like the rest of the area.
package web

import (
	"errors"
	"net/http"
	"time"

	"github.com/edementhon/kids-shelf/internal/youtube"
)

const (
	msgFollowDown     = "This copy can't follow playlists and channels."
	msgFollowGone     = "That playlist or channel isn't on the list anymore. Reload the page."
	msgFollowExists   = "You follow this one already. It's in the Following list."
	msgFollowChoice   = "That choice isn't possible here. Reload the page and try again."
	msgFollowNotAList = "That link isn't a playlist or a channel."
)

func init() {
	register(func(s *Server) {
		m := s.mux
		m.HandleFunc("GET /api/admin/content/youtube/follows", s.handleFollowList)
		m.HandleFunc("POST /api/admin/content/youtube/follows/start", s.handleFollowStart)
		m.HandleFunc("POST /api/admin/content/youtube/follows/cancel", s.handleFollowCancel)
		m.HandleFunc("POST /api/admin/content/youtube/follows/settings", s.handleFollowSettings)
		m.HandleFunc("POST /api/admin/content/youtube/follows/check", s.handleFollowCheck)
		m.HandleFunc("POST /api/admin/content/youtube/follows/stop", s.handleFollowStop)
		m.HandleFunc("POST /api/admin/content/youtube/follows/add", s.handleFollowAdd)
		m.HandleFunc("POST /api/admin/content/youtube/follows/skip", s.handleFollowSkip)
	})
}

// followRequest is the body of every Following action: the source, and
// for the actions that take them, the parent's choices.
type followRequest struct {
	ID string `json:"id"`
	youtube.StartOptions
	// Video is one waiting video for add and skip; "" answers them all.
	Video string `json:"video"`
}

// followGate checks the PIN and that the service runs, and reads the
// request body. It reports whether the handler goes on.
func (s *Server) followGate(w http.ResponseWriter, r *http.Request, req *followRequest) bool {
	if !s.requireAdminPIN(w, r) {
		return false
	}
	if s.deps.Follows == nil {
		writeError(w, http.StatusServiceUnavailable, msgFollowDown)
		return false
	}
	if req == nil {
		return true
	}
	if err := readJSON(r, req); err != nil || req.ID == "" {
		writeError(w, http.StatusBadRequest, msgContentBadBody)
		return false
	}
	return true
}

// followError answers a failed action in plain words.
func followError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, youtube.ErrFollowNotFound):
		writeError(w, http.StatusNotFound, msgFollowGone)
	case errors.Is(err, youtube.ErrFollowExists):
		writeError(w, http.StatusConflict, msgFollowExists)
	case errors.Is(err, youtube.ErrFollowChoice):
		writeError(w, http.StatusBadRequest, msgFollowChoice)
	case errors.Is(err, youtube.ErrNotAList):
		writeError(w, http.StatusBadRequest, msgFollowNotAList)
	default:
		writeError(w, http.StatusInternalServerError, msgContentSaveFailed)
	}
}

// contentFollowLookup starts the lookup of a pasted playlist or channel
// link (handleContentYouTubeAdd has checked the PIN and the link). The
// page shows the row ("Looking it up") at once and its preview when the
// lookup is done.
func (s *Server) contentFollowLookup(w http.ResponseWriter, link, kind string) {
	if s.deps.Follows == nil {
		writeError(w, http.StatusServiceUnavailable, msgFollowDown)
		return
	}
	id, err := s.deps.Follows.Lookup(link)
	if err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"follow": id, "kind": kind})
}

// contentFollow is one pasted list as the page shows it, with Shelf: how
// the videos taken into Kids Shelf are really doing, from their download
// and conversion jobs (the YouTube list's join, contentYouTubeRows).
type contentFollow struct {
	youtube.FollowView
	Shelf contentFollowShelf `json:"shelf"`
}

// contentFollowShelf: OnShelf finished converting (published); OnTheWay
// waits to download, downloads, waits to convert or converts; Failed could
// not be added (failed, or paused until the parent acts).
type contentFollowShelf struct {
	OnShelf  int `json:"onShelf"`
	OnTheWay int `json:"onTheWay"`
	Failed   int `json:"failed"`
}

// handleFollowList lists the pasted lists: lookups, previews, followed
// sources with the videos that wait for a yes, and sources added once.
// A source added once leaves the list by itself when none of its videos
// is on the way any more (this is where that is known). Now is the
// server's clock, so "Checked 2 hours ago" does not depend on the phone's.
func (s *Server) handleFollowList(w http.ResponseWriter, r *http.Request) {
	if !s.followGate(w, r, nil) {
		return
	}
	list, err := s.deps.Follows.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
		return
	}
	var videos map[string]*contentYouTubeRow
	if s.deps.YouTube != nil {
		if _, videos, _, err = s.contentYouTubeRows(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
			return
		}
	}
	out := make([]contentFollow, 0, len(list))
	waiting := 0
	for _, f := range list {
		cf := contentFollow{FollowView: f}
		for _, id := range f.AddedIDs {
			v := videos[id]
			if v == nil {
				continue
			}
			var c contentVideoCounts
			c.add(v.status)
			cf.Shelf.OnShelf += c.OnShelf
			cf.Shelf.OnTheWay += c.Downloading + c.Converting + c.Waiting
			cf.Shelf.Failed += c.NeedsYou
		}
		switch f.Status {
		case "following":
			waiting += f.Counts.Waiting
		case "once":
			if cf.Shelf.OnTheWay == 0 {
				if err := s.deps.Follows.Forget(f.ID); err != nil {
					writeError(w, http.StatusInternalServerError, msgContentLoadFailed)
					return
				}
				continue
			}
		}
		out = append(out, cf)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"follows": out, "waiting": waiting, "now": time.Now().UTC(),
		"limits": youtube.LengthLimits, "newest": youtube.NewestCount,
	})
}

// handleFollowStart acts on a preview with the parent's choices.
func (s *Server) handleFollowStart(w http.ResponseWriter, r *http.Request) {
	var req followRequest
	if !s.followGate(w, r, &req) {
		return
	}
	if err := s.deps.Follows.Start(req.ID, req.StartOptions); err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFollowCancel forgets a link being looked up or in its preview.
func (s *Server) handleFollowCancel(w http.ResponseWriter, r *http.Request) {
	var req followRequest
	if !s.followGate(w, r, &req) {
		return
	}
	if err := s.deps.Follows.Cancel(req.ID); err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFollowSettings changes a followed source's mode and length limit.
func (s *Server) handleFollowSettings(w http.ResponseWriter, r *http.Request) {
	var req followRequest
	if !s.followGate(w, r, &req) {
		return
	}
	if err := s.deps.Follows.Settings(req.ID, req.Mode, req.MaxMinutes); err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFollowCheck asks for a check now (or another try of a failed
// lookup).
func (s *Server) handleFollowCheck(w http.ResponseWriter, r *http.Request) {
	var req followRequest
	if !s.followGate(w, r, &req) {
		return
	}
	if err := s.deps.Follows.CheckNow(req.ID); err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFollowStop stops following a source; its videos stay on the shelf.
func (s *Server) handleFollowStop(w http.ResponseWriter, r *http.Request) {
	var req followRequest
	if !s.followGate(w, r, &req) {
		return
	}
	if err := s.deps.Follows.Stop(req.ID); err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFollowAdd says yes to one waiting video (or all of a source's).
func (s *Server) handleFollowAdd(w http.ResponseWriter, r *http.Request) {
	s.followAnswer(w, r, true)
}

// handleFollowSkip says no to one waiting video (or all of a source's).
func (s *Server) handleFollowSkip(w http.ResponseWriter, r *http.Request) {
	s.followAnswer(w, r, false)
}

func (s *Server) followAnswer(w http.ResponseWriter, r *http.Request, add bool) {
	var req followRequest
	if !s.followGate(w, r, &req) {
		return
	}
	n, err := s.deps.Follows.Answer(req.ID, req.Video, add)
	if err != nil {
		followError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"answered": n})
}
