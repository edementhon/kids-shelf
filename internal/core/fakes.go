package core

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// SourceCall records one SourceRegistry.AddSource call.
type SourceCall struct {
	Path      string
	Kind      SourceKind
	Allowlist []string
}

// FakeSourceRegistry records registered media roots.
type FakeSourceRegistry struct {
	mu    sync.Mutex
	Err   error
	calls []SourceCall
}

var _ SourceRegistry = (*FakeSourceRegistry)(nil)

// AddSource implements SourceRegistry.
func (f *FakeSourceRegistry) AddSource(ctx context.Context, path string, kind SourceKind, allowlist []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.calls = append(f.calls, SourceCall{Path: path, Kind: kind, Allowlist: append([]string(nil), allowlist...)})
	return nil
}

// Sources returns a copy of the recorded registrations, in call order.
func (f *FakeSourceRegistry) Sources() []SourceCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SourceCall, len(f.calls))
	for i, c := range f.calls {
		c.Allowlist = append([]string(nil), c.Allowlist...)
		out[i] = c
	}
	return out
}

// Fakes for the core interfaces, for unit tests only. Every fake is safe for
// concurrent use; read the recorded calls through the accessor methods, which
// return copies. Fakes deep-copy caller-owned slices, maps and pointers on
// entry, so callers may reuse or mutate their inputs after a call.

// FakeIngestQueue records Enqueue and IngestStatus calls and returns a
// fresh job id per Enqueue. A job finishes when the test marks it done with
// Complete, or — when the queue was built with NewFakeIngestQueue — once a
// published episode's source matches the job's enqueued provenance, exactly
// how migrate observes the real pipeline.
type FakeIngestQueue struct {
	mu sync.Mutex
	// Err, when set, is returned by every Enqueue call without recording it.
	Err error
	// StatusErr, when set, is returned by every IngestStatus call without
	// recording it.
	StatusErr error

	next   int
	calls  []EnqueueCall
	srcs   map[string]Source    // job id -> enqueued source (provenance key)
	done   map[string]EpisodeID // job id -> published episode; present means finished
	pub    *FakePublisher       // when set, IngestStatus resolves unfinished jobs against its provenance
	status []StatusCall
}

// EnqueueCall is one recorded Enqueue invocation.
type EnqueueCall struct {
	File   string
	Source Source
}

// StatusCall is one recorded IngestStatus invocation and its result.
type StatusCall struct {
	JobID string
	ID    EpisodeID
	Done  bool
}

// NewFakeIngestQueue returns a queue that resolves finished jobs against
// the episodes a FakePublisher has published: IngestStatus reports done
// once an episode's source matches the job's enqueued provenance, which is
// how migrate observes the real ingest pipeline.
func NewFakeIngestQueue(pub *FakePublisher) *FakeIngestQueue {
	return &FakeIngestQueue{pub: pub}
}

// Enqueue implements IngestQueue. Like the real queue it returns the
// existing job id for a source it has seen; every call is still recorded,
// so tests can assert how often a producer enqueued.
func (f *FakeIngestQueue) Enqueue(ctx context.Context, file string, src Source) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", f.Err
	}
	f.calls = append(f.calls, EnqueueCall{File: file, Source: src})
	if f.srcs == nil {
		f.srcs = make(map[string]Source)
		f.done = make(map[string]EpisodeID)
	}
	for id, s := range f.srcs {
		if s.Kind == src.Kind && s.Path == src.Path {
			return id, nil
		}
	}
	f.next++
	id := fmt.Sprintf("job-%d", f.next)
	f.srcs[id] = src
	return id, nil
}

// IngestStatus implements IngestQueue. An unknown job id is an error; the
// caller asks about jobs Enqueue returned.
func (f *FakeIngestQueue) IngestStatus(ctx context.Context, jobID string) (EpisodeID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.StatusErr != nil {
		return "", false, f.StatusErr
	}
	src, ok := f.srcs[jobID]
	if !ok {
		return "", false, fmt.Errorf("core: unknown ingest job %q", jobID)
	}
	id, finished := f.done[jobID]
	if !finished && f.pub != nil {
		for _, ep := range f.pub.Published() {
			if ep.Source.Kind == src.Kind && ep.Source.Path == src.Path {
				id, finished = ep.ID, true
				f.done[jobID] = id
				break
			}
		}
	}
	f.status = append(f.status, StatusCall{JobID: jobID, ID: id, Done: finished})
	return id, finished, nil
}

