package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/edementhon/kids-shelf/internal/disk"
	"github.com/edementhon/kids-shelf/internal/jobs"
	"github.com/edementhon/kids-shelf/internal/store"
)

// TestGoodReleaseStagesAndJournals is the app-side happy path: the release
// is verified and extracted to versions/<v>, and the journal says staged.
func TestGoodReleaseStagesAndJournals(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	_, _, _ = stageGood(t, dataDir, install)

	bin, err := os.ReadFile(VersionBinary(install, testTag))
	if err != nil {
		t.Fatalf("versioned binary missing: %v", err)
	}
	if string(bin) != string(releaseBytes(t)) {
		t.Error("installed binary differs from the release binary")
	}
	if _, err := os.Stat(VersionDir(install, testTag) + ".tmp"); !isNotExist(err) {
		t.Error("staging temp directory left behind")
	}

	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Staged != testTag {
		t.Errorf("journal staged = %q, want %s", st.Staged, testTag)
	}
	if st.Trying != nil || st.Current != "" || st.HighWater != "" {
		t.Errorf("journal = %+v; activation must not have started yet", st)
	}
}

// TestBadChecksumLeavesEverythingUnchanged: a zip whose manifest entry does
// not match its hash must fail before anything is staged.
func TestBadChecksumLeavesEverythingUnchanged(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, _ := makeReleaseZip(t)
	// A manifest that promises a different hash than the zip has.
	sums := makeSums(testTag, strings.Repeat("0", 64))
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	m := newTestManager(t, dataDir, install, srv.metaURL())

	err := m.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Update = %v, want a checksum error", err)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "" {
		t.Errorf("staged = %q after a bad checksum", st.Staged)
	}
	if _, serr := os.Stat(VersionDir(install, testTag)); !isNotExist(serr) {
		t.Error("a version directory appeared for the failed release")
	}
	if got := m.Status().State; got != StateFailed {
		t.Errorf("status = %q, want failed", got)
	}
	if msg := m.Status().Message; msg != "the download did not match its checksum" {
		t.Errorf("status message = %q", msg)
	}
}

// TestBadSignatureLeavesEverythingUnchanged: the manifest signed with a
// different key must fail before the zip is even downloaded.
func TestBadSignatureLeavesEverythingUnchanged(t *testing.T) {
	_ = newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	// Sign with a key that is not the compiled-in one.
	_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	srv := newFakeServer(t, wrongPriv, testTag, zipB, sums)
	m := newTestManager(t, dataDir, install, srv.metaURL())

	err = m.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("Update = %v, want a signature error", err)
	}
	if got := m.Status().Message; got != "the release is not signed with the expected key" {
		t.Errorf("status message = %q", got)
	}
	if n := srv.hitsFor("/assets/" + ZipAssetName()); n != 0 {
		t.Errorf("the zip asset was downloaded %d times despite the bad signature", n)
	}
	if _, serr := os.Stat(VersionDir(install, testTag)); !isNotExist(serr) {
		t.Error("a version directory appeared for the unsigned release")
	}
}

// TestEmptyKeyTurnsUpdatesOff: no compiled-in key means updates are off and
// nothing is fetched.
func TestEmptyKeyTurnsUpdatesOff(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	priv := newTestKey(t)
	srv := newFakeServer(t, priv, testTag, zipB, makeSums(testTag, zipSHA))

	old := PublicKeyHex
	PublicKeyHex = ""
	t.Cleanup(func() { PublicKeyHex = old })

	m := newTestManager(t, dataDir, install, srv.metaURL())
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update with an empty key = %v, want nil", err)
	}
	st := m.Status()
	if st.State != StateOff {
		t.Errorf("status = %+v, want off", st)
	}
	if !strings.Contains(st.Message, "off") {
		t.Errorf("status message %q does not say updates are off", st.Message)
	}
	if n := srv.hitsFor("latest"); n != 0 {
		t.Errorf("the release endpoint was hit %d times despite updates being off", n)
	}
	st2, _ := ReadState(dataDir)
	if st2.Staged != "" {
		t.Error("something was staged despite the empty key")
	}
}

