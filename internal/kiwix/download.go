package kiwix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
)

const (
	// streamRetries is how many times one mirror is retried after a
	// mid-transfer connection drop, each retry resuming with a Range request
	// from the last byte on disk. HTTP and request-level errors are treated
	// as "this mirror is down"; only a lost connection mid-stream gets a
	// Range retry.
	streamRetries = 3
	// progressInterval is how often downloaded-byte progress is persisted to
	// the job payload (every 4 MiB), so a restarted job and the dashboard
	// see close-to-current progress without a database write per chunk.
	progressInterval = 4 << 20
	// defaultHeaderTimeout is how long a mirror may take to respond with
	// headers before the attempt is abandoned for the next mirror.
	defaultHeaderTimeout = 30 * time.Second
	// defaultIdleTimeout is how long a mirror may stream no bytes before the
	// attempt is abandoned for the next mirror.
	defaultIdleTimeout = 60 * time.Second
)

// errDiskFull marks a local write failure caused by a full disk, as opposed
// to a mirror or network failure. download() turns it into a paused job: the
// reservation and the written prefix are kept, and the parent is told to
// free space.
var errDiskFull = errors.New("kiwix: disk full")

// fetchMeta4 downloads and parses the entry's .meta4 Metalink file.
func (m *Manager) fetchMeta4(ctx context.Context, meta4URL string) (*Meta4, error) {
	if err := checkURLScheme(meta4URL); err != nil {
		return nil, fmt.Errorf("fetch %s: %w", meta4URL, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, meta4URL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", meta4URL, err)
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", meta4URL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: HTTP %s", meta4URL, resp.Status)
	}
	// A metalink is a few kilobytes; cap the read so a broken mirror cannot
	// make the job swallow an unbounded response, and apply the idle
	// deadline so a mirror that stalls mid-body is abandoned instead of
	// hanging the job.
	body := io.Reader(resp.Body)
	if m.idleTimeout > 0 {
		body = &idleReadCloser{body: resp.Body, idle: m.idleTimeout}
	}
	meta, err := parseMeta4(io.LimitReader(body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", meta4URL, err)
	}
	return meta, nil
}

// download fills partial with the entry's ZIM, resuming from whatever the
// file already holds. It walks the metalink mirrors in priority order, moves
// on when a mirror fails, and retries a dropped mid-transfer connection on
// the same mirror with a Range request from the last persisted byte. The
// partial file is the only resume source: its size determines the next Range
// offset, and every progress update is written to the job payload.
func (m *Manager) download(ctx context.Context, job *jobs.Job, partial string, p *jobPayload) error {
	total := p.Meta4.Size
	cur := partialSize(partial)
	if cur > total {
		// A partial longer than the declared size cannot be right: drop it
		// and start over.
		if err := os.Remove(partial); err != nil {
			return fmt.Errorf("kiwix: reset %s: %w", partial, err)
		}
		cur = 0
	}
	p.Downloaded = cur
	if err := m.queue.Advance(job.ID, stageDownload, p); err != nil {
		return err
	}
	if cur == total {
		return nil // already fully downloaded; verification happens next
	}

	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("kiwix: open %s: %w", partial, err)
	}
	defer f.Close()

	var lastErr error
	pending := int64(0)
	for _, mirror := range p.Meta4.Mirrors {
		for range streamRetries {
			size, wrote, retry, err := m.fetchRange(ctx, f, mirror.URL, cur, total)
			cur = size
			p.Downloaded = size
			pending += wrote
			if pending >= progressInterval {
				pending = 0
				if err := m.queue.Advance(job.ID, stageDownload, p); err != nil {
					return err
				}
			}
			if err == nil {
				// Make the file durable before verification, so a power cut
				// right after the download cannot strip it back.
				if serr := f.Sync(); serr != nil {
					if disk.IsDiskFull(serr) {
						if perr := m.pauseDiskFull(job, p, serr); perr != nil {
							return perr
						}
						return fmt.Errorf("%w: %v", errDiskFull, serr)
					}
					return fmt.Errorf("kiwix: download %s: sync: %w", p.Meta4.File, serr)
				}
				return m.queue.Advance(job.ID, stageDownload, p)
			}
			lastErr = err
			if errors.Is(err, errDiskFull) {
				// The local disk filled mid-write: the partial holds exactly
				// the durable bytes, the reservation stays, and the job is
				// paused with a message the parent can act on. The error
				// still propagates: a pause is not a finished run.
				if perr := m.pauseDiskFull(job, p, err); perr != nil {
					return perr
				}
				return err
			}
			if ctx.Err() != nil {
				// Quit or kill: everything already written stays in the
				// partial file for the next run.
				return fmt.Errorf("kiwix: download %s: %w", p.Meta4.File, ctx.Err())
			}
			if !retry {
				break // this mirror is down; try the next one
			}
		}
	}
	return fmt.Errorf("kiwix: download %s: all mirrors failed: %w", p.Meta4.File, lastErr)
}

