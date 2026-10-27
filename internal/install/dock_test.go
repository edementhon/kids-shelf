// dock_test.go: unit tests for the pure Dock plist transform (dock.go).
// The transform is platform-free on purpose, so these tests run everywhere;
// the defaults/killall calls live in shortcuts_darwin.go. sampleDockPlist
// is a realistic `defaults export com.apple.dock -` document: a
// persistent-apps array of tile dicts whose tile-data/file-data holds
// _CFURLString with _CFURLStringType 15, a persistent-others array, and the
// other keys a real export carries.
package install

import (
	"bytes"
	"strings"
	"testing"
)

// sampleDockPlist mimics what macOS exports for a Dock with Finder, Safari
// and Google Chrome pinned and Terminal in persistent-others.
const sampleDockPlist = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>autohide</key>
	<false/>
	<key>magnification</key>
	<false/>
	<key>mod-count</key>
	<integer>260</integer>
	<key>orientation</key>
	<string>bottom</string>
	<key>persistent-apps</key>
	<array>
		<dict>
			<key>GUID</key>
			<integer>7195391495842467519</integer>
			<key>tile-data</key>
			<dict>
				<key>dock-extra</key>
				<false/>
				<key>file-data</key>
				<dict>
					<key>_CFURLString</key>
					<string>file:///System/Applications/Finder.app/</string>
					<key>_CFURLStringType</key>
					<integer>15</integer>
				</dict>
				<key>file-label</key>
				<string>Finder</string>
				<key>file-type</key>
				<integer>41</integer>
				<key>is-beta</key>
				<false/>
				<key>parent-mod-date</key>
				<date>2026-10-01T12:00:00Z</date>
			</dict>
			<key>tile-type</key>
			<string>file-tile</string>
		</dict>
		<dict>
			<key>GUID</key>
			<integer>4617450480432627715</integer>
			<key>tile-data</key>
			<dict>
				<key>file-data</key>
				<dict>
					<key>_CFURLString</key>
					<string>file:///Applications/Safari.app/</string>
					<key>_CFURLStringType</key>
					<integer>15</integer>
				</dict>
				<key>file-label</key>
				<string>Safari</string>
				<key>file-type</key>
				<integer>41</integer>
			</dict>
			<key>tile-type</key>
			<string>file-tile</string>
		</dict>
		<dict>
			<key>GUID</key>
			<integer>8616450169623724136</integer>
			<key>tile-data</key>
			<dict>
				<key>file-data</key>
				<dict>
					<key>_CFURLString</key>
					<string>file:///Applications/Google%20Chrome.app/</string>
					<key>_CFURLStringType</key>
					<integer>15</integer>
				</dict>
				<key>file-label</key>
				<string>Google Chrome</string>
				<key>file-type</key>
				<integer>41</integer>
			</dict>
			<key>tile-type</key>
			<string>file-tile</string>
		</dict>
	</array>
	<key>persistent-others</key>
	<array>
		<dict>
			<key>GUID</key>
			<integer>2127628442636237233</integer>
			<key>tile-data</key>
			<dict>
				<key>file-data</key>
				<dict>
					<key>_CFURLString</key>
					<string>file:///System/Applications/Utilities/Terminal.app/</string>
					<key>_CFURLStringType</key>
					<integer>15</integer>
				</dict>
				<key>file-label</key>
				<string>Terminal</string>
			</dict>
			<key>tile-type</key>
			<string>file-tile</string>
		</dict>
	</array>
	<key>show-recents</key>
	<true/>
	<key>tilesize</key>
	<real>59</real>
	<key>trash-full</key>
	<false/>
	<key>version</key>
	<integer>1</integer>