// TestOlderSignedReleaseRefused: a release at or below the high-water mark
// is not staged.
func TestOlderSignedReleaseRefused(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)

	// The journal says v2.0.0 already ran (high-water and current).
	st := State{Current: testTag, HighWater: testTag}
	if err := writeState(dataDir, st); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := ReadState(dataDir)
	if got.Staged != "" {
		t.Error("an older or equal release was staged")
	}
	if _, serr := os.Stat(VersionDir(install, testTag)); !isNotExist(serr) {
		t.Error("a version directory appeared for the refused release")
	}
	if m.Status().State != StateIdle {
		t.Errorf("status = %+v, want idle (already up to date)", m.Status())
	}
}

// TestRejectedVersionNeverRetried: a version that failed its trial is not
// tried again, while a newer version still is.
func TestRejectedVersionNeverRetried(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)

	st := State{Current: testOldTag, HighWater: testOldTag, Rejected: []string{testTag}}
	if err := writeState(dataDir, st); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := ReadState(dataDir)
	if got.Staged != "" {
		t.Error("the rejected version was staged again")
	}

	// A newer release is not rejected and goes through; the manifest is
	// re-signed for the new tag (a relabelled release would be refused).
	srv.setTag("v2.0.1")
	srv.setSums(makeSums("v2.0.1", zipSHA))
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update of the newer release: %v", err)
	}
	got, _ = ReadState(dataDir)
	if got.Staged != "v2.0.1" {
		t.Errorf("staged = %q, want v2.0.1", got.Staged)
	}
}

// TestOversizedAssetRefused: an asset announced above the cap is refused
// before any download.
func TestOversizedAssetRefused(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	srv.setSize(MaxDownloadBytes + 1)

	m := newTestManager(t, dataDir, install, srv.metaURL())
	err := m.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "larger") {
		t.Fatalf("Update = %v, want an oversized error", err)
	}
	if n := srv.hitsFor("/assets/" + ZipAssetName()); n != 0 {
		t.Errorf("the zip asset was downloaded %d times despite the size", n)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "" {
		t.Error("the oversized release was staged")
	}
	if msg := m.Status().Message; msg != "the release asset is larger than the 300 MB limit" {
		t.Errorf("status message = %q", msg)
	}
}

// TestReadOnlyInstallDirReportsFailure: with an unwritable install the
// update fails into Status, and the manager keeps working afterwards.
func TestReadOnlyInstallDirReportsFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directories do not block root")
	}
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	if err := os.Chmod(install, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(install, 0o755) })

	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	m := newTestManager(t, dataDir, install, srv.metaURL())

	if err := m.Update(context.Background()); err == nil {
		t.Fatal("Update into a read-only install succeeded")
	}
	st := m.Status()
	if st.State != StateFailed {
		t.Errorf("status = %+v, want failed", st)
	}
	if st.Message == "" || strings.HasPrefix(st.Message, "update:") {
		t.Errorf("status message %q is not plain words", st.Message)
	}

	// The manager is still usable: after the install becomes writable the
	// very same check stages the release.
	os.Chmod(install, 0o755)
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update after the install became writable: %v", err)
	}
	if m.Status().State != StateStaged {
		t.Errorf("status = %+v, want staged", m.Status())
	}
}

// TestManagerSkipsWhileTryingAndStaged: the app side never stages over an
// update the launcher is still settling.
func TestManagerSkipsWhileTryingAndStaged(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums("v2.0.1", zipSHA)
	srv := newFakeServer(t, priv, "v2.0.1", zipB, sums)

	if err := writeState(dataDir, State{Trying: &Trial{Version: testTag, Snapshot: "pre.db"}}); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update while trying: %v", err)
	}
	got, _ := ReadState(dataDir)
	if got.Staged != "" {
		t.Error("staged on top of an unsettled trial")
	}
	if m.Status().State != StateStaged {
		t.Errorf("status during a trial = %+v, want staged", m.Status())
	}
	// The launcher settles the trial without telling the app: the status
	// follows the journal, confirmed (up to date) or rolled back (failed).
	if err := writeState(dataDir, State{Current: testTag, Previous: "v1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.State != StateIdle || !strings.Contains(s.Message, "up to date") {
		t.Errorf("status after the trial was confirmed = %+v, want idle and up to date", s)
	}
	if err := writeState(dataDir, State{Current: "v1.0.0", Rejected: []string{testTag}}); err != nil {
		t.Fatal(err)
	}
	if s := m.Status(); s.State != StateFailed || !strings.Contains(s.Message, "went back to v1.0.0") {
		t.Errorf("status after the trial was rolled back = %+v, want failed naming v1.0.0", s)
	}

	if err := writeState(dataDir, State{Staged: testTag}); err != nil {
		t.Fatal(err)
	}
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update while staged: %v", err)
	}
	got, _ = ReadState(dataDir)
	if got.Staged != testTag {
		t.Errorf("staged = %q, want %q unchanged", got.Staged, testTag)
	}
}

