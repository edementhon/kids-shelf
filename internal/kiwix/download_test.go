package kiwix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

const (
	testEntryID = "urn:uuid:test-kids-shelf-zim"
	testZimName = "test_zim"
	// fileZIM is the name the ZIM is stored under, generated from the
	// catalog entry: name_flavour_updated-date.zim. The .meta4 files the
	// mirrors serve declare "test.zim" - deliberately different, so the
	// tests prove the stored name never comes from the metalink.
	fileZIM   = "test_zim_maxi_2026-08.zim"
	zimPath   = "/test.zim"
	meta4Path = "/test.zim.meta4"
)

func testEntry(meta4URL string) Entry {
	return Entry{
		ID:       testEntryID,
		Name:     testZimName,
		Flavour:  "maxi",
		Updated:  time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC),
		Title:    "Test ZIM",
		Language: "eng",
		Meta4URL: meta4URL,
	}
}

// mirror is a Range-capable HTTP server for one small ZIM plus its .meta4,
// with optional failure injection.
type mirror struct {
	// Configuration; set before any request is served.
	data              []byte
	meta4             string
	fail500           bool          // ZIM requests respond 500
	redirectTo        string        // ZIM requests 302 to this URL ("" = off)
	cutFirst          bool          // the first ZIM request closes mid-body
	cutBytes          int           // bytes written before a cut or hold
	holdFirst         bool          // the first ZIM request holds after cutBytes until the client goes away
	holdHeaders       bool          // ZIM requests never send headers until the client goes away
	ignoreRange       bool          // ZIM requests always answer 200 with the whole file
	wrongRangeStart   int           // a 206's Content-Range start is shifted by this many bytes
	started           chan struct{} // closed after a holding request writes its prefix
	servedMeta4ByPath string        // path the meta4 is served on (default meta4Path)
	extraBytes        int           // ZIM 200 responses declare and send this many extra trailing bytes
	stallMeta4        bool          // the .meta4 response sends headers then holds the body open
	httpSrv           *httptest.Server

	mu      sync.Mutex
	reqs    []string // "METHOD path range" per request
	zstatus []int    // HTTP status of each ZIM response, in request order
}

func newMirror(t *testing.T) *mirror {
	t.Helper()
	m := &mirror{started: make(chan struct{}, 1), servedMeta4ByPath: meta4Path}
	m.httpSrv = httptest.NewServer(http.HandlerFunc(m.handler))
	t.Cleanup(m.httpSrv.Close)
	return m
}

func (m *mirror) zimURL() string   { return m.httpSrv.URL + zimPath }
func (m *mirror) meta4URL() string { return m.httpSrv.URL + m.servedMeta4ByPath }

func (m *mirror) record(r *http.Request) {
	m.mu.Lock()
	m.reqs = append(m.reqs, r.Method+" "+r.URL.Path+" "+r.Header.Get("Range"))
	m.mu.Unlock()
}

func (m *mirror) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.reqs...)
}

// zimRequests returns the recorded requests for the ZIM file itself,
// excluding .meta4 fetches.
func (m *mirror) zimRequests() []string {
	var out []string
	for _, req := range m.requests() {
		if strings.Contains(req, " "+zimPath+" ") {
			out = append(out, req)
		}
	}
	return out
}

// rangeStarts returns the parsed byte offset of every ZIM request's Range
// header (0 when the request had none).
func (m *mirror) rangeStarts() []int {
	var out []int
	for _, req := range m.zimRequests() {
		fields := strings.Fields(req)
		rng := ""
		if len(fields) == 3 {
			rng = fields[2]
		}
		out = append(out, parseRangeStart(rng))
	}
	return out
}

func parseRangeStart(rng string) int {
	if !strings.HasPrefix(rng, "bytes=") || !strings.HasSuffix(rng, "-") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rng, "bytes="), "-"))
	if err != nil {
		return 0
	}
	return n
}

