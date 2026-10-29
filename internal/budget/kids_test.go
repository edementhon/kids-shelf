package budget

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
	"github.com/edementhon/kids-shelf/internal/store"
)

// TestCreateKidIdempotentByName: creating a name that already exists returns
// the existing id and leaves the existing profile (PIN included) untouched,
// so a re-run of migrate is safe.
func TestCreateKidIdempotentByName(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	id1, err := b.CreateKid(ctx, "alex", "1234")
	if err != nil {
		t.Fatalf("CreateKid: %v", err)
	}
	id2, err := b.CreateKid(ctx, "alex", "9999")
	if err != nil {
		t.Fatalf("CreateKid retry: %v", err)
	}
	if id1 != id2 {
		t.Errorf("ids differ: %s vs %s, want the same", id1, id2)
	}
	if b.VerifyKidPIN(ctx, id1, "1234") != true {
		t.Error("original PIN no longer verifies")
	}
	if b.VerifyKidPIN(ctx, id1, "9999") {
		t.Error("retry PIN verifies; CreateKid must not overwrite the profile")
	}
}

// TestCreateKidEmptyNameRejected.
func TestCreateKidEmptyNameRejected(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	if _, err := b.CreateKid(ctx, "", "1234"); err == nil {
		t.Error("CreateKid with empty name: want error")
	}
}

// TestFindKid: name resolution to the stable id, exact match.
func TestFindKid(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	id := mustCreateKid(t, b, "alex", "1234")
	foundID, ok, err := b.FindKid(ctx, "alex")
	if err != nil {
		t.Fatalf("FindKid: %v", err)
	}
	if !ok || foundID != id {
		t.Errorf("FindKid(alex) = (%s, %v), want (%s, true)", foundID, ok, id)
	}
	if _, ok, err := b.FindKid(ctx, "nobody"); err != nil || ok {
		t.Errorf("FindKid(nobody) = ok %v, err %v; want false, nil", ok, err)
	}
}

// TestListKidsSortedByName: the dashboard lists profiles order-stably.
func TestListKidsSortedByName(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	alex := mustCreateKid(t, b, "alex", "1111")
	mustCreateKid(t, b, "sam", "2222")
	abby := mustCreateKid(t, b, "abby", "3333")
	kids, err := b.ListKids(ctx)
	if err != nil {
		t.Fatalf("ListKids: %v", err)
	}
	if len(kids) != 3 {
		t.Fatalf("ListKids len = %d, want 3", len(kids))
	}
	wantNames := []string{"abby", "alex", "sam"}
	for i, k := range kids {
		if k.Name != wantNames[i] {
			t.Errorf("kids[%d].Name = %q, want %q", i, k.Name, wantNames[i])
		}
	}
	if kids[0].ID != abby || kids[1].ID != alex {
		t.Errorf("ids not carried: %v", kids)
	}
}

// TestRenameKid: the display name changes but the stable id (and everything
// keyed on it) stays.
func TestRenameKid(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	id := mustCreateKid(t, b, "alex", "1111")
	if err := b.RenameKid(ctx, id, "alex2"); err != nil {
		t.Fatalf("RenameKid: %v", err)
	}
	if _, ok, _ := b.FindKid(ctx, "alex"); ok {
		t.Error("old name still resolves")
	}
	foundID, ok, err := b.FindKid(ctx, "alex2")
	if err != nil || !ok {
		t.Fatalf("FindKid(alex2) = ok %v, err %v", ok, err)
	}
	if foundID != id {
		t.Errorf("FindKid(alex2) = %s, want %s", foundID, id)
	}
	if !b.VerifyKidPIN(ctx, id, "1111") {
		t.Error("PIN lost after rename")
	}
	// a name already taken by another kid is rejected
	mustCreateKid(t, b, "sam", "2222")
	if err := b.RenameKid(ctx, id, "sam"); err == nil {
		t.Error("RenameKid onto a taken name: want error")
	}
	// unknowns and empty names are rejected
	if err := b.RenameKid(ctx, ghostID, "x"); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("RenameKid(unknown): err = %v, want ErrNoSuchKid", err)
	}
	// the same name in other letter cases is taken too, and the error names
	// the kid as stored
	var taken *KidNameTakenError
	if err := b.RenameKid(ctx, id, "SAM"); !errors.As(err, &taken) || taken.Name != "sam" {
		t.Errorf("RenameKid onto SAM: err = %v, want KidNameTakenError for sam", err)
	}
	// a kid may change the case of its own name
	if err := b.RenameKid(ctx, id, "Alex2"); err != nil {
		t.Errorf("RenameKid alex2 to Alex2: %v", err)
	}
	if err := b.RenameKid(ctx, id, ""); err == nil {
		t.Error("RenameKid to empty name: want error")
	}
}