// TestStatusCheckingTransition: during a check the status says checking.
func TestStatusCheckingTransition(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	block := make(chan struct{})
	srv.latestSlow = func() { <-block }
	m := newTestManager(t, dataDir, install, srv.metaURL())
	done := make(chan error, 1)
	go func() { done <- m.Update(context.Background()) }()
	var openOnce sync.Once
	openBlock := func() { openOnce.Do(func() { close(block) }) }
	defer openBlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if m.Status().State == StateChecking {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %+v, want checking while the check is in flight", m.Status())
		}
		select {
		case err := <-done:
			t.Fatalf("Update finished before the server responded: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := m.Status().State; got != StateChecking {
		t.Errorf("status during the check = %q, want checking", got)
	}
	openBlock()
	if err := <-done; err != nil {
		t.Fatalf("Update: %v", err)
	}
}

// TestManagerStatusSurvivesRepeatedFailures: consecutive failures keep the
// manager usable (the app never stops serving because of them).
func TestManagerStatusSurvivesRepeatedFailures(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	m := newTestManager(t, dataDir, install, srv.metaURL())
	for i := range 3 {
		if err := m.Update(context.Background()); err != nil {
			t.Fatalf("Update %d: %v", i, err)
		}
		if m.Status().State != StateStaged {
			t.Fatalf("Update %d status = %+v, want staged", i, m.Status())
		}
	}
}

// TestRelabelledReleaseRefused: the manifest is signed for v1.0.0 while the
// release is tagged v2.0.0 - a relabelled old release must be refused even
// though the tag looks newer, and the zip must not even be downloaded.
func TestRelabelledReleaseRefused(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	sums := makeSums(testOldTag, zipSHA)
	srv := newFakeServer(t, priv, testTag, zipB, sums)
	m := newTestManager(t, dataDir, install, srv.metaURL())

	err := m.Update(context.Background())
	if err == nil {
		t.Fatal("Update accepted a relabelled release")
	}
	if !strings.Contains(err.Error(), "signed version") {
		t.Fatalf("Update = %v, want a signed-version mismatch error", err)
	}
	if n := srv.hitsFor("/assets/" + ZipAssetName()); n != 0 {
		t.Errorf("the zip asset was downloaded %d times despite the relabel", n)
	}
	if msg := m.Status().Message; msg != "the release's signed version does not match its tag" {
		t.Errorf("status message = %q", msg)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "" {
		t.Error("the relabelled release was staged")
	}
}

// TestVersionLineMissingRefused: a SHA256SUMS file without the `version:`
// first line (the pre-digest format) is refused before anything downloads.
func TestVersionLineMissingRefused(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	// The old format: checksum lines only, no version line.
	legacy := []byte(fmt.Sprintf("%s  %s\n", zipSHA, ZipAssetName()))
	srv := newFakeServer(t, priv, testTag, zipB, legacy)
	m := newTestManager(t, dataDir, install, srv.metaURL())

	err := m.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("Update = %v, want a missing-version error", err)
	}
	st, _ := ReadState(dataDir)
	if st.Staged != "" {
		t.Error("a manifest without a signed version was staged")
	}
}

// TestFirstRunSeedsHighWaterFromBuild: on a fresh data dir (no journal)
// the running build's version becomes the high-water mark, and a release
// above it still stages.
func TestFirstRunSeedsHighWaterFromBuild(t *testing.T) {
	old := BuildVersion
	BuildVersion = testTag // v2.0.0
	t.Cleanup(func() { BuildVersion = old })
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, "v2.0.1", zipB, makeSums("v2.0.1", zipSHA))
	m := newTestManager(t, dataDir, install, srv.metaURL())

	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st, _ := ReadState(dataDir)
	if st.HighWater != testTag {
		t.Errorf("high_water = %q, want the build's %s", st.HighWater, testTag)
	}
	if st.Staged != "v2.0.1" {
		t.Errorf("staged = %q, want v2.0.1 (a release above the seeded mark stages)", st.Staged)
	}
}