// Complete marks a previously enqueued job as finished with the given
// episode, as the ingest pipeline would after publishing it; passing the
// zero EpisodeID marks the job finished without an episode. It returns an
// error for an unknown job id.
func (f *FakeIngestQueue) Complete(jobID string, ep EpisodeID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.srcs[jobID]; !ok {
		return fmt.Errorf("core: unknown ingest job %q", jobID)
	}
	f.done[jobID] = ep
	return nil
}

// Calls returns a copy of the recorded Enqueue calls, in order.
func (f *FakeIngestQueue) Calls() []EnqueueCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]EnqueueCall(nil), f.calls...)
}

// Enqueued reports how many Enqueue calls were recorded.
func (f *FakeIngestQueue) Enqueued() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// StatusCalls returns a copy of the recorded IngestStatus calls, in order.
func (f *FakeIngestQueue) StatusCalls() []StatusCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StatusCall(nil), f.status...)
}

// FakePublisher records Publish calls.
type FakePublisher struct {
	mu sync.Mutex
	// Err, when set, is returned by every Publish call without recording it.
	Err error

	published []Episode
}

// Publish implements Publisher.
func (f *FakePublisher) Publish(ctx context.Context, ep Episode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.published = append(f.published, ep)
	return nil
}

// Published returns a copy of the recorded episodes, in order.
func (f *FakePublisher) Published() []Episode {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Episode(nil), f.published...)
}

// Called reports how many Publish calls were recorded.
func (f *FakePublisher) Called() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.published)
}

// CreditCall is one recorded Credit invocation that resulted in a credit.
type CreditCall struct {
	Kid     string
	Counter string
	Minutes float64
	AwardID string
}

// FakeCrediter records Credit calls and applies the idempotency contract: a
// second Credit with the same awardID is accepted without recording another
// credit (the real implementation must behave the same).
type FakeCrediter struct {
	mu sync.Mutex
	// Err, when set, is returned by every Credit call without recording it.
	Err error

	seen     map[string]bool // awardID -> credited
	credits  []CreditCall
	attempts int
}

// Credit implements Crediter.
func (f *FakeCrediter) Credit(ctx context.Context, kid string, counter string, minutes float64, awardID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.Err != nil {
		return f.Err
	}
	if f.seen == nil {
		f.seen = make(map[string]bool)
	}
	if f.seen[awardID] {
		return nil
	}
	f.seen[awardID] = true
	f.credits = append(f.credits, CreditCall{Kid: kid, Counter: counter, Minutes: minutes, AwardID: awardID})
	return nil
}

// Credits returns a copy of the recorded credits, in order.
func (f *FakeCrediter) Credits() []CreditCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CreditCall(nil), f.credits...)
}

// Attempts reports how many Credit calls were received, credited or not.
func (f *FakeCrediter) Attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// CreateKidCall is one recorded CreateKid invocation.
type CreateKidCall struct {
	Name string
	PIN  string
	ID   KidID
}

// FindKidCall is one recorded FindKid invocation and its result.
type FindKidCall struct {
	Name  string
	ID    KidID
	Found bool
}

// KidRecord is a kid registered in FakeKids.
type KidRecord struct {
	ID   KidID
	Name string
	PIN  string
}

// FakeKids keeps kids in memory and records every call. CreateKid is
// idempotent by name: creating a name that already exists returns the
// existing id without a second registration, so a re-run of migrate is
// safe (the real implementation must behave the same).
type FakeKids struct {
	mu sync.Mutex
	// Err, when set, is returned by every call without recording it.
	Err error

	next       int
	byName     map[string]KidID
	kids       map[KidID]string // id -> name
	pins       map[KidID]string // id -> pin
	grownupPIN string

	creates  []CreateKidCall
	finds    []FindKidCall
	pinCalls []string
}