func (m *mirror) handler(w http.ResponseWriter, r *http.Request) {
	m.record(r)
	noteStatus := func(code int) {
		m.mu.Lock()
		m.zstatus = append(m.zstatus, code)
		m.mu.Unlock()
	}
	switch {
	case r.URL.Path == m.servedMeta4ByPath:
		w.Header().Set("Content-Type", "application/metalink4+xml")
		if m.stallMeta4 {
			// Headers out, then the body stalls: fetchMeta4's idle deadline
			// must abandon the attempt instead of hanging the job.
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Write([]byte(m.meta4))
		return
	case r.URL.Path != zimPath:
		http.NotFound(w, r)
		return
	}
	if m.redirectTo != "" {
		http.Redirect(w, r, m.redirectTo, http.StatusFound)
		return
	}
	if m.fail500 {
		noteStatus(http.StatusInternalServerError)
		http.Error(w, "test mirror is down", http.StatusInternalServerError)
		return
	}
	if m.holdHeaders {
		<-r.Context().Done() // never send headers; the client times out
		return
	}

	start := parseRangeStart(r.Header.Get("Range"))
	total := len(m.data)

	// cut and hold apply to the first ZIM request; the meta4 fetch does not
	// count, so count the ZIM requests recorded so far (which includes this
	// one).
	m.mu.Lock()
	zno := 0
	for _, req := range m.reqs {
		if strings.Contains(req, " "+zimPath+" ") {
			zno++
		}
	}
	m.mu.Unlock()
	cut := m.cutFirst && zno == 1
	hold := m.holdFirst && zno == 1
	if cut || hold {
		if start > 0 && start < total {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
			w.Header().Set("Content-Length", strconv.Itoa(total-start))
			w.WriteHeader(http.StatusPartialContent)
			noteStatus(http.StatusPartialContent)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(total))
			w.WriteHeader(http.StatusOK)
			noteStatus(http.StatusOK)
		}
		n := m.cutBytes
		if start+n > total {
			n = total - start
		}
		w.Write(m.data[start : start+n])
		w.(http.Flusher).Flush()
		if hold {
			close(m.started)
			<-r.Context().Done()
		}
		return
	}
	if m.ignoreRange {
		// Pretend the server does not understand Range at all: every ZIM
		// request gets the whole file with 200.
		w.Header().Set("Content-Length", strconv.Itoa(total))
		w.WriteHeader(http.StatusOK)
		noteStatus(http.StatusOK)
		w.Write(m.data)
		return
	}
	if m.wrongRangeStart != 0 && start > 0 {
		// Answer 206 but with a Content-Range that does not match the
		// requested offset.
		rs := start + m.wrongRangeStart
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", rs, total-1, total))
		w.Header().Set("Content-Length", strconv.Itoa(total-rs))
		w.WriteHeader(http.StatusPartialContent)
		noteStatus(http.StatusPartialContent)
		w.Write(m.data[rs:])
		return
	}

	if start >= total {
		noteStatus(http.StatusRequestedRangeNotSatisfiable)
		http.Error(w, "range beyond file", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	if start > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, total-1, total))
		w.Header().Set("Content-Length", strconv.Itoa(total-start))
		w.WriteHeader(http.StatusPartialContent)
		noteStatus(http.StatusPartialContent)
		w.Write(m.data[start:])
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(total+m.extraBytes))
	w.WriteHeader(http.StatusOK)
	noteStatus(http.StatusOK)
	w.Write(m.data)
	if m.extraBytes > 0 {
		w.Write(make([]byte, m.extraBytes))
	}
}

// zstatuses returns the recorded HTTP status of each ZIM response.
func (m *mirror) zstatuses() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]int(nil), m.zstatus...)
}

// buildMeta4 renders a metalink document for data with the given mirrors in
// order; priorities ascend with position.
func buildMeta4(t *testing.T, name string, data []byte, mirrors []string) string {
	t.Helper()
	sum := sha256.Sum256(data)
	var b strings.Builder
	fmt.Fprintf(&b, `<?xml version="1.0" encoding="UTF-8"?>
<metalink xmlns="urn:ietf:params:xml:ns:metalink">
  <file name="%s">
    <size>%d</size>
    <hash type="sha-256">%s</hash>
`, name, len(data), hex.EncodeToString(sum[:]))
	for i, u := range mirrors {
		fmt.Fprintf(&b, "    <url location=\"us\" priority=\"%d\">%s</url>\n", i+1, u)
	}
	b.WriteString("  </file>\n</metalink>\n")
	return b.String()
}

// makeTestZIM returns deterministic pseudo-random bytes of the given size.
func makeTestZIM(size int) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i*7 + i>>8)
	}
	return data
}

// newStoreQueue opens a store on a temp dir, applies the jobs migrations and
// returns it plus a fresh library directory.
func newStoreQueue(t *testing.T) (*store.Store, *jobs.Queue, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "kiwix.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	q, err := jobs.New(s)
	if err != nil {
		t.Fatalf("jobs.New: %v", err)
	}
	return s, q, filepath.Join(t.TempDir(), "library")
}

func newTestManager(t *testing.T) (*Manager, *jobs.Queue, string) {
	t.Helper()
	s, q, lib := newStoreQueue(t)
	m, err := NewManager(s, q, disk.NewReservations(q), lib)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	// The tests' mirrors are httptest servers on 127.0.0.1; the outbound
	// destination policy refuses loopback unless opted in explicitly.
	m.allowLoopback = true
	return m, q, lib
}