// TestFreshInstallRefusesReleaseOlderThanBuild: on a fresh install whose
// build is v2.0.0, a correctly signed v1.0.0 release is refused - the seed
// makes the built version the floor, so a fresh install can never
// downgrade.
func TestFreshInstallRefusesReleaseOlderThanBuild(t *testing.T) {
	old := BuildVersion
	BuildVersion = testTag // v2.0.0
	t.Cleanup(func() { BuildVersion = old })
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testOldTag, zipB, makeSums(testOldTag, zipSHA))
	m := newTestManager(t, dataDir, install, srv.metaURL())

	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st, _ := ReadState(dataDir)
	if st.HighWater != testTag {
		t.Errorf("high_water = %q, want the build's %s", st.HighWater, testTag)
	}
	if st.Staged != "" {
		t.Error("a signed release older than the running build was staged")
	}
	if m.Status().State != StateIdle {
		t.Errorf("status = %+v, want idle", m.Status())
	}
}

// TestStageRefusesSignedVersionNotAboveHighWater: even a directly staged
// release whose signed version is at or below the high-water mark is
// refused (the authoritative check is on the signed version, not the tag).
func TestStageRefusesSignedVersionNotAboveHighWater(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testOldTag, zipB, makeSums(testOldTag, zipSHA))
	if err := writeState(dataDir, State{Current: testTag, HighWater: testTag}); err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	rel := &Release{
		Tag:     testOldTag,
		ZipURL:  srv.URL + "/assets/" + ZipAssetName(),
		ZipSize: int64(len(zipB)),
		SumURL:  srv.URL + "/assets/SHA256SUMS",
		SigURL:  srv.URL + "/assets/SHA256SUMS.sig",
	}
	err := m.Stage(context.Background(), rel)
	if err == nil || !strings.Contains(err.Error(), "not newer") {
		t.Fatalf("Stage = %v, want a not-newer error", err)
	}
	if msg := statusMessage(err); msg != "the release is not newer than the version already installed" {
		t.Errorf("status message = %q", msg)
	}
}

// TestReservationCoversZipExtractedAndSnapshot: the reservations before a
// download cover the zip asset plus the whole extraction cap (install
// filesystem) and the current database size (data directory filesystem, the
// activation snapshot the launcher takes on restart); the stage one is
// released when the stage finishes, the snapshot one is kept for the
// launcher.
func TestReservationCoversZipExtractedAndSnapshot(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testTag, zipB, makeSums(testTag, zipSHA))

	rec := &recordingStore{byJob: map[string]uint64{}}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	m.Reservations = disk.NewReservations(rec).
		WithFree(func(string) (uint64, error) { return 1 << 40, nil }).
		WithFloor(1 << 20)

	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fi, err := os.Stat(DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	wantStage := uint64(len(zipB)) + MaxExtractedBytes
	if got := rec.getPeak(stageReservationID(testTag)); got != wantStage {
		t.Errorf("stage reservation = %d, want %d (zip %d + extracted cap %d)", got, wantStage, len(zipB), MaxExtractedBytes)
	}
	if got := rec.getPeak(snapshotReservationID(testTag)); got != uint64(fi.Size()) {
		t.Errorf("snapshot reservation = %d, want the database size %d", got, fi.Size())
	}
}

// TestReservationMeasuredPerFilesystem: the stage reservation is checked
// against the install directory and the snapshot reservation against the
// data directory, each with its own free-space probe, and the stage one is
// released at the end of the stage while the snapshot one is kept for the
// launcher to release at activation or rejection.
func TestReservationMeasuredPerFilesystem(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testTag, zipB, makeSums(testTag, zipSHA))

	rec := &recordingStore{byJob: map[string]uint64{}}
	var probed []string
	m := newTestManager(t, dataDir, install, srv.metaURL())
	m.Reservations = disk.NewReservations(rec).
		WithFree(func(path string) (uint64, error) {
			probed = append(probed, path)
			return 1 << 40, nil
		}).
		WithFloor(1 << 20)

	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(probed) != 2 || probed[0] != install || probed[1] != dataDir {
		t.Errorf("reservation probes %v, want [install %s, data dir %s]", probed, install, dataDir)
	}
	if got := rec.get(stageReservationID(testTag)); got != 0 {
		t.Errorf("stage reservation after the stage = %d, want 0 (released)", got)
	}
	fi, err := os.Stat(DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.get(snapshotReservationID(testTag)); got != uint64(fi.Size()) {
		t.Errorf("snapshot reservation after the stage = %d, want %d (kept until activation)", got, fi.Size())
	}
}