// CreateKid implements Kids.
func (f *FakeKids) CreateKid(ctx context.Context, name string, pin string) (KidID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", f.Err
	}
	if f.byName == nil {
		f.byName = make(map[string]KidID)
		f.kids = make(map[KidID]string)
		f.pins = make(map[KidID]string)
	}
	id, exists := f.byName[name]
	if !exists {
		f.next++
		id = KidID(fmt.Sprintf("kid-%d", f.next))
		f.byName[name] = id
		f.kids[id] = name
		f.pins[id] = pin
	}
	f.creates = append(f.creates, CreateKidCall{Name: name, PIN: pin, ID: id})
	return id, nil
}

// FindKid implements Kids.
func (f *FakeKids) FindKid(ctx context.Context, name string) (KidID, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return "", false, f.Err
	}
	id, found := f.byName[name]
	f.finds = append(f.finds, FindKidCall{Name: name, ID: id, Found: found})
	return id, found, nil
}

// SetGrownupPIN implements Kids.
func (f *FakeKids) SetGrownupPIN(ctx context.Context, pin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.grownupPIN = pin
	f.pinCalls = append(f.pinCalls, pin)
	return nil
}

// Kids returns a copy of the registered kids.
func (f *FakeKids) Kids() []KidRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]KidRecord, 0, len(f.kids))
	for id, name := range f.kids {
		out = append(out, KidRecord{ID: id, Name: name, PIN: f.pins[id]})
	}
	return out
}

// Creates returns a copy of the recorded CreateKid calls, in order.
func (f *FakeKids) Creates() []CreateKidCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]CreateKidCall(nil), f.creates...)
}

// Finds returns a copy of the recorded FindKid calls, in order.
func (f *FakeKids) Finds() []FindKidCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FindKidCall(nil), f.finds...)
}

// PinCalls returns a copy of the recorded SetGrownupPIN arguments, in
// order.
func (f *FakeKids) PinCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.pinCalls...)
}

// GrownupPIN returns the PIN last set through SetGrownupPIN, "" when never
// called.
func (f *FakeKids) GrownupPIN() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grownupPIN
}

// ImportAccountCall is one recorded ImportAccount invocation.
type ImportAccountCall struct {
	Kid   KidID
	Today float64
	Saved float64
	Day   string
}

// FakeBudgetImporter records ImportAccount calls.
type FakeBudgetImporter struct {
	mu sync.Mutex
	// Err, when set, is returned by every call without recording it.
	Err error

	accounts []ImportAccountCall
}

// ImportAccount implements BudgetImporter.
func (f *FakeBudgetImporter) ImportAccount(ctx context.Context, kid KidID, today float64, saved float64, day string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.accounts = append(f.accounts, ImportAccountCall{Kid: kid, Today: today, Saved: saved, Day: day})
	return nil
}

// Accounts returns a copy of the recorded accounts, in order.
func (f *FakeBudgetImporter) Accounts() []ImportAccountCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ImportAccountCall(nil), f.accounts...)
}

// LookupCall is one recorded BySource invocation and its result.
type LookupCall struct {
	Source Source
	Ref    EpisodeRef
	Found  bool
}

// BinCall is one recorded SetBin invocation.
type BinCall struct {
	ShowID ShowID
	Bin    string
	// Name is set for SetShowBin calls (ShowID is then empty).
	Name string
}

// FakeEpisodeLookup resolves sources through the provenance a
// FakePublisher recorded — the same hand-off the real pipeline makes when
// ingest publishes a verified episode with its source — and records every
// BySource and SetBin call. A lookup built without a publisher resolves
// nothing.
type FakeEpisodeLookup struct {
	mu sync.Mutex
	// Err, when set, is returned by every call without recording it.
	Err error

	pub     *FakePublisher
	lookups []LookupCall
	bins    []BinCall
}

// NewFakeEpisodeLookup returns a lookup that resolves sources against the
// episodes pub has published.
func NewFakeEpisodeLookup(pub *FakePublisher) *FakeEpisodeLookup {
	return &FakeEpisodeLookup{pub: pub}
}

// BySource implements EpisodeLookup. It matches the source's kind and
// path — for legacy data the path is the "show/file" key.
func (f *FakeEpisodeLookup) BySource(ctx context.Context, src Source) (EpisodeRef, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return EpisodeRef{}, false, f.Err
	}
	ref, found := f.resolve(src)
	f.lookups = append(f.lookups, LookupCall{Source: src, Ref: ref, Found: found})
	return ref, found, nil
}