func TestInstallCompletes(t *testing.T) {
	data := makeTestZIM(300 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, q, lib := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if job.ReservedBytes != 0 {
		t.Errorf("reservation still held: %d bytes", job.ReservedBytes)
	}

	got, err := os.ReadFile(filepath.Join(lib, fileZIM))
	if err != nil {
		t.Fatalf("read installed ZIM: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("installed ZIM content differs from the source")
	}
	if _, err := os.Stat(filepath.Join(lib, ".partial", fileZIM+".part")); !os.IsNotExist(err) {
		t.Error("partial file was not cleaned up after publishing")
	}

	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 1 {
		t.Fatalf("installed rows = %d, want 1", len(inst))
	}
	in := inst[0]
	if in.ID != testEntryID || in.Name != testZimName || in.Title != "Test ZIM" ||
		in.Language != "eng" || in.File != fileZIM || in.Size != int64(len(data)) {
		t.Errorf("installed row = %+v", in)
	}

	// Publishing once must be the end of it: a second enqueue is refused.
	if _, err := m.Enqueue(testEntry(mir.meta4URL())); !errors.Is(err, ErrAlreadyInstalled) {
		t.Errorf("second Enqueue error = %v, want ErrAlreadyInstalled", err)
	}
}

func TestInterruptedTransferResumesWithRange(t *testing.T) {
	data := makeTestZIM(300 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})
	mir.cutFirst = true
	mir.cutBytes = 60 << 10

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Errorf("installed ZIM wrong: err=%v", err)
	}

	starts := mir.rangeStarts()
	// Request 1 starts at 0 and is cut; request 2 must be a Range resume from
	// exactly where the cut happened.
	if len(starts) < 2 || starts[0] != 0 || starts[1] != 60<<10 {
		t.Errorf("Range starts = %v, want [0 61440]", starts)
	}
	if len(mir.zimRequests()) != 2 {
		t.Errorf("ZIM requests = %d, want 2", len(mir.zimRequests()))
	}
}

func TestMirrorFallbackOn500(t *testing.T) {
	data := makeTestZIM(200 << 10)
	bad := newMirror(t)
	bad.data = data
	bad.fail500 = true
	good := newMirror(t)
	good.data = data
	doc := buildMeta4(t, fileZIM, data, []string{bad.zimURL(), good.zimURL()})
	bad.meta4 = doc
	good.meta4 = doc

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(bad.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if n := len(bad.zimRequests()); n != 1 {
		t.Errorf("bad mirror served %d ZIM requests, want 1", n)
	}
	if n := len(good.zimRequests()); n != 1 {
		t.Errorf("good mirror served %d ZIM requests, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(m.lib, fileZIM)); err != nil {
		t.Errorf("installed ZIM missing after fallback: %v", err)
	}
}

func TestBadHashFailsWithoutInstalling(t *testing.T) {
	data := makeTestZIM(200 << 10)
	mir := newMirror(t)
	mir.data = data
	// Declare a sha-256 that cannot match the served bytes.
	other := strings.Repeat("0", 64)
	sum := sha256.Sum256(data)
	mir.meta4 = strings.Replace(buildMeta4(t, fileZIM, data, []string{mir.zimURL()}), hex.EncodeToString(sum[:]), other, 1)

	m, q, lib := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed", job.State)
	}
	if !strings.Contains(job.Error, "sha-256") {
		t.Errorf("job error %q does not mention the checksum", job.Error)
	}
	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 0 {
		t.Errorf("installed rows = %d, want 0", len(inst))
	}
	if _, err := os.Stat(filepath.Join(lib, fileZIM)); !os.IsNotExist(err) {
		t.Error("bad ZIM was published anyway")
	}
	if entries, _ := os.ReadDir(filepath.Join(lib, ".partial")); len(entries) != 0 {
		t.Error("bad partial file was not removed")
	}
}

func TestNoSpaceFailsBeforeDownload(t *testing.T) {
	data := makeTestZIM(200 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	s, q, lib := newStoreQueue(t)
	res := disk.NewReservations(q).WithFree(func(string) (uint64, error) { return 0, nil }).WithFloor(0)
	m, err := NewManager(s, q, res, lib)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	m.allowLoopback = true
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed", job.State)
	}
	if !strings.Contains(job.Error, "no space") {
		t.Errorf("job error %q does not read as a space problem", job.Error)
	}
	if n := len(mir.zimRequests()); n != 0 {
		t.Errorf("mirror served %d ZIM requests, want 0 (no download without a reservation)", n)
	}
	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 0 {
		t.Errorf("installed rows = %d, want 0", len(inst))
	}
}

func TestKillMidDownloadResumesAfterRecover(t *testing.T) {
	data := makeTestZIM(300 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})
	mir.holdFirst = true
	mir.cutBytes = 64 << 10

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First launch: the job is claimed and killed (context canceled) while
	// the transfer is live; it stays running in the queue, exactly like a
	// process killed mid-download.
	ctx, cancel := context.WithCancel(context.Background())
	job, err := q.Claim(Kind)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	// Run drives the download; launch it concurrently so the test can watch
	// the transfer reach the mirror and kill it mid-stream. The mirror keeps
	// the connection open forever, so waiting out the prefix read before the
	// cancel guarantees the partial holds those bytes and the resume uses a
	// Range request.
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(ctx, job) }()
	<-mir.started // the first request has written its prefix
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-runErr; err == nil {
		t.Fatal("Run with a canceled context succeeded, want an error")
	}
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}

	// Second launch: Recover requeued the job; it resumes from the partial
	// file with a Range request and finishes.
	job2, err := q.Claim(Kind)
	if err != nil {
		t.Fatalf("Claim after Recover: %v", err)
	}
	if err := m.Run(context.Background(), job2); err != nil {
		t.Fatalf("resumed Run: %v", err)
	}

	done, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if done.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", done.State, done.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	starts := mir.rangeStarts()
	if len(starts) < 2 || starts[0] != 0 {
		t.Fatalf("Range starts = %v, want a full first request and a resumed one", starts)
	}
	if starts[len(starts)-1] <= 0 {
		t.Errorf("resumed request did not use a Range offset (starts = %v)", starts)
	}
}