// TestAddKidRefusesTakenName: the parent page's add refuses a name that is
// already a kid's, in any letter case (Unicode too), and never touches the
// existing kid's PIN; CreateKid stays idempotent for migrate.
func TestAddKidRefusesTakenName(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	ava, err := b.AddKid(ctx, "Ava", "1111")
	if err != nil {
		t.Fatalf("AddKid: %v", err)
	}
	for _, name := range []string{"Ava", "ava", "AVA"} {
		var taken *KidNameTakenError
		if _, err := b.AddKid(ctx, name, "5555"); !errors.As(err, &taken) || taken.Name != "Ava" {
			t.Errorf("AddKid(%q): err = %v, want KidNameTakenError for Ava", name, err)
		}
	}
	if !b.VerifyKidPIN(ctx, ava, "1111") || b.VerifyKidPIN(ctx, ava, "5555") {
		t.Error("a refused add changed the existing kid's PIN")
	}
	if _, err := b.AddKid(ctx, "Élodie", "2222"); err != nil {
		t.Fatalf("AddKid(Élodie): %v", err)
	}
	if _, err := b.AddKid(ctx, "élodie", "3333"); !errors.As(err, new(*KidNameTakenError)) {
		t.Errorf("AddKid(élodie): err = %v, want KidNameTakenError", err)
	}
	if _, err := b.AddKid(ctx, "", "3333"); err == nil {
		t.Error("AddKid with empty name: want error")
	}
	kids, err := b.ListKids(ctx)
	if err != nil || len(kids) != 2 {
		t.Errorf("kids = %v (%v), want Ava and Élodie only", kids, err)
	}
	// migrate's CreateKid still returns the existing id for the exact name.
	if id, err := b.CreateKid(ctx, "Ava", "9999"); err != nil || id != ava {
		t.Errorf("CreateKid(Ava) = %s, %v; want the existing id %s", id, err, ava)
	}
}

// TestSetKidPIN: the new PIN verifies, the old one does not.
func TestSetKidPIN(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	id := mustCreateKid(t, b, "alex", "1111")
	if err := b.SetKidPIN(ctx, id, "7777"); err != nil {
		t.Fatalf("SetKidPIN: %v", err)
	}
	if !b.VerifyKidPIN(ctx, id, "7777") {
		t.Error("new PIN does not verify")
	}
	if b.VerifyKidPIN(ctx, id, "1111") {
		t.Error("old PIN still verifies")
	}
	if err := b.SetKidPIN(ctx, ghostID, "1"); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("SetKidPIN(unknown): err = %v, want ErrNoSuchKid", err)
	}
}

// TestRemoveKidCascades: removing a kid drops its accounts, per-kid settings
// and credit records, not just the profile row.
func TestRemoveKidCascades(t *testing.T) {
	b, s := newTestBudget(t, at(nowOct26))
	kid := mustCreateKid(t, b, "alex", "1111")
	importAccount(t, b, kid, 5, 2, "2026-10-25")
	if err := b.SetDailyMinutes(ctx, kid, 25); err != nil {
		t.Fatalf("SetDailyMinutes: %v", err)
	}
	if err := b.Credit(ctx, string(kid), DefaultCounter, 3, "a1"); err != nil {
		t.Fatalf("Credit: %v", err)
	}
	if err := b.RemoveKid(ctx, kid); err != nil {
		t.Fatalf("RemoveKid: %v", err)
	}
	if _, ok, _ := b.FindKid(ctx, "alex"); ok {
		t.Error("removed kid still resolves")
	}
	if _, _, err := b.Balance(ctx, kid, DefaultCounter); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("Balance after remove: err = %v, want ErrNoSuchKid", err)
	}
	for _, q := range []string{
		`SELECT COUNT(*) FROM accounts WHERE kid = ?`,
		`SELECT COUNT(*) FROM credits WHERE kid = ?`,
		`SELECT COUNT(*) FROM kid_settings WHERE kid = ?`,
	} {
		var n int
		if err := s.DB().QueryRow(q, string(kid)).Scan(&n); err != nil {
			t.Fatalf("cascade query: %v", err)
		}
		if n != 0 {
			t.Errorf("cascade left %d rows for %q", n, q)
		}
	}
	if err := b.RemoveKid(ctx, kid); !errors.Is(err, ErrNoSuchKid) {
		t.Errorf("RemoveKid again: err = %v, want ErrNoSuchKid", err)
	}
}