// resolve searches the publisher's filed episodes for one whose provenance
// is this source.
func (f *FakeEpisodeLookup) resolve(src Source) (EpisodeRef, bool) {
	if f.pub == nil {
		return EpisodeRef{}, false
	}
	for _, ep := range f.pub.Published() {
		if ep.Source.Kind == src.Kind && ep.Source.Path == src.Path {
			return EpisodeRef{Episode: ep.ID, Show: ShowID(ep.ShowID)}, true
		}
	}
	return EpisodeRef{}, false
}

// SetBin implements EpisodeLookup.
func (f *FakeEpisodeLookup) SetBin(ctx context.Context, show ShowID, bin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.bins = append(f.bins, BinCall{ShowID: show, Bin: bin})
	return nil
}

// SetShowBin implements EpisodeLookup.
func (f *FakeEpisodeLookup) SetShowBin(ctx context.Context, showName string, bin string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.bins = append(f.bins, BinCall{Name: showName, Bin: bin})
	return nil
}

// Lookups returns a copy of the recorded BySource calls, in order.
func (f *FakeEpisodeLookup) Lookups() []LookupCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]LookupCall(nil), f.lookups...)
}

// Bins returns a copy of the recorded SetBin calls, in order.
func (f *FakeEpisodeLookup) Bins() []BinCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]BinCall(nil), f.bins...)
}

// ResumeCall is one recorded ImportResume invocation.
type ResumeCall struct {
	Kid     KidID
	Ep      EpisodeID
	PosSec  float64
	Updated time.Time
}

// WatchCall is one recorded ImportWatch invocation.
type WatchCall struct {
	Kid       KidID
	Ep        EpisodeID
	WatchedAt time.Time
	Title     string
	Cost      float64 // minutes the watch was metered; 0 for free watches
	Free      bool    // grown-up watch-together playback is unmetered
}

// HouseholdRecentCall is one recorded ImportHouseholdRecent invocation.
type HouseholdRecentCall struct {
	Ep  EpisodeID
	At  time.Time
	Kid KidID // may be empty: the legacy recent list is household-wide
}

// FakePlaybackImporter records playback imports.
type FakePlaybackImporter struct {
	mu sync.Mutex
	// Err, when set, is returned by every call without recording it.
	Err error

	resumes []ResumeCall
	watches []WatchCall
	recents []HouseholdRecentCall
}

// ImportResume implements PlaybackImporter.
func (f *FakePlaybackImporter) ImportResume(ctx context.Context, kid KidID, ep EpisodeID, posSec float64, updated time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.resumes = append(f.resumes, ResumeCall{Kid: kid, Ep: ep, PosSec: posSec, Updated: updated})
	return nil
}

// ImportWatch implements PlaybackImporter.
func (f *FakePlaybackImporter) ImportWatch(ctx context.Context, kid KidID, ep EpisodeID, watchedAt time.Time, title string, cost float64, free bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.watches = append(f.watches, WatchCall{Kid: kid, Ep: ep, WatchedAt: watchedAt, Title: title, Cost: cost, Free: free})
	return nil
}

// ImportHouseholdRecent implements PlaybackImporter.
func (f *FakePlaybackImporter) ImportHouseholdRecent(ctx context.Context, ep EpisodeID, at time.Time, kid KidID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.recents = append(f.recents, HouseholdRecentCall{Ep: ep, At: at, Kid: kid})
	return nil
}

// Resumes returns a copy of the recorded ImportResume calls, in order.
func (f *FakePlaybackImporter) Resumes() []ResumeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ResumeCall(nil), f.resumes...)
}

// Watches returns a copy of the recorded ImportWatch calls, in order.
func (f *FakePlaybackImporter) Watches() []WatchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]WatchCall(nil), f.watches...)
}

// Recents returns a copy of the recorded ImportHouseholdRecent calls, in
// order.
func (f *FakePlaybackImporter) Recents() []HouseholdRecentCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]HouseholdRecentCall(nil), f.recents...)
}