</dict>
</plist>
`

// dockKnownURLs are the entry bundle's file URLs at both entry locations:
// the system copy and the user copy a machine that installed before
// /Applications became the default may still have.
var dockKnownURLs = []string{
	"file:///Applications/kids-shelf.app/",
	"file:///Users/alice/Applications/kids-shelf.app/",
}

// dockAppURL is the system-location URL dockAdd pins when the account can
// write /Applications.
const dockAppURL = "file:///Applications/kids-shelf.app/"

// dockUserURL is the user-location URL, used for the "both locations are
// recognised" cases.
const dockUserURL = "file:///Users/alice/Applications/kids-shelf.app/"

// dockPersistentApps parses a transformed document and returns its
// persistent-apps array.
func dockPersistentApps(t *testing.T, doc []byte) *plistNode {
	t.Helper()
	root, err := parsePlist(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("re-parsing the transformed plist: %v", err)
	}
	dict := plistRootDict(root)
	if dict == nil {
		t.Fatal("transformed plist has no root dict")
	}
	return dictValue(dict, "persistent-apps")
}

// dockTileURLs lists the normalized URLs of an array's file tiles.
func dockTileURLs(apps *plistNode) []string {
	var urls []string
	if apps == nil {
		return urls
	}
	for _, c := range apps.children {
		if c.kind == plistElement && c.name == "dict" {
			urls = append(urls, tileURL(c))
		}
	}
	return urls
}

// hasURL reports whether list contains want.
func hasURL(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// assertOutsideArrayUntouched checks that the bytes before and after the
// persistent-apps array are identical between original and transformed.
func assertOutsideArrayUntouched(t *testing.T, original, transformed []byte) {
	t.Helper()
	start := bytes.Index(original, []byte("<key>persistent-apps</key>"))
	if start < 0 {
		t.Fatal("sample has no persistent-apps key")
	}
	rel := bytes.Index(original[start:], []byte("</array>"))
	if rel < 0 {
		t.Fatal("sample has no closing </array>")
	}
	end := start + rel + len("</array>")
	if !bytes.HasPrefix(transformed, original[:start]) {
		t.Error("the document changed before persistent-apps")
	}
	if !bytes.HasSuffix(transformed, original[end:]) {
		t.Error("the document changed after persistent-apps")
	}
}

func TestDockAddEntryAppendsTile(t *testing.T) {
	doc := []byte(sampleDockPlist)
	out, changed, err := dockPlistAddEntry(doc, dockAppURL, dockKnownURLs, 123456789)
	if err != nil {
		t.Fatalf("dockPlistAddEntry: %v", err)
	}
	if !changed {
		t.Fatal("a Dock without kids-shelf must change")
	}
	if bytes.Equal(out, doc) {
		t.Fatal("the transformed document is unchanged")
	}

	apps := dockPersistentApps(t, out)
	if !dockHasEntry(apps, dockKnownURLs) {
		t.Error("the kids-shelf tile is missing after the add")
	}
	urls := dockTileURLs(apps)
	if !hasURL(urls, "/Applications/kids-shelf.app") {
		t.Errorf("kids-shelf URL not in the array: %v", urls)
	}
	if urls[len(urls)-1] != "/Applications/kids-shelf.app" {
		t.Errorf("the new tile must be the last one, got %v", urls)
	}
	if len(urls) != 4 {
		t.Errorf("expected 4 app tiles, got %d", len(urls))
	}
	// The other tiles survive.
	for _, want := range []string{
		"/System/Applications/Finder.app",
		"/Applications/Safari.app",
		"/Applications/Google Chrome.app",
	} {
		if !hasURL(urls, want) {
			t.Errorf("tile for %s was lost", want)
		}
	}
	// The tile itself carries the Dock's file URL form and our metadata.
	for _, want := range []string{
		"<string>file:///Applications/kids-shelf.app/</string>",
		"<integer>15</integer>",
		"<string>kids-shelf</string>",
		"<string>file-tile</string>",
		"<integer>123456789</integer>",
	} {
		if !bytes.Contains(out, []byte(want)) {
			t.Errorf("transformed document lacks %s", want)
		}
	}
	// persistent-others and the other keys survive untouched.
	others := dictValue(plistRootDict(mustParse(t, out)), "persistent-others")
	if !hasURL(dockTileURLs(others), "/System/Applications/Utilities/Terminal.app") {
		t.Error("persistent-others lost the Terminal tile")
	}
	if !bytes.Contains(out, []byte("<real>59</real>")) || !bytes.Contains(out, []byte("<string>bottom</string>")) {
		t.Error("a top-level key changed")
	}
	assertOutsideArrayUntouched(t, doc, out)
}

func TestDockAddEntryIdempotentWhenPresent(t *testing.T) {
	doc := []byte(sampleDockPlist)
	first, _, err := dockPlistAddEntry(doc, dockAppURL, dockKnownURLs, 1)
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	second, changed, err := dockPlistAddEntry(first, dockAppURL, dockKnownURLs, 2)
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	if changed {
		t.Error("adding an already-pinned app must not change the document")
	}
	if !bytes.Equal(second, first) {
		t.Error("the idempotent add must return the input unchanged")
	}
}

func TestDockAddEntryRecognisesBothLocations(t *testing.T) {
	// A user-location tile in the Dock counts as already pinned when the
	// add would use the system URL...
	userDoc := []byte(strings.Replace(sampleDockPlist,
		"<string>file:///Applications/Google%20Chrome.app/</string>",
		"<string>"+dockUserURL+"</string>", 1))
	out, changed, err := dockPlistAddEntry(userDoc, dockAppURL, dockKnownURLs, 7)
	if err != nil {
		t.Fatalf("add over a user-location tile: %v", err)
	}
	if changed {
		t.Error("a user-location kids-shelf tile must count as already pinned")
	}
	if !bytes.Equal(out, userDoc) {
		t.Error("the recognised add must return the input unchanged")
	}
	// ...and a system-location tile counts when the add would use the
	// user URL (an account that lost write access to /Applications).
	sysDoc, _, err := dockPlistAddEntry([]byte(sampleDockPlist), dockAppURL, dockKnownURLs, 8)
	if err != nil {
		t.Fatalf("seeding the system tile: %v", err)
	}
	out2, changed2, err := dockPlistAddEntry(sysDoc, dockUserURL, dockKnownURLs, 9)
	if err != nil {
		t.Fatalf("add over a system-location tile: %v", err)
	}
	if changed2 {
		t.Error("a system-location kids-shelf tile must count as already pinned")
	}
	if !bytes.Equal(out2, sysDoc) {
		t.Error("the recognised add must return the input unchanged")
	}
}

func TestDockRemoveEntryLeavesOtherItemsIntact(t *testing.T) {
	withKids, _, err := dockPlistAddEntry([]byte(sampleDockPlist), dockAppURL, dockKnownURLs, 42)
	if err != nil {
		t.Fatalf("seeding the tile: %v", err)
	}
	out, changed, err := dockPlistRemoveEntry(withKids, dockKnownURLs)
	if err != nil {
		t.Fatalf("dockPlistRemoveEntry: %v", err)
	}
	if !changed {
		t.Fatal("a Dock with kids-shelf must change on removal")
	}
	apps := dockPersistentApps(t, out)
	if dockHasEntry(apps, dockKnownURLs) {
		t.Error("the kids-shelf tile is still in the array")
	}
	urls := dockTileURLs(apps)
	if len(urls) != 3 {
		t.Errorf("expected the 3 original app tiles, got %d", len(urls))
	}
	for _, want := range []string{
		"/System/Applications/Finder.app",
		"/Applications/Safari.app",
		"/Applications/Google Chrome.app",
	} {
		if !hasURL(urls, want) {
			t.Errorf("tile for %s was lost", want)
		}
	}
	root := plistRootDict(mustParse(t, out))
	others := dictValue(root, "persistent-others")
	if !hasURL(dockTileURLs(others), "/System/Applications/Utilities/Terminal.app") {
		t.Error("persistent-others lost the Terminal tile")
	}
	if !bytes.Contains(out, []byte("<real>59</real>")) {
		t.Error("a top-level key changed")
	}
	if !bytes.Contains(out, []byte("<integer>7195391495842467519</integer>")) {
		t.Error("an untouched tile's GUID changed")
	}
	assertOutsideArrayUntouched(t, withKids, out)

	// Removal is idempotent too.
	again, changedAgain, err := dockPlistRemoveEntry(out, dockKnownURLs)
	if err != nil {
		t.Fatalf("second removal: %v", err)
	}
	if changedAgain {
		t.Error("removing an absent tile must not change the document")
	}
	if !bytes.Equal(again, out) {
		t.Error("the idempotent removal must return the input unchanged")
	}
}

func TestDockRemoveEntryRecognisesBothLocations(t *testing.T) {
	// The user-location tile is removed even though the system location
	// is the one that would be added today.
	userDoc := []byte(strings.Replace(sampleDockPlist,
		"<string>file:///Applications/Google%20Chrome.app/</string>",
		"<string>"+dockUserURL+"</string>", 1))
	out, changed, err := dockPlistRemoveEntry(userDoc, dockKnownURLs)
	if err != nil {
		t.Fatalf("dockPlistRemoveEntry: %v", err)
	}
	if !changed {
		t.Error("a user-location kids-shelf tile must be removed")
	}
	apps := dockPersistentApps(t, out)
	if dockHasEntry(apps, dockKnownURLs) {
		t.Error("the user-location tile is still in the array")
	}
	if len(dockTileURLs(apps)) != 2 {
		t.Errorf("expected the 2 other app tiles, got %d", len(dockTileURLs(apps)))
	}
}

func TestDockAddEntryWhenPersistentAppsMissing(t *testing.T) {
	// A Dock that has never been touched has no persistent-apps key at
	// all; the add creates the key and a fresh array.
	const bareDock = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>show-recents</key>
	<true/>
	<key>tilesize</key>
	<real>59</real>
</dict>
</plist>
`
	out, changed, err := dockPlistAddEntry([]byte(bareDock), dockAppURL, dockKnownURLs, 99)
	if err != nil {
		t.Fatalf("dockPlistAddEntry: %v", err)
	}
	if !changed {
		t.Fatal("a Dock without persistent-apps must change")
	}
	apps := dockPersistentApps(t, out)
	if !dockHasEntry(apps, dockKnownURLs) {
		t.Error("the fresh array does not hold the tile")
	}
	if len(dockTileURLs(apps)) != 1 {
		t.Errorf("expected exactly one tile, got %d", len(dockTileURLs(apps)))
	}
	if !bytes.Contains(out, []byte("<real>59</real>")) {
		t.Error("the existing keys were lost")
	}
	if !bytes.Contains(out, []byte("<true/>")) {
		t.Error("the existing show-recents value was lost")
	}
}

func TestDockNormalizeURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"file:///Applications/kids-shelf.app/", "/Applications/kids-shelf.app"},
		{"file:///Applications/kids-shelf.app", "/Applications/kids-shelf.app"},
		{"file:///Users/alice/Applications/kids-shelf.app/", "/Users/alice/Applications/kids-shelf.app"},
		{"file:///Applications/Google%20Chrome.app/", "/Applications/Google Chrome.app"},
		{"file://localhost/Applications/Finder.app/", "/Applications/Finder.app"},
		{"file:///System/Applications/Finder.app/", "/System/Applications/Finder.app"},
	} {
		if got := normalizeDockURL(tc.in); got != tc.want {
			t.Errorf("normalizeDockURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// mustParse parses a transformed document, failing the test.
func mustParse(t *testing.T, doc []byte) *plistNode {
	t.Helper()
	root, err := parsePlist(bytes.NewReader(doc))
	if err != nil {
		t.Fatalf("re-parsing the transformed plist: %v", err)
	}
	return root
}