// TestVerifyKidPIN: only the exact stored PIN verifies.
func TestVerifyKidPIN(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	id := mustCreateKid(t, b, "alex", "1234")
	if !b.VerifyKidPIN(ctx, id, "1234") {
		t.Error("correct PIN does not verify")
	}
	if b.VerifyKidPIN(ctx, id, "0000") {
		t.Error("wrong PIN verifies")
	}
	if b.VerifyKidPIN(ctx, id, "") {
		t.Error("empty PIN verifies against a stored PIN")
	}
}

// TestVerifyGrownupPIN: an unset grown-up PIN never verifies, not even
// against an empty pin; once set it verifies and survives a restart.
func TestVerifyGrownupPIN(t *testing.T) {
	b, s := newTestBudget(t, at(nowOct26))
	if b.VerifyGrownupPIN(ctx, "") {
		t.Error("unset grown-up PIN verifies empty input")
	}
	if b.VerifyGrownupPIN(ctx, "0000") {
		t.Error("unset grown-up PIN verifies a PIN")
	}
	if err := b.SetGrownupPIN(ctx, "0000"); err != nil {
		t.Fatalf("SetGrownupPIN: %v", err)
	}
	if !b.VerifyGrownupPIN(ctx, "0000") {
		t.Error("set grown-up PIN does not verify")
	}
	if b.VerifyGrownupPIN(ctx, "1111") {
		t.Error("wrong grown-up PIN verifies")
	}
	b2, err := New(s, at(nowOct26))
	if err != nil {
		t.Fatalf("budget.New: %v", err)
	}
	if !b2.VerifyGrownupPIN(ctx, "0000") {
		t.Error("grown-up PIN lost on restart")
	}
}

// TestPinsEqual: the constant-time comparator itself, equal only for
// identical strings, and length differences do not match.
func TestPinsEqual(t *testing.T) {
	if !pinsEqual("1234", "1234") {
		t.Error("pinsEqual(same) = false, want true")
	}
	if pinsEqual("1234", "1235") {
		t.Error("pinsEqual(differing digit) = true, want false")
	}
	if pinsEqual("1234", "123") {
		t.Error("pinsEqual(shorter) = true, want false")
	}
	if pinsEqual("1234", "12345") {
		t.Error("pinsEqual(longer) = true, want false")
	}
	if pinsEqual("", "1234") {
		t.Error("pinsEqual(empty vs non-empty) = true, want false")
	}
	if !pinsEqual("", "") {
		t.Error("pinsEqual(empty, empty) = false, want true")
	}
}

// TestVerifyKidPINFromCoreInterface: Budget satisfies the phase-1 Kids
// contract (compile-time checked in kids.go); exercise it through the
// interface to make sure the signatures line up at runtime too.
func TestKidsInterface(t *testing.T) {
	var kids core.Kids = &Budget{}
	if kids == nil {
		t.Fatal("Budget does not implement core.Kids")
	}
}

// animalsByName lists the kids and returns each kid's animal by name,
// failing the test when a kid has none or two kids share one.
func animalsByName(t *testing.T, b *Budget) map[string]string {
	t.Helper()
	kids, err := b.ListKids(ctx)
	if err != nil {
		t.Fatalf("ListKids: %v", err)
	}
	out := make(map[string]string, len(kids))
	seen := make(map[string]string)
	for _, k := range kids {
		if k.Animal == "" {
			t.Fatalf("%s has no animal", k.Name)
		}
		if other, ok := seen[k.Animal]; ok {
			t.Fatalf("%s and %s share %s", other, k.Name, k.Animal)
		}
		seen[k.Animal] = k.Name
		out[k.Name] = k.Animal
	}
	return out
}