// FactStatsCall is one recorded ImportFactStats invocation.
type FactStatsCall struct {
	Kid   KidID
	Mode  string
	Key   string
	Stats FactStats
}

// SessionCall is one recorded ImportSessions invocation.
type SessionCall struct {
	Kid     KidID
	Session Session
}

// AwardsCall is one recorded ImportAwards invocation.
type AwardsCall struct {
	Kid    KidID
	Awards Awards
}

// FlagCall is one recorded ImportFlags invocation.
type FlagCall struct {
	Kid  KidID
	Flag Flag
}

// AwardLogCall is one recorded ImportAwardLog invocation.
type AwardLogCall struct {
	Entry AwardLogEntry
}

// SpellingCall is one recorded ImportSpelling invocation.
type SpellingCall struct {
	Kid      KidID
	Spelling Spelling
}

// BossCall is one recorded ImportBosses invocation.
type BossCall struct {
	Kid  KidID
	Boss BossState
}

// ConfigCall is one recorded ImportConfig invocation.
type ConfigCall struct {
	Config Config
}

// FakePracticeImporter records practice imports.
type FakePracticeImporter struct {
	mu sync.Mutex
	// Err, when set, is returned by every call without recording it.
	Err error

	factStats []FactStatsCall
	sessions  []SessionCall
	awards    []AwardsCall
	flags     []FlagCall
	awardLog  []AwardLogCall
	spelling  []SpellingCall
	bosses    []BossCall
	configs   []ConfigCall
}

// ImportFactStats implements PracticeImporter.
func (f *FakePracticeImporter) ImportFactStats(ctx context.Context, kid KidID, mode string, key string, s FactStats) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	s.Times = append([]int(nil), s.Times...)
	if s.MedianMS != nil {
		v := *s.MedianMS
		s.MedianMS = &v
	}
	if s.BestMS != nil {
		v := *s.BestMS
		s.BestMS = &v
	}
	f.factStats = append(f.factStats, FactStatsCall{Kid: kid, Mode: mode, Key: key, Stats: s})
	return nil
}

// ImportSessions implements PracticeImporter.
func (f *FakePracticeImporter) ImportSessions(ctx context.Context, kid KidID, s Session) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	if s.MedianMS != nil {
		v := *s.MedianMS
		s.MedianMS = &v
	}
	f.sessions = append(f.sessions, SessionCall{Kid: kid, Session: s})
	return nil
}

// ImportAwards implements PracticeImporter.
func (f *FakePracticeImporter) ImportAwards(ctx context.Context, kid KidID, a Awards) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.awards = append(f.awards, AwardsCall{Kid: kid, Awards: a})
	return nil
}

// ImportFlags implements PracticeImporter.
func (f *FakePracticeImporter) ImportFlags(ctx context.Context, kid KidID, fl Flag) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.flags = append(f.flags, FlagCall{Kid: kid, Flag: fl})
	return nil
}

// ImportAwardLog implements PracticeImporter.
func (f *FakePracticeImporter) ImportAwardLog(ctx context.Context, e AwardLogEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	f.awardLog = append(f.awardLog, AwardLogCall{Entry: e})
	return nil
}

// ImportSpelling implements PracticeImporter.
func (f *FakePracticeImporter) ImportSpelling(ctx context.Context, kid KidID, s Spelling) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	s.Records = append([]SpellingRecord(nil), s.Records...)
	s.Sessions = append([]SpellingSession(nil), s.Sessions...)
	f.spelling = append(f.spelling, SpellingCall{Kid: kid, Spelling: s})
	return nil
}

// ImportBosses implements PracticeImporter.
func (f *FakePracticeImporter) ImportBosses(ctx context.Context, kid KidID, b BossState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	b.Defeated = append([]string(nil), b.Defeated...)
	b.HP = copyIntMap(b.HP)
	b.PHP = copyIntMap(b.PHP)
	if b.PendingBid != nil {
		v := *b.PendingBid
		b.PendingBid = &v
	}
	f.bosses = append(f.bosses, BossCall{Kid: kid, Boss: b})
	return nil
}