func TestRemoveDeletesFileAndRow(t *testing.T) {
	data := makeTestZIM(100 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, _, lib := newTestManager(t)
	if _, err := m.Enqueue(testEntry(mir.meta4URL())); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if err := m.Remove(testEntryID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lib, fileZIM)); !os.IsNotExist(err) {
		t.Error("ZIM file still on disk after Remove")
	}
	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 0 {
		t.Errorf("installed rows = %d, want 0", len(inst))
	}
	if err := m.Remove(testEntryID); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("second Remove error = %v, want ErrNotInstalled", err)
	}
}

// enospcWriter mirrors a disk that fills mid-download: it accepts bytes up to
// limit, then every further write fails with ENOSPC without writing anything,
// so the partial file holds exactly the accepted prefix.
type enospcWriter struct {
	f       *os.File
	limit   int64
	written int64
}

func (w *enospcWriter) Write(p []byte) (int, error) {
	if w.written+int64(len(p)) > w.limit {
		return 0, syscall.ENOSPC
	}
	n, err := w.f.Write(p)
	w.written += int64(n)
	return n, err
}

func TestQuitMidDownloadLeavesJobRunningAndRecovers(t *testing.T) {
	data := makeTestZIM(300 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})
	mir.holdFirst = true
	mir.cutBytes = 64 << 10

	m, q, lib := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// First launch through Work: quit (context canceled) while the transfer
	// is live. Work must not fail the job: it stays running with its
	// reservation and its partial file, exactly like a process killed
	// mid-download on disk.
	ctx, cancel := context.WithCancel(context.Background())
	workErr := make(chan error, 1)
	go func() { workErr <- m.Work(ctx) }()
	select {
	case <-mir.started:
	case <-time.After(30 * time.Second):
		t.Fatal("download never reached the mirror")
	}
	// The mirror holds the connection open forever, so wait out the prefix
	// read: the partial then provably holds those bytes and the resumed run
	// continues with a Range request.
	time.Sleep(100 * time.Millisecond)
	cancel()
	if err := <-workErr; err == nil {
		t.Fatal("Work with a canceled context succeeded, want an error")
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateRunning {
		t.Fatalf("job state = %s, want running (a quit must never fail the job)", job.State)
	}
	if job.ReservedBytes == 0 {
		t.Error("reservation was released on quit")
	}
	partial := filepath.Join(lib, ".partial", fileZIM+".part")
	if st, err := os.Stat(partial); err != nil || st.Size() == 0 {
		t.Errorf("partial file was not kept after quit: %v", err)
	}

	// Second launch: Recover requeues the running job; Work resumes from the
	// partial and finishes.
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("resumed Work: %v", err)
	}
	job, err = q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 1 {
		t.Errorf("installed rows = %d, want exactly 1", len(inst))
	}
	starts := mir.rangeStarts()
	if len(starts) < 2 || starts[0] != 0 || starts[len(starts)-1] <= 0 {
		t.Errorf("Range starts = %v, want a full first request and a resumed one", starts)
	}
}