// TestReservationRefusedWhenDataDirShort: a data directory whose free space
// cannot cover the snapshot reservation (even though the install directory
// has plenty) refuses the stage - neither filesystem's floor check can be
// skipped.
func TestReservationRefusedWhenDataDirShort(t *testing.T) {
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testTag, zipB, makeSums(testTag, zipSHA))

	fi, err := os.Stat(DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	// dataDir has exactly the floor plus one byte less than the database:
	// the snapshot reservation must be refused there.
	rec := &recordingStore{byJob: map[string]uint64{}}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	m.Free = func(path string) (uint64, error) {
		if path == dataDir {
			return (1 << 20) + uint64(fi.Size()) - 1, nil
		}
		return 1 << 40, nil
	}
	m.Reservations = disk.NewReservations(rec).
		WithFree(func(path string) (uint64, error) { return m.Free(path) }).
		WithFloor(1 << 20)

	err = m.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("Update with a short data dir = %v, want a no-space error", err)
	}
	if _, err := os.Stat(VersionBinary(install, testTag)); !os.IsNotExist(err) {
		t.Error("the version was installed although the reservation was refused")
	}
}

// TestStageCleansTempDirs: the staging directory under the install root is
// removed whether the stage succeeds or fails (here: a checksum failure).
func TestStageCleansTempDirs(t *testing.T) {
	stageTempDirs := func(t *testing.T, install string) []string {
		t.Helper()
		left, err := filepath.Glob(filepath.Join(install, ".update-*"))
		if err != nil {
			t.Fatal(err)
		}
		return left
	}
	priv := newTestKey(t)

	// Success path.
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testTag, zipB, makeSums(testTag, zipSHA))
	m := newTestManager(t, dataDir, install, srv.metaURL())
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if left := stageTempDirs(t, install); len(left) != 0 {
		t.Errorf("staging dirs left after a successful stage: %v", left)
	}

	// Checksum failure path: the manifest verifies but the zip hash does
	// not match, so the failure happens after the download.
	badZipSHA := strings.Repeat("0", 64)
	srv2 := newFakeServer(t, priv, "v2.0.1", zipB, makeSums("v2.0.1", badZipSHA))
	install2 := setupInstall(t)
	dataDir2 := t.TempDir()
	seedDB(t, dataDir2)
	m2 := newTestManager(t, dataDir2, install2, srv2.metaURL())
	if err := m2.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Update with a bad checksum = %v, want a checksum error", err)
	}
	if left := stageTempDirs(t, install2); len(left) != 0 {
		t.Errorf("staging dirs left after a failed stage: %v", left)
	}
}
func TestFailedStageReleasesSnapshotReservation(t *testing.T) {
	// The snapshot reservation is kept only after successful staging: a
	// checksum failure (which happens after the download) must release
	// every hold again, leaving zero bytes reserved in the shared book.
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	zipB, _ := makeReleaseZip(t)
	badSHA := strings.Repeat("0", 64)
	srv := newFakeServer(t, priv, "v2.0.1", zipB, makeSums("v2.0.1", badSHA))

	s, err := store.Open(DatabasePath(dataDir))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	q, err := jobs.New(s)
	if err != nil {
		t.Fatal(err)
	}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	m.Reservations = disk.NewReservations(q).
		WithFree(func(string) (uint64, error) { return 1 << 40, nil }).
		WithFloor(1 << 20)

	if err := m.Update(context.Background()); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("Update with a bad checksum = %v, want a checksum error", err)
	}
	sum, err := q.SumActiveReservedBytes("")
	if err != nil {
		t.Fatal(err)
	}
	if sum != 0 {
		t.Errorf("reserved bytes after a failed stage = %d, want 0 (every hold released)", sum)
	}
}