// ImportConfig implements PracticeImporter.
func (f *FakePracticeImporter) ImportConfig(ctx context.Context, c Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return f.Err
	}
	c.Global = copyFloatMap(c.Global)
	if c.Kids != nil {
		kids := make(map[string]map[string]float64, len(c.Kids))
		for name, m := range c.Kids {
			kids[name] = copyFloatMap(m)
		}
		c.Kids = kids
	}
	f.configs = append(f.configs, ConfigCall{Config: c})
	return nil
}

// FactStats returns a copy of the recorded fact imports, in order.
func (f *FakePracticeImporter) FactStats() []FactStatsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FactStatsCall, len(f.factStats))
	for i, c := range f.factStats {
		c.Stats.Times = append([]int(nil), c.Stats.Times...)
		if c.Stats.MedianMS != nil {
			v := *c.Stats.MedianMS
			c.Stats.MedianMS = &v
		}
		if c.Stats.BestMS != nil {
			v := *c.Stats.BestMS
			c.Stats.BestMS = &v
		}
		out[i] = c
	}
	return out
}

// Sessions returns a copy of the recorded session imports, in order.
func (f *FakePracticeImporter) Sessions() []SessionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SessionCall, len(f.sessions))
	for i, c := range f.sessions {
		if c.Session.MedianMS != nil {
			v := *c.Session.MedianMS
			c.Session.MedianMS = &v
		}
		out[i] = c
	}
	return out
}

// Awards returns a copy of the recorded award imports, in order.
func (f *FakePracticeImporter) Awards() []AwardsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AwardsCall(nil), f.awards...)
}

// Flags returns a copy of the recorded flag imports, in order.
func (f *FakePracticeImporter) Flags() []FlagCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]FlagCall(nil), f.flags...)
}

// AwardLog returns a copy of the recorded award-log imports, in order.
func (f *FakePracticeImporter) AwardLog() []AwardLogCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]AwardLogCall(nil), f.awardLog...)
}

// Spelling returns a copy of the recorded spelling imports, in order.
func (f *FakePracticeImporter) Spelling() []SpellingCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]SpellingCall, len(f.spelling))
	for i, c := range f.spelling {
		c.Spelling.Records = append([]SpellingRecord(nil), c.Spelling.Records...)
		c.Spelling.Sessions = append([]SpellingSession(nil), c.Spelling.Sessions...)
		out[i] = c
	}
	return out
}

// Bosses returns a copy of the recorded boss imports, in order.
func (f *FakePracticeImporter) Bosses() []BossCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]BossCall, len(f.bosses))
	for i, c := range f.bosses {
		c.Boss.Defeated = append([]string(nil), c.Boss.Defeated...)
		c.Boss.HP = copyIntMap(c.Boss.HP)
		c.Boss.PHP = copyIntMap(c.Boss.PHP)
		if c.Boss.PendingBid != nil {
			v := *c.Boss.PendingBid
			c.Boss.PendingBid = &v
		}
		out[i] = c
	}
	return out
}

// Configs returns a copy of the recorded config imports, in order.
func (f *FakePracticeImporter) Configs() []ConfigCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ConfigCall, len(f.configs))
	for i, c := range f.configs {
		c.Config.Global = copyFloatMap(c.Config.Global)
		if c.Config.Kids != nil {
			kids := make(map[string]map[string]float64, len(c.Config.Kids))
			for name, m := range c.Config.Kids {
				kids[name] = copyFloatMap(m)
			}
			c.Config.Kids = kids
		}
		out[i] = c
	}
	return out
}

func copyIntMap(m map[string]int) map[string]int {
	if m == nil {
		return nil
	}
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyFloatMap(m map[string]float64) map[string]float64 {
	if m == nil {
		return nil
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Compile-time checks that the fakes implement their interfaces.
var (
	_ IngestQueue      = (*FakeIngestQueue)(nil)
	_ Publisher        = (*FakePublisher)(nil)
	_ Crediter         = (*FakeCrediter)(nil)
	_ Kids             = (*FakeKids)(nil)
	_ BudgetImporter   = (*FakeBudgetImporter)(nil)
	_ EpisodeLookup    = (*FakeEpisodeLookup)(nil)
	_ PlaybackImporter = (*FakePlaybackImporter)(nil)
	_ PracticeImporter = (*FakePracticeImporter)(nil)
)