// pauseDiskFull advances the job's payload with the durable byte count and
// pauses it with a parent-readable disk-full explanation. The reservation is
// deliberately kept: the job still owns its space.
func (m *Manager) pauseDiskFull(job *jobs.Job, p *jobPayload, cause error) error {
	if err := m.queue.Advance(job.ID, stageDownload, p); err != nil {
		return err
	}
	msg := fmt.Sprintf(
		"Disk is full while downloading %s: %v. Free up space and the download resumes from where it stopped.",
		p.Meta4.File, cause)
	return m.queue.Pause(job.ID, msg)
}

// fetchRange downloads one contiguous slice of the file starting at start of
// a file declared to be total bytes long, appending it to f. It returns the
// file's new size, the bytes newly written, whether a retry (stream dropped
// mid-transfer) may help, and the error, if any. An error with retry set
// means some bytes were written and a Range request from the new offset can
// continue; retry is false for errors that will not improve on the same
// mirror (HTTP status, connection refused, truncated body, stall, disk full).
func (m *Manager) fetchRange(ctx context.Context, f *os.File, url string, start, total int64) (size int64, written int64, retry bool, err error) {
	if start >= total {
		return start, 0, false, nil
	}
	if err := checkURLScheme(url); err != nil {
		return start, 0, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return start, 0, false, err
	}
	if start > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", start))
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return start, 0, false, fmt.Errorf("kiwix: mirror %s: %w", url, err)
	}
	defer resp.Body.Close()

	// offset is where the response body belongs in the file: start for a
	// validated 206, 0 when a 200 restarts the file from scratch.
	offset := start
	remaining := total - start
	switch resp.StatusCode {
	case http.StatusPartialContent:
		// The mirror must serve exactly the slice we asked for; a range that
		// does not line up would corrupt the partial file.
		rs, re, rt, perr := parseContentRange(resp.Header.Get("Content-Range"))
		if perr != nil {
			return start, 0, false, fmt.Errorf("kiwix: mirror %s: %v", url, perr)
		}
		if rs != start {
			return start, 0, false, fmt.Errorf(
				"kiwix: mirror %s: Content-Range starts at %d, wanted %d", url, rs, start)
		}
		if rt != total {
			return start, 0, false, fmt.Errorf(
				"kiwix: mirror %s: Content-Range total %d, wanted %d", url, rt, total)
		}
		if re < rs || re >= total {
			return start, 0, false, fmt.Errorf(
				"kiwix: mirror %s: Content-Range end %d out of bounds", url, re)
		}
		// A 206 may legitimately serve less than the whole remainder of the
		// file: the body ends at its Content-Range end, and nothing beyond
		// it belongs in the partial.
		remaining = re - rs + 1
	case http.StatusOK:
		if start > 0 {
			// The mirror ignored the Range header and sent the whole file:
			// the partial's prefix no longer lines up, so restart from byte
			// 0 instead of appending onto it.
			if err := f.Truncate(0); err != nil {
				return start, 0, false, fmt.Errorf("kiwix: mirror %s: restart: %w", url, err)
			}
			offset = 0
			remaining = total
		}
	case http.StatusRequestedRangeNotSatisfiable:
		return start, 0, false, fmt.Errorf("kiwix: mirror %s: HTTP 416 at offset %d", url, start)
	default:
		return start, 0, false, fmt.Errorf("kiwix: mirror %s: HTTP %s", url, resp.Status)
	}

	dst := m.partWriterFor(f)
	// A body declared longer than the range end means the mirror will send
	// bytes that do not belong in the file; reject it before reading.
	if resp.ContentLength > remaining {
		return offset, 0, false, fmt.Errorf(
			"kiwix: mirror %s: body declared %d bytes, range needs %d", url, resp.ContentLength, remaining)
	}
	body := io.Reader(resp.Body)
	if m.idleTimeout > 0 {
		body = &idleReadCloser{body: resp.Body, idle: m.idleTimeout}
	}
	buf := make([]byte, 256<<10)
	for written < remaining {
		n, rerr := body.Read(buf)
		if n > 0 {
			// Never write past the declared range end: a read carrying more
			// than the remaining bytes is the mirror padding its copy, so
			// keep the in-range prefix and reject the mirror.
			keep := int64(n)
			if keep > remaining-written {
				keep = remaining - written
			}
			if _, werr := dst.Write(buf[:keep]); werr != nil {
				if disk.IsDiskFull(werr) {
					// Local disk filled mid-write: keep exactly the durable
					// bytes (the failed write may have extended the file
					// partway) and report a disk-full so the caller pauses.
					f.Truncate(offset + written)
					f.Sync()
					return offset + written, written, false, fmt.Errorf("%w: %v", errDiskFull, werr)
				}
				return offset + written, written, false, fmt.Errorf("kiwix: mirror %s: write: %w", url, werr)
			}
			written += keep
			if keep != int64(n) {
				return offset + written, written, false, fmt.Errorf(
					"kiwix: mirror %s: body longer than the requested range (%d bytes for %d remaining)", url, n, remaining)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			if isStall(rerr) {
				// No bytes for the idle deadline: the mirror stalled, so the
				// attempt ends and the next mirror takes over.
				return offset + written, written, false, fmt.Errorf(
					"kiwix: mirror %s: no data for %s", url, m.idleTimeout)
			}
			// Connection dropped mid-transfer: keep the bytes written so far
			// and let the caller retry with a Range request.
			return offset + written, written, true, fmt.Errorf("kiwix: mirror %s: %w", url, rerr)
		}
	}
	// The declared range is full; peek one more read so a chunked mirror
	// that keeps streaming past it is caught instead of silently accepted.
	// A stall here still means the file holds exactly the right bytes, so
	// it is not a failure: the unread remainder never touches the file.
	if written == remaining {
		n, _ := body.Read(buf)
		if n > 0 {
			return offset + written, written, false, fmt.Errorf(
				"kiwix: mirror %s: body longer than the requested range (%d extra bytes)", url, n)
		}
	}
	if written < remaining {
		// Clean end of body before the declared size: the mirror's copy is
		// truncated and another mirror must complete it.
		return offset + written, written, false, fmt.Errorf(
			"kiwix: mirror %s: short body: got %d of %d bytes", url, written, remaining)
	}
	if offset+written < total {
		// The response ended exactly at its declared range but the file is
		// still short (a 206 may serve less than the whole remainder): this
		// is a resumable interruption, never a completed download.
		return offset + written, written, false, fmt.Errorf(
			"kiwix: mirror %s: short body: got %d of %d bytes", url, written, total)
	}
	return offset + written, written, false, nil
}