// TestKidAnimalsStayWhenKidsComeAndGo: a kid's animal is chosen once, so
// adding a kid who sorts first, or removing one, changes nobody else's.
func TestKidAnimalsStayWhenKidsComeAndGo(t *testing.T) {
	b, _ := newTestBudget(t, at(nowOct26))
	if _, err := b.AddKid(ctx, "Bea", "1111"); err != nil {
		t.Fatalf("AddKid Bea: %v", err)
	}
	mustCreateKid(t, b, "Cy", "2222") // the importer's path
	before := animalsByName(t, b)

	aaron, err := b.AddKid(ctx, "Aaron", "3333")
	if err != nil {
		t.Fatalf("AddKid Aaron: %v", err)
	}
	added := animalsByName(t, b)
	for _, name := range []string{"Bea", "Cy"} {
		if added[name] != before[name] {
			t.Errorf("adding Aaron changed %s from %s to %s", name, before[name], added[name])
		}
	}

	if err := b.RemoveKid(ctx, aaron); err != nil {
		t.Fatalf("RemoveKid: %v", err)
	}
	removed := animalsByName(t, b)
	for _, name := range []string{"Bea", "Cy"} {
		if removed[name] != before[name] {
			t.Errorf("removing Aaron changed %s from %s to %s", name, before[name], removed[name])
		}
	}
}

// TestKidAnimalsMigrationKeepsOldAnimals: kids that existed before
// animals were stored keep the animal the old rule showed them: position i
// in ListKids' name order gets the i%8th of the old eight.
func TestKidAnimalsMigrationKeepsOldAnimals(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "budget.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	var before []store.Migration
	for _, m := range Migrations {
		if m.Version < 4 {
			before = append(before, m)
		}
	}
	if err := s.Migrate(before...); err != nil {
		t.Fatalf("migrate to version 3: %v", err)
	}
	// Inserted out of order; byte order puts "Zed" before "abby".
	names := []string{"sam", "Zed", "abby", "kim", "Ben", "lou", "max", "eve", "dan", "ida"}
	for i, name := range names {
		if _, err := s.DB().Exec(`INSERT INTO kids (id, name, pin, created) VALUES (?, ?, '0000', '')`,
			name+"-id", name); err != nil {
			t.Fatalf("insert kid %d: %v", i, err)
		}
	}
	b, err := New(s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	kids, err := b.ListKids(ctx)
	if err != nil {
		t.Fatalf("ListKids: %v", err)
	}
	old := []string{"🦊", "🐺", "🐰", "🐻", "🦁", "🐧", "🦄", "🐨"}
	wantOrder := []string{"Ben", "Zed", "abby", "dan", "eve", "ida", "kim", "lou", "max", "sam"}
	if len(kids) != len(wantOrder) {
		t.Fatalf("ListKids = %v, want %d kids", kids, len(wantOrder))
	}
	for i, k := range kids {
		if k.Name != wantOrder[i] || k.Animal != old[i%len(old)] {
			t.Errorf("kids[%d] = %s %s, want %s %s", i, k.Name, k.Animal, wantOrder[i], old[i%len(old)])
		}
	}
}

// TestKidAnimalsAssignedOnceWhenListed: a kid stored without an animal
// gets a free one the first time the kids are listed, and keeps it.
func TestKidAnimalsAssignedOnceWhenListed(t *testing.T) {
	b, s := newTestBudget(t, at(nowOct26))
	mustCreateKid(t, b, "alex", "1111")
	if _, err := s.DB().Exec(`INSERT INTO kids (id, name, pin, created) VALUES ('bare-id', 'bo', '0000', '')`); err != nil {
		t.Fatalf("insert bare kid: %v", err)
	}
	first := animalsByName(t, b)
	var stored string
	if err := s.DB().QueryRow(`SELECT animal FROM kids WHERE id = 'bare-id'`).Scan(&stored); err != nil {
		t.Fatalf("read stored animal: %v", err)
	}
	if stored != first["bo"] {
		t.Errorf("bo's animal stored as %q, listed as %q", stored, first["bo"])
	}
	if again := animalsByName(t, b); again["bo"] != first["bo"] || again["alex"] != first["alex"] {
		t.Errorf("second listing %v, first %v", again, first)
	}
}

// TestPickAnimal: the hashed slot when free, the next free animal after it
// when not, and the hashed slot again once every animal is taken.
func TestPickAnimal(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	slot := pickAnimal(id, nil)
	start := -1
	for i, a := range Animals {
		if a == slot {
			start = i
		}
	}
	if start < 0 {
		t.Fatalf("pickAnimal = %q, not an animal", slot)
	}
	next := Animals[(start+1)%len(Animals)]
	if got := pickAnimal(id, map[string]bool{slot: true}); got != next {
		t.Errorf("slot taken: pickAnimal = %s, want the next one %s", got, next)
	}
	all := make(map[string]bool)
	for _, a := range Animals {
		all[a] = true
	}
	if got := pickAnimal(id, all); got != slot {
		t.Errorf("all taken: pickAnimal = %s, want the hashed slot %s", got, slot)
	}
}