func TestSnapshotReservationVisibleToQueueAfterReopen(t *testing.T) {
	// The production wiring hands the updater the app's queue-backed book
	// (app.Reservations() over the jobs queue). A snapshot reservation
	// made by Stage must be visible to the queue, survive a reopen (the
	// restart, where the launcher re-checks it) and count against later
	// reservations on the same filesystem.
	priv := newTestKey(t)
	install := setupInstall(t)
	dataDir := t.TempDir()
	seedDB(t, dataDir)
	zipB, zipSHA := makeReleaseZip(t)
	srv := newFakeServer(t, priv, testTag, zipB, makeSums(testTag, zipSHA))

	dbPath := DatabasePath(dataDir)
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	q, err := jobs.New(s)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	m := newTestManager(t, dataDir, install, srv.metaURL())
	m.Reservations = disk.NewReservations(q).
		WithFree(func(string) (uint64, error) { return 1 << 40, nil }).
		WithFloor(1 << 20)
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fi, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := q.SumActiveReservedBytes("")
	if err != nil {
		t.Fatal(err)
	}
	// Only the snapshot hold remains: the stage hold was released.
	if sum != uint64(fi.Size()) {
		t.Fatalf("reserved after staging = %d, want the database size %d", sum, fi.Size())
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopening the store (the restart) finds the hold still in force.
	s2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	q2, err := jobs.New(s2)
	if err != nil {
		t.Fatal(err)
	}
	sum, err = q2.SumActiveReservedBytes("")
	if err != nil {
		t.Fatal(err)
	}
	if sum != uint64(fi.Size()) {
		t.Fatalf("reserved after reopen = %d, want %d (the snapshot hold)", sum, fi.Size())
	}
	// It counts against a later reservation on the same filesystem: with
	// floor 1<<20 and free 1<<40, only free-floor-hold bytes are left.
	job, err := q2.Enqueue("ingest", nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := disk.FSKey(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	over := uint64(1<<40-1<<20) - uint64(fi.Size()) + 1
	other, ok, err := q2.TryReserve(job, over, 1<<40, 1<<20, key)
	if err != nil {
		t.Fatal(err)
	}
	if ok || other != uint64(fi.Size()) {
		t.Errorf("TryReserve over the snapshot hold = (other %d, ok %v), want (db size %d, refused)", other, ok, fi.Size())
	}
}

// TestReconcileHoldsKeepsOnlyTheStagedSnapshotHold is the crash-recovery
// contract: holds left in the shared book must be reconciled against the
// journal, so a crash after journaling "staged" but before the deferred
// release cannot leave the staging hold (update:<tag>) behind forever, and
// snapshot holds of versions the journal no longer points at stop stealing
// floor. With one staged version only its snapshot hold survives, and
// holds under other prefixes are never touched.
func TestReconcileHoldsKeepsOnlyTheStagedSnapshotHold(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Staged: testTag}); err != nil {
		t.Fatal(err)
	}

	rec := &recordingStore{byJob: map[string]uint64{}}
	m := newTestManager(t, dataDir, install, "http://unused")
	m.Reservations = disk.NewReservations(rec).
		WithFree(func(string) (uint64, error) { return 1 << 40, nil }).
		WithFloor(1 << 20)
	res := m.Reservations
	need := uint64(1 << 20)
	// The crash leftovers plus one hold that has nothing to do with the
	// updater and must not be touched.
	for _, id := range []string{
		stageReservationID(testTag),       // staged version's staging hold (crash leftover)
		stageReservationID(testOldTag),    // an older stage's hold (crash leftover)
		snapshotReservationID(testTag),    // the one the journal justifies
		snapshotReservationID(testOldTag), // a version that is not staged anymore
		"ingest:ocr-123",                  // a non-update hold
	} {
		if err := res.Hold(id, need, install); err != nil {
			t.Fatalf("Hold %s: %v", id, err)
		}
	}

	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reconcileHolds(st); err != nil {
		t.Fatalf("reconcileHolds: %v", err)
	}
	if got := rec.get(snapshotReservationID(testTag)); got != need {
		t.Errorf("snapshot hold of the staged version = %d, want %d (kept)", got, need)
	}
	for _, id := range []string{
		stageReservationID(testTag),
		stageReservationID(testOldTag),
		snapshotReservationID(testOldTag),
	} {
		if got := rec.get(id); got != 0 {
			t.Errorf("hold %s = %d after the reconcile, want 0 (released)", id, got)
		}
	}
	if got := rec.get("ingest:ocr-123"); got != need {
		t.Errorf("non-update hold = %d, want %d (untouched)", got, need)
	}

	// Running through Update (the updater-start entry) reconciles the same
	// way: idempotent, and the staged journal short-circuits before any
	// network use.
	if err := m.Update(context.Background()); err != nil {
		t.Fatalf("Update with a stale staging hold: %v", err)
	}
	if got := rec.get(snapshotReservationID(testTag)); got != need {
		t.Errorf("snapshot hold after Update = %d, want %d (still kept)", got, need)
	}
}

// TestReconcileHoldsKeepsTheTryingSnapshotHold: while a trial is in flight
// its snapshot hold still guards the rollback bytes, so the journal's
// trying version survives the reconcile like the staged one.
func TestReconcileHoldsKeepsTheTryingSnapshotHold(t *testing.T) {
	install := setupInstall(t)
	dataDir := t.TempDir()
	if err := writeState(dataDir, State{Trying: &Trial{Version: testTag}}); err != nil {
		t.Fatal(err)
	}

	rec := &recordingStore{byJob: map[string]uint64{}}
	m := newTestManager(t, dataDir, install, "http://unused")
	m.Reservations = disk.NewReservations(rec).
		WithFree(func(string) (uint64, error) { return 1 << 40, nil }).
		WithFloor(1 << 20)
	res := m.Reservations
	need := uint64(512)
	for _, id := range []string{
		snapshotReservationID(testTag),
		snapshotReservationID(testOldTag),
	} {
		if err := res.Hold(id, need, install); err != nil {
			t.Fatalf("Hold %s: %v", id, err)
		}
	}

	st, err := ReadState(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.reconcileHolds(st); err != nil {
		t.Fatalf("reconcileHolds: %v", err)
	}
	if got := rec.get(snapshotReservationID(testTag)); got != need {
		t.Errorf("snapshot hold of the trying version = %d, want %d (kept)", got, need)
	}
	if got := rec.get(snapshotReservationID(testOldTag)); got != 0 {
		t.Errorf("snapshot hold of the not-trying version = %d, want 0 (released)", got)
	}
}

// recordingStore records the bytes of every reservation, keyed by job id:
// the current value (after releases) and the last nonzero value seen. It
// ignores filesystem keys - the update tests check per-directory free-space
// probes and total bytes, not key separation (that is jobs/disk's own
// coverage).
type recordingStore struct {
	mu    sync.Mutex
	byJob map[string]uint64
	peak  map[string]uint64
}

func (r *recordingStore) get(jobID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byJob[jobID]
}

func (r *recordingStore) getPeak(jobID string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.peak[jobID]
}

func (r *recordingStore) trySet(jobID string, bytes, free, floor uint64) (uint64, bool) {
	var other uint64
	for id, b := range r.byJob {
		if id != jobID {
			other += b
		}
	}
	// free - floor, guarded against underflow, like jobs.Queue.TryReserve.
	var available uint64
	if free > floor {
		available = free - floor
	}
	if other > available || bytes > available-other {
		return other, false
	}
	r.byJob[jobID] = bytes
	if r.peak == nil {
		r.peak = map[string]uint64{}
	}
	r.peak[jobID] = bytes
	return other, true
}

func (r *recordingStore) TryReserve(jobID string, bytes, free, floor uint64, fsKey string) (uint64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	other, ok := r.trySet(jobID, bytes, free, floor)
	return other, ok, nil
}

func (r *recordingStore) TryHoldReservation(id string, bytes, free, floor uint64, fsKey string) (uint64, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	other, ok := r.trySet(id, bytes, free, floor)
	return other, ok, nil
}

func (r *recordingStore) SetReservedBytes(jobID string, bytes uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if bytes == 0 {
		delete(r.byJob, jobID)
		return nil
	}
	r.byJob[jobID] = bytes
	if r.peak == nil {
		r.peak = map[string]uint64{}
	}
	r.peak[jobID] = bytes
	return nil
}

func (r *recordingStore) HeldReservationIDs(prefix string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for id := range r.byJob {
		if strings.HasPrefix(id, prefix) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}