// parseContentRange parses a 206 "Content-Range: bytes start-end/total"
// header into its three numbers.
func parseContentRange(v string) (start, end, total int64, err error) {
	if _, err := fmt.Sscanf(v, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return 0, 0, 0, fmt.Errorf("kiwix: unparsable Content-Range %q", v)
	}
	return start, end, total, nil
}

// errStall marks a response body read that produced no bytes for the idle
// deadline, as opposed to a dropped connection: a stalled mirror is
// abandoned, not retried.
var errStall = errors.New("kiwix: mirror stall")

// idleReadCloser wraps an HTTP response body so a Read that stays blocked
// without producing data for idle is interrupted - the body's Close is what
// unblocks a pending read - and surfaces as errStall.
type idleReadCloser struct {
	body io.ReadCloser
	idle time.Duration
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	timer := time.AfterFunc(r.idle, func() { r.body.Close() })
	defer timer.Stop()
	n, err := r.body.Read(p)
	if n > 0 && err == nil {
		return n, nil // data arrived: not a stall even if the timer raced
	}
	if !timer.Stop() {
		return n, errStall
	}
	return n, err
}

// isStall reports whether a body read error is an idle-read deadline firing,
// as opposed to a genuine connection drop.
func isStall(err error) bool {
	if errors.Is(err, errStall) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// partWriterFor returns the writer the partial file's data goes through:
// the file itself, or the test-injected stand-in.
func (m *Manager) partWriterFor(f *os.File) io.Writer {
	if m.partWriter == nil {
		return f
	}
	return m.partWriter(f)
}

// client returns the Manager's HTTP client, built once from the current
// timeout fields so tests can lower them before the first request. It
// enforces the outbound destination policy at dial time, so catalog,
// meta4 and mirror traffic can never reach a loopback, private, link-local,
// CGNAT, multicast or unspecified address - redirects and DNS tricks
// included - and follows at most maxRedirects redirects.
func (m *Manager) client() *http.Client {
	m.httpOnce.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = m.headerTimeout
		tr.DialContext = restrictedDialer(m.allowLoopback).DialContext
		m.httpClient = &http.Client{Transport: tr, CheckRedirect: checkRedirect}
	})
	return m.httpClient
}

// verify re-reads path and compares its sha-256 to want (hex). It checks the
// context along the way so a quit during verification stops without wasting
// the rename that follows.
func (m *Manager) verify(ctx context.Context, path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("kiwix: verify: open %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 256<<10)
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("kiwix: verify %s: %w", path, rerr)
		}
		if ctx.Err() != nil {
			return fmt.Errorf("kiwix: verify %s: %w", path, ctx.Err())
		}
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !equalFoldHex(got, want) {
		return fmt.Errorf("sha-256 mismatch: got %s, want %s", got, want)
	}
	return nil
}

// equalFoldHex compares two hex strings case-insensitively.
func equalFoldHex(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if lowerHex(a[i]) != lowerHex(b[i]) {
			return false
		}
	}
	return true
}

func lowerHex(c byte) byte {
	if c >= 'A' && c <= 'F' {
		return c + ('a' - 'A')
	}
	return c
}

// partialSize is the current size of the partial file, 0 when absent.
func partialSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return st.Size()
}
