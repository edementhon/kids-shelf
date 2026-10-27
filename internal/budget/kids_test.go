package budget

import (
	"errors"
	"testing"

	"github.com/edementhon/kids-shelf/internal/core"
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

// TestRenameKid: the display name changes but the stable id — and everything
// keyed on it — stays.
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
	if err := b.RenameKid(ctx, id, ""); err == nil {
		t.Error("RenameKid to empty name: want error")
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

// TestVerifyGrownupPIN: an unset grown-up PIN never verifies — not even
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

// TestPinsEqual: the constant-time comparator itself — equal only for
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