func TestQuitDuringVerifyKeepsCompletedDownload(t *testing.T) {
	// Big enough that verification takes a moment, so the quit lands while
	// the hash is still running.
	data := makeTestZIM(256 << 20)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, q, lib := newTestManager(t)
	// Prime the completed partial: Work goes straight from download to
	// verification, so the quit hits the verify stage.
	partial := filepath.Join(lib, ".partial", fileZIM+".part")
	if err := os.MkdirAll(filepath.Dir(partial), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(partial, data, 0o644); err != nil {
		t.Fatalf("prime partial: %v", err)
	}
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	workErr := make(chan error, 1)
	go func() { workErr <- m.Work(ctx) }()
	tick := time.NewTicker(2 * time.Millisecond)
	defer tick.Stop()
	waitErr := make(chan error, 1)
	go func() {
		deadline := time.After(60 * time.Second)
		for {
			select {
			case <-deadline:
				waitErr <- fmt.Errorf("job never reached the verify stage")
				return
			case <-tick.C:
				job, err := q.Get(id)
				if err == nil && job.Stage == stageVerify {
					waitErr <- nil
					return
				}
			}
		}
	}()
	select {
	case err := <-workErr:
		t.Fatalf("Work returned before reaching verification: %v", err)
	case err := <-waitErr:
		if err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	if err := <-workErr; err == nil {
		t.Fatal("Work with a canceled context succeeded, want an error")
	}

	// The completed download must survive the quit: not failed, not deleted,
	// reservation kept.
	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateRunning {
		t.Fatalf("job state = %s, want running", job.State)
	}
	if job.ReservedBytes == 0 {
		t.Error("reservation was released on quit during verification")
	}
	if st, err := os.Stat(partial); err != nil || st.Size() != int64(len(data)) {
		t.Fatalf("completed download was deleted on quit during verification (size %v): %v", st, err)
	}

	// Second launch finishes and publishes exactly once.
	if n, err := q.Recover(); err != nil || n != 1 {
		t.Fatalf("Recover = %d, %v; want 1", n, err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("resumed Work: %v", err)
	}
	job, err = q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 1 {
		t.Errorf("installed rows = %d, want exactly 1", len(inst))
	}
}

func TestDiskFullMidWritePausesAndKeepsPartial(t *testing.T) {
	data := makeTestZIM(600 << 10)
	first := newMirror(t)
	first.data = data
	second := newMirror(t)
	second.data = data
	doc := buildMeta4(t, fileZIM, data, []string{first.zimURL(), second.zimURL()})
	first.meta4 = doc
	second.meta4 = doc

	m, q, lib := newTestManager(t)
	// The injected writer fills up after ~300 KiB: the first 256 KiB chunk
	// lands, the next write fails with ENOSPC.
	m.partWriter = func(f *os.File) io.Writer {
		return &enospcWriter{f: f, limit: 300 << 10}
	}
	id, err := m.Enqueue(testEntry(first.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Work returns normally: the disk-full job is paused, not failed.
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StatePaused {
		t.Fatalf("job state = %s, want paused (error: %s)", job.State, job.Error)
	}
	if job.Stage != stageDownload {
		t.Errorf("job stage = %s, want %s", job.Stage, stageDownload)
	}
	if !strings.Contains(job.Error, "Disk is full") {
		t.Errorf("job error %q does not explain the disk-full pause to the parent", job.Error)
	}
	if job.ReservedBytes == 0 {
		t.Error("reservation was released on disk-full pause")
	}
	partial := filepath.Join(lib, ".partial", fileZIM+".part")
	got, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("read partial: %v", err)
	}
	// The partial holds exactly the accepted prefix - truncated to the last
	// durable offset, not a byte more.
	if len(got) == 0 || len(got) >= len(data) {
		t.Fatalf("partial size = %d, want 0 < size < %d", len(got), len(data))
	}
	if !bytes.Equal(got, data[:len(got)]) {
		t.Error("partial is not the durable prefix of the source")
	}
	// Mirror retries stop: the failing attempt is not resumed and the next
	// mirror is not tried, because the disk - not the mirrors - is the
	// problem.
	if n := len(first.zimRequests()); n != 1 {
		t.Errorf("first mirror served %d ZIM requests, want 1", n)
	}
	if n := len(second.zimRequests()); n != 0 {
		t.Errorf("second mirror served %d ZIM requests, want 0", n)
	}
}

func TestWrongRangeStartFallsBack(t *testing.T) {
	data := makeTestZIM(200 << 10)
	bad := newMirror(t)
	bad.data = data
	bad.cutFirst = true
	bad.cutBytes = 60 << 10
	bad.wrongRangeStart = 4096
	good := newMirror(t)
	good.data = data
	doc := buildMeta4(t, fileZIM, data, []string{bad.zimURL(), good.zimURL()})
	bad.meta4 = doc
	good.meta4 = doc

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(bad.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	// The cut request and the mismatched 206 hit the bad mirror, which is
	// then abandoned; the good mirror resumes from the cut offset.
	if n := len(bad.zimRequests()); n != 2 {
		t.Errorf("bad mirror served %d ZIM requests, want 2 (cut + bad 206)", n)
	}
	starts := good.rangeStarts()
	if len(starts) != 1 || starts[0] != 60<<10 {
		t.Errorf("good mirror Range starts = %v, want [%d]", starts, 60<<10)
	}
}

func TestIgnoredRangeRestartsFromZero(t *testing.T) {
	data := makeTestZIM(300 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})
	mir.cutFirst = true
	mir.cutBytes = 60 << 10
	mir.ignoreRange = true

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	// Appending the whole file onto the 60 KiB prefix would exceed the
	// declared size and fail the checksum; only a truncate-and-restart can
	// produce byte-identical output.
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	starts := mir.rangeStarts()
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 60<<10 {
		t.Errorf("Range starts = %v, want [0 %d]", starts, 60<<10)
	}
	// The ranged request was answered 200: the whole-file restart happened.
	if st := mir.zstatuses(); len(st) != 2 || st[0] != http.StatusOK || st[1] != http.StatusOK {
		t.Errorf("ZIM response statuses = %v, want [200 200]", st)
	}
}

func TestStalledMirrorFallsBack(t *testing.T) {
	data := makeTestZIM(200 << 10)
	stalled := newMirror(t)
	stalled.data = data
	stalled.holdFirst = true
	stalled.cutBytes = 64 << 10
	good := newMirror(t)
	good.data = data
	doc := buildMeta4(t, fileZIM, data, []string{stalled.zimURL(), good.zimURL()})
	stalled.meta4 = doc
	good.meta4 = doc

	m, q, _ := newTestManager(t)
	m.idleTimeout = 200 * time.Millisecond
	id, err := m.Enqueue(testEntry(stalled.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	start := time.Now()
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Work took %s, the idle deadline did not fire", elapsed)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	// The stalled mirror got one request and was abandoned for the next one,
	// which resumes from the prefix the stall left behind.
	if n := len(stalled.zimRequests()); n != 1 {
		t.Errorf("stalled mirror served %d ZIM requests, want 1", n)
	}
	starts := good.rangeStarts()
	if len(starts) != 1 || starts[0] != 64<<10 {
		t.Errorf("good mirror Range starts = %v, want [%d]", starts, 64<<10)
	}
}

func TestHeaderTimeoutFallsBack(t *testing.T) {
	data := makeTestZIM(200 << 10)
	slow := newMirror(t)
	slow.data = data
	slow.holdHeaders = true
	good := newMirror(t)
	good.data = data
	doc := buildMeta4(t, fileZIM, data, []string{slow.zimURL(), good.zimURL()})
	slow.meta4 = doc
	good.meta4 = doc

	m, q, _ := newTestManager(t)
	m.headerTimeout = 200 * time.Millisecond
	id, err := m.Enqueue(testEntry(slow.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	start := time.Now()
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Work took %s, the header timeout did not fire", elapsed)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	if n := len(slow.zimRequests()); n != 1 {
		t.Errorf("slow mirror served %d ZIM requests, want 1 (abandoned after the header timeout)", n)
	}
	if n := len(good.zimRequests()); n != 1 {
		t.Errorf("good mirror served %d ZIM requests, want 1", n)
	}
}

func TestExtraTrailingBytesRejected(t *testing.T) {
	data := makeTestZIM(200 << 10)
	bad := newMirror(t)
	bad.data = data
	bad.extraBytes = 4096
	good := newMirror(t)
	good.data = data
	doc := buildMeta4(t, fileZIM, data, []string{bad.zimURL(), good.zimURL()})
	bad.meta4 = doc
	good.meta4 = doc

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(bad.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	// The over-long mirror was abandoned after one request; the good mirror
	// served the whole file from byte 0 (nothing partial was kept).
	if n := len(bad.zimRequests()); n != 1 {
		t.Errorf("over-long mirror served %d ZIM requests, want 1", n)
	}
	starts := good.rangeStarts()
	if len(starts) != 1 || starts[0] != 0 {
		t.Errorf("good mirror Range starts = %v, want [0]", starts)
	}
}

func TestShortBodyResumesNotCompletes(t *testing.T) {
	data := makeTestZIM(200 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})
	// The first response declares the full Content-Length but sends fewer
	// bytes before closing: a truncated 200 must never complete the job.
	mir.cutFirst = true
	mir.cutBytes = 60 << 10

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateDone {
		t.Fatalf("job state = %s, want done (error: %s)", job.State, job.Error)
	}
	if got, err := os.ReadFile(filepath.Join(m.lib, fileZIM)); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("installed ZIM wrong: err=%v", err)
	}
	// The truncated 200 was a resumable interruption, never a completion:
	// the same mirror was asked for the remainder with a Range request.
	starts := mir.rangeStarts()
	if len(starts) != 2 || starts[0] != 0 || starts[1] != 60<<10 {
		t.Errorf("Range starts = %v, want [0 %d]", starts, 60<<10)
	}
	if st := mir.zstatuses(); len(st) != 2 || st[0] != http.StatusOK || st[1] != http.StatusPartialContent {
		t.Errorf("ZIM response statuses = %v, want [200 206]", st)
	}
}

func TestStalledMeta4BodyFailsFast(t *testing.T) {
	data := makeTestZIM(200 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.stallMeta4 = true
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, q, _ := newTestManager(t)
	m.idleTimeout = 200 * time.Millisecond
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	start := time.Now()
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("Work took %s, the meta4 idle deadline did not fire", elapsed)
	}

	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed (error: %s)", job.State, job.Error)
	}
	if !strings.Contains(job.Error, "stall") {
		t.Errorf("job error = %q, want a stall error", job.Error)
	}
	if n := len(mir.requests()); n != 1 {
		t.Errorf("mirror served %d requests, want 1 (one meta4 attempt)", n)
	}
}
func TestMeta4FileNameCannotEscapeLibrary(t *testing.T) {
	data := makeTestZIM(100 << 10)
	mir := newMirror(t)
	mir.data = data
	// The metalink declares a name that would walk out of the library dir;
	// the stored file must come from the catalog entry instead, so the
	// download lands inside the library under the generated name.
	mir.meta4 = buildMeta4(t, "../evil.zim", data, []string{mir.zimURL()})

	m, _, lib := newTestManager(t)
	if _, err := m.Enqueue(testEntry(mir.meta4URL())); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(lib, fileZIM))
	if err != nil {
		t.Fatalf("ZIM not installed under the generated name: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("installed ZIM content differs from the source")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(lib), "evil.zim")); !os.IsNotExist(err) {
		t.Error("the meta4 file name was written outside the library dir")
	}
	if _, err := os.Stat(filepath.Join(lib, "evil.zim")); !os.IsNotExist(err) {
		t.Error("the meta4 file name was used as a path inside the library dir")
	}

	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 1 || inst[0].File != fileZIM {
		t.Errorf("installed rows = %+v, want one row with file %q", inst, fileZIM)
	}
}

func TestRemoveRefusesPathOutsideLibrary(t *testing.T) {
	m, _, lib := newTestManager(t)
	// A corrupted installed row whose stored name escapes the library dir:
	// Remove must refuse to delete anything outside it.
	if _, err := m.store.DB().Exec(`INSERT INTO installed (id, name, title, language, file, size, installed_at)
		VALUES ('urn:uuid:escape', 'evil', '', '', '../outside.zim', 1, '2026-08-07T00:00:00Z')`); err != nil {
		t.Fatalf("seed installed row: %v", err)
	}
	outside := filepath.Join(filepath.Dir(lib), "outside.zim")
	if err := os.WriteFile(outside, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed outside file: %v", err)
	}
	if err := m.Remove("urn:uuid:escape"); err == nil {
		t.Fatal("Remove with an escaping stored name succeeded, want a refusal")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("file outside the library dir was deleted: %v", err)
	}
	// The row stays, so a retry is refused the same way.
	inst, err := m.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if len(inst) != 1 {
		t.Errorf("installed rows = %d, want the row kept", len(inst))
	}
}

func TestInstalledFileSurvivesFailedRedownload(t *testing.T) {
	data := makeTestZIM(100 << 10)
	mir := newMirror(t)
	mir.data = data
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, q, lib := newTestManager(t)
	if _, err := m.Enqueue(testEntry(mir.meta4URL())); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(lib, fileZIM))
	if err != nil {
		t.Fatalf("read installed ZIM: %v", err)
	}

	// Force a re-download of the same entry: the installed row is gone but
	// the verified file stays on disk.
	if _, err := m.store.DB().Exec(`DELETE FROM installed WHERE id = ?`, testEntryID); err != nil {
		t.Fatalf("drop installed row: %v", err)
	}

	// The fresh download fails; the existing installed file must survive
	// untouched, because nothing is deleted or overwritten before the new
	// download has fully verified. The new meta4 declares a different size
	// so the installed file cannot short-circuit into a re-publish.
	mir.meta4 = buildMeta4(t, fileZIM, makeTestZIM(200<<10), []string{mir.zimURL()})
	mir.fail500 = true
	id2, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	job, err := q.Get(id2)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed (error: %s)", job.State, job.Error)
	}
	got, err := os.ReadFile(filepath.Join(lib, fileZIM))
	if err != nil {
		t.Fatalf("installed file vanished on failed re-download: %v", err)
	}
	if !bytes.Equal(got, installed) {
		t.Error("installed file was overwritten by the failed re-download")
	}
}

// TestDialPolicyRefusesBannedDestinations drives the manager's client
// directly so the meta4 fetch itself does not need a reachable mirror: the
// loopback opt-in is off here, so every banned destination must be refused
// at dial time (localhost maps to 127.0.0.1 through the hosts file - a DNS
// trick, not an IP-literal URL - which is exactly the case the resolved-IP
// check has to cover).
func TestDialPolicyRefusesBannedDestinations(t *testing.T) {
	s, q, lib := newStoreQueue(t)
	m, err := NewManager(s, q, disk.NewReservations(q), lib)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	f, err := os.CreateTemp(t.TempDir(), "part")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	for _, tc := range []struct {
		url     string
		wantErr string
	}{
		{"http://localhost" + zimPath, "refusing to connect"},       // 127.0.0.1 via DNS
		{"http://10.0.0.1" + zimPath, "refusing to connect"},        // RFC 1918
		{"http://100.64.0.1" + zimPath, "refusing to connect"},      // CGNAT
		{"http://[::1]" + zimPath, "refusing to connect"},           // IPv6 loopback
		{"http://169.254.169.254" + zimPath, "refusing to connect"}, // link-local
		{"http://224.0.0.1" + zimPath, "refusing to connect"},       // multicast
		{"http://0.0.0.0" + zimPath, "refusing to connect"},         // unspecified
		{"file:///etc/passwd", "not a usable URL"},                  // not http(s)
	} {
		_, _, _, err := m.fetchRange(context.Background(), f, tc.url, 0, 16)
		if err == nil {
			t.Errorf("fetchRange(%s) connected, want a refusal", tc.url)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("fetchRange(%s) error = %q, want %q", tc.url, err, tc.wantErr)
		}
	}
}

func TestRedirectToPrivateAddressRefused(t *testing.T) {
	data := makeTestZIM(16 << 10)
	mir := newMirror(t)
	mir.data = data
	// The mirror answers the ZIM request with a redirect into a private
	// range; following it must fail at the dial policy, never connect.
	mir.redirectTo = "http://10.0.0.1" + zimPath
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed (error: %s)", job.State, job.Error)
	}
	if !strings.Contains(job.Error, "refusing to connect") {
		t.Errorf("job error = %q, want the dial refusal", job.Error)
	}
	if reqs := mir.zimRequests(); len(reqs) != 1 {
		t.Errorf("ZIM requests = %v, want exactly the redirect request", reqs)
	}
}
func TestRedirectLoopStopsAtCap(t *testing.T) {
	data := makeTestZIM(16 << 10)
	mir := newMirror(t)
	mir.data = data
	// The mirror redirects every ZIM request back to itself; the client's
	// redirect cap must stop the loop instead of following it forever.
	mir.redirectTo = mir.zimURL()
	mir.meta4 = buildMeta4(t, fileZIM, data, []string{mir.zimURL()})

	m, q, _ := newTestManager(t)
	id, err := m.Enqueue(testEntry(mir.meta4URL()))
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := m.Work(context.Background()); err != nil {
		t.Fatalf("Work: %v", err)
	}
	job, err := q.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if job.State != jobs.StateFailed {
		t.Fatalf("job state = %s, want failed (error: %s)", job.State, job.Error)
	}
	if !strings.Contains(job.Error, "redirects") {
		t.Errorf("job error = %q, want the redirect-cap refusal", job.Error)
	}
	if reqs := mir.zimRequests(); len(reqs) != 6 {
		t.Errorf("ZIM requests = %d, want 6 (initial + 5 redirects)", len(reqs))
	}
}
