package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFakeIngestQueueRecordsCalls(t *testing.T) {
	f := &FakeIngestQueue{}
	ctx := context.Background()
	id1, err := f.Enqueue(ctx, "/media/shows/a.mp4", Source{
		Kind: SourceFolder, Path: "/media/shows/a.mp4", Title: "A", Show: "Shows",
	})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	id2, err := f.Enqueue(ctx, "https://youtu.be/abc", Source{Kind: SourceYouTube, Path: "https://youtu.be/abc"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if id1 == "" || id1 == id2 {
		t.Errorf("job ids = %q, %q, want non-empty and distinct", id1, id2)
	}
	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls len = %d, want 2", len(calls))
	}
	if calls[0].Source.Kind != SourceFolder || calls[1].Source.Kind != SourceYouTube {
		t.Errorf("kinds = %s, %s; want folder then youtube", calls[0].Source.Kind, calls[1].Source.Kind)
	}
	if calls[1].File != "https://youtu.be/abc" {
		t.Errorf("second call file = %q, want the URL", calls[1].File)
	}
	// A failing Enqueue records nothing.
	f2 := &FakeIngestQueue{Err: errors.New("disk full")}
	if _, err := f2.Enqueue(ctx, "/x.mp4", Source{}); err == nil {
		t.Fatal("Enqueue with Err: got nil, want error")
	}
	if f2.Enqueued() != 0 {
		t.Errorf("Enqueued with Err = %d, want 0", f2.Enqueued())
	}
}

func TestFakePublisherRecords(t *testing.T) {
	f := &FakePublisher{}
	ep := Episode{
		ID: "ep-1", ShowID: "show-1", Title: "Episode One",
		File: "/media/ep-1.mp4", DurationSec: 120,
		Source: Source{Kind: SourceFolder, Path: "/media/shows/a.mp4", Title: "A", Show: "Show 1"},
	}
	if err := f.Publish(context.Background(), ep); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := f.Published()
	if len(got) != 1 || got[0].ID != "ep-1" || got[0].ShowID != "show-1" {
		t.Fatalf("Published = %+v, want the episode", got)
	}
	f2 := &FakePublisher{Err: errors.New("duplicate id")}
	if err := f2.Publish(context.Background(), ep); err == nil {
		t.Fatal("Publish with Err: got nil, want error")
	}
	if f2.Called() != 0 {
		t.Errorf("Called with Err = %d, want 0", f2.Called())
	}
}

func TestFakeCrediterIdempotentByAwardID(t *testing.T) {
	f := &FakeCrediter{}
	ctx := context.Background()
	if err := f.Credit(ctx, "kid", "video-minutes", 5, "award-1"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	// Replaying the same award (a retried practice session) must not credit
	// twice.
	if err := f.Credit(ctx, "kid", "video-minutes", 5, "award-1"); err != nil {
		t.Fatalf("Credit retry: %v", err)
	}
	if err := f.Credit(ctx, "kid", "video-minutes", 2, "award-2"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	credits := f.Credits()
	if len(credits) != 2 {
		t.Fatalf("credits = %d, want 2 (award-1 once, award-2 once)", len(credits))
	}
	if credits[0].AwardID != "award-1" || credits[1].AwardID != "award-2" {
		t.Errorf("credits = %+v, want award-1 then award-2", credits)
	}
	if f.Attempts() != 3 {
		t.Errorf("Attempts = %d, want 3", f.Attempts())
	}
}

func TestFakeIngestQueueStatus(t *testing.T) {
	ctx := context.Background()
	f := &FakeIngestQueue{}
	job, err := f.Enqueue(ctx, "/media/shows/a.mp4", Source{Kind: SourceFolder, Path: "/media/shows/a.mp4"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, done, err := f.IngestStatus(ctx, job); err != nil || done {
		t.Fatalf("IngestStatus before completion: done %v, err %v; want running", done, err)
	}
	if err := f.Complete(job, "ep-1"); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	id, done, err := f.IngestStatus(ctx, job)
	if err != nil || !done || id != "ep-1" {
		t.Fatalf("IngestStatus after completion = %q done %v err %v; want ep-1 done", id, done, err)
	}
	// Unknown job ids are errors and record nothing.
	if _, _, err := f.IngestStatus(ctx, "job-99"); err == nil {
		t.Fatal("IngestStatus with unknown job: got nil, want error")
	}
	// A job completed without an episode reads as done with no id; migrate
	// reports those as unmatched.
	job2, err := f.Enqueue(ctx, "/media/shows/b.mp4", Source{Kind: SourceFolder, Path: "/media/shows/b.mp4"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := f.Complete(job2, ""); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	id, done, err = f.IngestStatus(ctx, job2)
	if err != nil || !done || id != "" {
		t.Fatalf("IngestStatus failed job = %q done %v err %v; want done with no id", id, done, err)
	}
	if got := f.StatusCalls(); len(got) != 3 {
		t.Errorf("StatusCalls len = %d, want 3", len(got))
	}
	if _, _, err := (&FakeIngestQueue{StatusErr: errors.New("store down")}).IngestStatus(ctx, "job-1"); err == nil {
		t.Fatal("IngestStatus with StatusErr: got nil, want error")
	}
}

func TestFakeIngestQueueResolvesThroughPublisher(t *testing.T) {
	ctx := context.Background()
	pub := &FakePublisher{}
	q := NewFakeIngestQueue(pub)
	key := Source{Kind: SourceFolder, Path: "/media/Bluey (2018)/S01E01.mp4", Title: "Magic Xylophone"}
	job, err := q.Enqueue(ctx, "/media/legacy/bluey.mp4", key)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, done, err := q.IngestStatus(ctx, job); err != nil || done {
		t.Fatalf("IngestStatus before publish: done %v err %v; want running", done, err)
	}
	// The ingest pipeline publishes the converted episode with the source
	// as provenance; the queue then resolves the job to its id.
	if err := pub.Publish(ctx, Episode{ID: "ep-1", ShowID: "show-1", Title: "Magic Xylophone", Source: key}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	id, done, err := q.IngestStatus(ctx, job)
	if err != nil || !done || id != "ep-1" {
		t.Fatalf("IngestStatus after publish = %q done %v err %v; want ep-1 done", id, done, err)
	}
}

func TestFakeKidsCreateKidIdempotentByName(t *testing.T) {
	ctx := context.Background()
	f := &FakeKids{}
	id1, err := f.CreateKid(ctx, "Alex", "1234")
	if err != nil {
		t.Fatalf("CreateKid: %v", err)
	}
	// Re-creating the same name returns the same id without a duplicate
	// registration; a re-run of migrate must be safe.
	id2, err := f.CreateKid(ctx, "Alex", "1234")
	if err != nil {
		t.Fatalf("CreateKid duplicate: %v", err)
	}
	if id1 == "" || id1 != id2 {
		t.Fatalf("ids = %q, %q; want non-empty and equal", id1, id2)
	}
	kids := f.Kids()
	if len(kids) != 1 || kids[0].Name != "Alex" || kids[0].PIN != "1234" {
		t.Fatalf("Kids = %+v, want one Alex/1234", kids)
	}
	got, found, err := f.FindKid(ctx, "Alex")
	if err != nil || !found || got != id1 {
		t.Fatalf("FindKid Alex = %q %v %v; want %q found", got, found, err, id1)
	}
	if _, found, err := f.FindKid(ctx, "Sam"); err != nil || found {
		t.Fatalf("FindKid Sam: found %v err %v; want not found", found, err)
	}
	if err := f.SetGrownupPIN(ctx, "0000"); err != nil {
		t.Fatalf("SetGrownupPIN: %v", err)
	}
	if f.GrownupPIN() != "0000" || len(f.PinCalls()) != 1 {
		t.Errorf("grownup PIN = %q, calls %v; want 0000, one call", f.GrownupPIN(), f.PinCalls())
	}
	f2 := &FakeKids{Err: errors.New("store down")}
	if _, err := f2.CreateKid(ctx, "Alex", "1234"); err == nil {
		t.Fatal("CreateKid with Err: got nil, want error")
	}
	if len(f2.Kids()) != 0 {
		t.Errorf("Kids with Err = %d, want 0", len(f2.Kids()))
	}
}

func TestFakeEpisodeLookupBySourceRoundTrip(t *testing.T) {
	ctx := context.Background()
	pub := &FakePublisher{}
	key := Source{Kind: SourceFolder, Path: "/media/Bluey (2018)/S01E01.mp4", Title: "Magic Xylophone", Show: "Bluey (2018)"}
	if err := pub.Publish(ctx, Episode{
		ID: "ep-1", ShowID: "show-1", Title: "Magic Xylophone",
		Source: Source{Kind: SourceFolder, Path: key.Path, Title: key.Title, Show: key.Show},
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	lookup := NewFakeEpisodeLookup(pub)
	// The source the publisher filed as provenance round-trips to the
	// episode id, the same resolution migration performs for every legacy
	// "show/file" key.
	ref, found, err := lookup.BySource(ctx, key)
	if err != nil || !found || ref.Episode != "ep-1" || ref.Show != "show-1" {
		t.Fatalf("BySource = %+v found %v err %v; want ep-1/show-1", ref, found, err)
	}
	if _, found, err := lookup.BySource(ctx, Source{Kind: SourceFolder, Path: "/media/Unknown Show/ep.mp4"}); err != nil || found {
		t.Fatalf("BySource unknown key: found %v err %v; want not found", found, err)
	}
	// The show id for SetBin comes from BySource: migrate resolves each
	// legacy show/file key and bins the show it gets back.
	if err := lookup.SetBin(ctx, ref.Show, "fun"); err != nil {
		t.Fatalf("SetBin: %v", err)
	}
	if got := lookup.Lookups(); len(got) != 2 {
		t.Errorf("Lookups len = %d, want 2", len(got))
	}
	if got := lookup.Bins(); len(got) != 1 || got[0].ShowID != "show-1" || got[0].Bin != "fun" {
		t.Errorf("Bins = %+v, want show-1/fun", got)
	}
	bad := &FakeEpisodeLookup{Err: errors.New("store down")}
	if _, _, err := bad.BySource(ctx, key); err == nil {
		t.Fatal("BySource with Err: got nil, want error")
	}
	if len(bad.Lookups()) != 0 {
		t.Errorf("Lookups with Err = %d, want 0", len(bad.Lookups()))
	}
}

func TestFakeBudgetImporterRecordsAccounts(t *testing.T) {
	f := &FakeBudgetImporter{}
	ctx := context.Background()
	if err := f.ImportAccount(ctx, "kid-1", 3.5, 12, "2026-10-25"); err != nil {
		t.Fatalf("ImportAccount: %v", err)
	}
	if err := f.ImportAccount(ctx, "kid-2", 0, 0.5, "2026-10-26"); err != nil {
		t.Fatalf("ImportAccount: %v", err)
	}
	got := f.Accounts()
	if len(got) != 2 {
		t.Fatalf("Accounts len = %d, want 2", len(got))
	}
	if got[0].Kid != "kid-1" || got[0].Today != 3.5 || got[0].Saved != 12 || got[0].Day != "2026-10-25" {
		t.Errorf("account 1 = %+v, want kid-1 today 3.5 saved 12 day 2026-10-25", got[0])
	}
	f2 := &FakeBudgetImporter{Err: errors.New("store down")}
	if err := f2.ImportAccount(ctx, "kid-1", 1, 1, "2026-10-26"); err == nil {
		t.Fatal("ImportAccount with Err: got nil, want error")
	}
	if len(f2.Accounts()) != 0 {
		t.Errorf("Accounts with Err = %d, want 0", len(f2.Accounts()))
	}
}

func TestFakePlaybackImporterRecords(t *testing.T) {
	f := &FakePlaybackImporter{}
	ctx := context.Background()
	at := time.Date(2026, 10, 25, 18, 30, 0, 0, time.FixedZone("ET", -4*3600))
	if err := f.ImportResume(ctx, "kid-1", "ep-1", 421.5, at); err != nil {
		t.Fatalf("ImportResume: %v", err)
	}
	if err := f.ImportWatch(ctx, "kid-1", "ep-1", at, "Magic Xylophone", 7.5, false); err != nil {
		t.Fatalf("ImportWatch: %v", err)
	}
	if err := f.ImportWatch(ctx, "", "ep-2", at.Add(5*time.Minute), "The Grand Tour", 0, true); err != nil {
		t.Fatalf("ImportWatch free: %v", err)
	}
	if err := f.ImportHouseholdRecent(ctx, "ep-1", at, "kid-1"); err != nil {
		t.Fatalf("ImportHouseholdRecent: %v", err)
	}
	if err := f.ImportHouseholdRecent(ctx, "ep-2", at.Add(5*time.Minute), ""); err != nil {
		t.Fatalf("ImportHouseholdRecent unknown kid: %v", err)
	}
	resumes := f.Resumes()
	if len(resumes) != 1 || resumes[0].PosSec != 421.5 || !resumes[0].Updated.Equal(at) {
		t.Errorf("Resumes = %+v, want kid-1/ep-1 at 421.5s", resumes)
	}
	if watches := f.Watches(); len(watches) != 2 ||
		watches[0].Title != "Magic Xylophone" || watches[0].Ep != "ep-1" ||
		watches[0].Cost != 7.5 || watches[0].Free ||
		watches[1].Kid != "" || watches[1].Cost != 0 || !watches[1].Free {
		t.Errorf("Watches = %+v, want both watch entries with cost/free", watches)
	}
	// The household recent list keeps call order (most recent first, as the
	// legacy portal kept it) and tolerates entries no kid was matched to.
	if recents := f.Recents(); len(recents) != 2 || recents[0].Ep != "ep-1" || recents[0].Kid != "kid-1" ||
		recents[1].Ep != "ep-2" || recents[1].Kid != "" {
		t.Errorf("Recents = %+v, want kid-1/ep-1 then (no kid)/ep-2", recents)
	}
	f2 := &FakePlaybackImporter{Err: errors.New("store down")}
	if err := f2.ImportResume(ctx, "kid-1", "ep-1", 1, at); err == nil {
		t.Fatal("ImportResume with Err: got nil, want error")
	}
	if len(f2.Resumes()) != 0 {
		t.Errorf("Resumes with Err = %d, want 0", len(f2.Resumes()))
	}
}

func TestFakePracticeImporterRecords(t *testing.T) {
	f := &FakePracticeImporter{}
	ctx := context.Background()
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.FixedZone("ET", -4*3600))

	if err := f.ImportFactStats(ctx, "kid-1", "mul", "3x4", FactStats{
		Asked: 10, Correct: 8, Times: []int{2100, 1800, 1950},
		MedianMS: new(1950.0), BestMS: new(1800.0),
	}); err != nil {
		t.Fatalf("ImportFactStats: %v", err)
	}
	if err := f.ImportSessions(ctx, "kid-1", Session{At: at, Mode: "mul", N: 10, Correct: 8, MedianMS: new(1950.0)}); err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	if err := f.ImportAwards(ctx, "kid-1", Awards{Day: "2026-10-25", Earned: 2.5, Coins: 7}); err != nil {
		t.Fatalf("ImportAwards: %v", err)
	}
	if err := f.ImportFlags(ctx, "kid-1", Flag{Key: "promo_bonus:k", Value: true}); err != nil {
		t.Fatalf("ImportFlags: %v", err)
	}
	if err := f.ImportAwardLog(ctx, AwardLogEntry{At: at, Kid: "alex", Kind: "jar", Amount: 1}); err != nil {
		t.Fatalf("ImportAwardLog: %v", err)
	}
	if err := f.ImportSpelling(ctx, "kid-1", Spelling{
		Records:  []SpellingRecord{{Word: "cat", Hits: 3, Misses: 1, BestMS: 900}},
		Sessions: []SpellingSession{{At: at, Word: "cat", Correct: true, Ms: 900}},
		Awards:   Awards{Day: "2026-10-25", Earned: 1.5, Coins: 3},
	}); err != nil {
		t.Fatalf("ImportSpelling: %v", err)
	}
	if err := f.ImportBosses(ctx, "kid-1", BossState{
		Defeated: []string{"adder"}, HP: map[string]int{"wyrm": 5},
	}); err != nil {
		t.Fatalf("ImportBosses: %v", err)
	}
	if err := f.ImportConfig(ctx, Config{
		Global: map[string]float64{"award_daily_cap": 3},
		Kids:   map[string]map[string]float64{"alex": {"award_daily_cap": 4}},
	}); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}

	stats := f.FactStats()
	if len(stats) != 1 || stats[0].Mode != "mul" || stats[0].Key != "3x4" ||
		len(stats[0].Stats.Times) != 3 || stats[0].Stats.BestMS == nil || *stats[0].Stats.BestMS != 1800 ||
		stats[0].Stats.MedianMS == nil || *stats[0].Stats.MedianMS != 1950 {
		t.Errorf("FactStats = %+v, want the fact import", stats)
	}
	// Mutating a returned copy must not corrupt the recorded state: the
	// Times slice, median and best pointers are all deep-copied.
	stats[0].Stats.Times[0] = 0
	*stats[0].Stats.MedianMS = 0
	*stats[0].Stats.BestMS = 0
	got := f.FactStats()[0].Stats
	if got.Times[0] != 2100 || got.MedianMS == nil || *got.MedianMS != 1950 ||
		got.BestMS == nil || *got.BestMS != 1800 {
		t.Errorf("FactStats copy shares the recorded state; got %+v", got)
	}
	if sessions := f.Sessions(); len(sessions) != 1 || sessions[0].Session.Mode != "mul" || sessions[0].Session.Checkup ||
		sessions[0].Session.MedianMS == nil || *sessions[0].Session.MedianMS != 1950 {
		t.Errorf("Sessions = %+v, want the session", sessions)
	}
	if awards := f.Awards(); len(awards) != 1 || awards[0].Awards.Coins != 7 || awards[0].Awards.Earned != 2.5 {
		t.Errorf("Awards = %+v, want coins 7 earned 2.5", awards)
	}
	if flags := f.Flags(); len(flags) != 1 || flags[0].Flag.Value != true {
		t.Errorf("Flags = %+v, want the promo flag", flags)
	}
	if log := f.AwardLog(); len(log) != 1 || log[0].Entry.Kind != "jar" || log[0].Entry.Amount != 1 {
		t.Errorf("AwardLog = %+v, want the award entry", log)
	}
	sp := f.Spelling()
	if len(sp) != 1 || len(sp[0].Spelling.Records) != 1 || sp[0].Spelling.Records[0].Word != "cat" ||
		sp[0].Spelling.Awards.Coins != 3 {
		t.Errorf("Spelling = %+v, want records+sessions+spell awards", sp)
	}
	if bosses := f.Bosses(); len(bosses) != 1 || len(bosses[0].Boss.Defeated) != 1 || bosses[0].Boss.HP["wyrm"] != 5 {
		t.Errorf("Bosses = %+v, want the boss progress", bosses)
	}
	if cfg := f.Configs(); len(cfg) != 1 || cfg[0].Config.Global["award_daily_cap"] != 3 ||
		cfg[0].Config.Kids["alex"]["award_daily_cap"] != 4 {
		t.Errorf("Configs = %+v, want global+kid overrides", cfg)
	}

	f2 := &FakePracticeImporter{Err: errors.New("store down")}
	if err := f2.ImportFactStats(ctx, "kid-1", "add", "2+2", FactStats{}); err == nil {
		t.Fatal("ImportFactStats with Err: got nil, want error")
	}
	if len(f2.FactStats()) != 0 {
		t.Errorf("FactStats with Err = %d, want 0", len(f2.FactStats()))
	}
}

// TestFakePracticeImporterDeepCopiesInputs verifies the importer fakes do
// not retain caller-owned slices, maps or pointers: mutating an input after
// the call must not change the recorded value.
func TestFakePracticeImporterDeepCopiesInputs(t *testing.T) {
	f := &FakePracticeImporter{}
	ctx := context.Background()

	times := []int{2100, 1800, 1950}
	med := new(1950.0)
	best := new(1800.0)
	if err := f.ImportFactStats(ctx, "kid-1", "mul", "3x4", FactStats{
		Asked: 10, Correct: 8, Times: times, MedianMS: med, BestMS: best,
	}); err != nil {
		t.Fatalf("ImportFactStats: %v", err)
	}
	times[0] = 0
	*med = 999
	*best = 999

	sessMed := new(1950.0)
	if err := f.ImportSessions(ctx, "kid-1", Session{Mode: "mul", N: 10, Correct: 8, MedianMS: sessMed}); err != nil {
		t.Fatalf("ImportSessions: %v", err)
	}
	*sessMed = 999
	if got := f.Sessions()[0].Session.MedianMS; got == nil || *got != 1950 {
		t.Errorf("recorded session median changed with the caller's input: %v", got)
	}

	records := []SpellingRecord{{Word: "cat", Hits: 3, Misses: 1, BestMS: 900}}
	sessionEntries := []SpellingSession{{At: time.Now(), Word: "cat", Correct: true, Ms: 900}}
	if err := f.ImportSpelling(ctx, "kid-1", Spelling{Records: records, Sessions: sessionEntries}); err != nil {
		t.Fatalf("ImportSpelling: %v", err)
	}
	records[0].BestMS = 999
	sessionEntries[0].Ms = 999

	pending := "adder"
	defeated := []string{"adder"}
	hp := map[string]int{"wyrm": 5}
	php := map[string]int{"wyrm": 2}
	if err := f.ImportBosses(ctx, "kid-1", BossState{Defeated: defeated, HP: hp, PHP: php, PendingBid: &pending}); err != nil {
		t.Fatalf("ImportBosses: %v", err)
	}
	defeated[0] = "wyrm"
	hp["wyrm"] = 99
	php["wyrm"] = 99
	pending = "other"

	global := map[string]float64{"award_daily_cap": 3}
	kids := map[string]map[string]float64{"alex": {"award_daily_cap": 4}}
	if err := f.ImportConfig(ctx, Config{Global: global, Kids: kids}); err != nil {
		t.Fatalf("ImportConfig: %v", err)
	}
	global["award_daily_cap"] = 99
	kids["alex"]["award_daily_cap"] = 99

	stats := f.FactStats()
	if len(stats) != 1 || stats[0].Stats.Times[0] != 2100 ||
		stats[0].Stats.MedianMS == nil || *stats[0].Stats.MedianMS != 1950 ||
		stats[0].Stats.BestMS == nil || *stats[0].Stats.BestMS != 1800 {
		t.Errorf("FactStats kept caller-owned inputs; got %+v", stats[0].Stats)
	}
	sp := f.Spelling()
	if len(sp) != 1 || sp[0].Spelling.Records[0].BestMS != 900 || sp[0].Spelling.Sessions[0].Ms != 900 {
		t.Errorf("Spelling kept the caller's slices; got %+v", sp[0].Spelling)
	}
	bosses := f.Bosses()
	if len(bosses) != 1 || bosses[0].Boss.Defeated[0] != "adder" || bosses[0].Boss.HP["wyrm"] != 5 ||
		bosses[0].Boss.PHP["wyrm"] != 2 || bosses[0].Boss.PendingBid == nil || *bosses[0].Boss.PendingBid != "adder" {
		t.Errorf("Bosses kept the caller's slices/maps; got %+v", bosses[0].Boss)
	}
	cfg := f.Configs()
	if len(cfg) != 1 || cfg[0].Config.Global["award_daily_cap"] != 3 || cfg[0].Config.Kids["alex"]["award_daily_cap"] != 4 {
		t.Errorf("Configs kept the caller's maps; got %+v", cfg[0].Config)
	}
}
