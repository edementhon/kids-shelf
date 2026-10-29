package budget

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Kid is one registered kid profile as listed for the dashboard: the stable
// id, the display name and the kid's animal (one of Animals). PINs are never
// returned.
type Kid struct {
	ID     core.KidID
	Name   string
	Animal string
}

// Animals is every animal a kid can be, in slot order. The server gives each
// kid one when the kid is created (pickAnimal) and stores it with the kid,
// so the shelf, the practice pages and every screen show the same animal,
// and adding or removing a sibling changes nobody's. The first eight are the
// earlier list, which migration 4 used to seed the kids that already existed.
var Animals = []string{"🦊", "🐺", "🐰", "🐻", "🦁", "🐧", "🦄", "🐨", "🐼", "🐯", "🐸", "🐵",
	"🐶", "🐱", "🐮", "🐷", "🐙", "🐢", "🦉", "🐳", "🦒", "🐘", "🦓", "🐬"}

// pickAnimal chooses the animal for kid id: from a slot hashed from the id
// (FNV-1a, 32-bit), the first animal no other kid holds (taken); when every
// animal is taken, the hashed slot's.
func pickAnimal(id string, taken map[string]bool) string {
	h := fnv.New32a()
	h.Write([]byte(id))
	start := int(h.Sum32() % uint32(len(Animals)))
	for n := range len(Animals) {
		if a := Animals[(start+n)%len(Animals)]; !taken[a] {
			return a
		}
	}
	return Animals[start]
}

// takenAnimals is the set of animals the registered kids already hold.
func takenAnimals(tx *sql.Tx) (map[string]bool, error) {
	rows, err := tx.Query(`SELECT animal FROM kids WHERE animal != ''`)
	if err != nil {
		return nil, fmt.Errorf("budget: read kid animals: %w", err)
	}
	defer rows.Close()
	taken := make(map[string]bool)
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("budget: read kid animals: %w", err)
		}
		taken[a] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budget: read kid animals: %w", err)
	}
	return taken, nil
}

// insertKid stores a new kid with a fresh id and its animal.
func (b *Budget) insertKid(tx *sql.Tx, name, pin string) (core.KidID, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	taken, err := takenAnimals(tx)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO kids (id, name, pin, created, animal) VALUES (?, ?, ?, ?, ?)`,
		id, name, pin, b.now().UTC().Format(time.RFC3339Nano), pickAnimal(id, taken)); err != nil {
		return "", err
	}
	return core.KidID(id), nil
}

// CreateKid implements core.Kids: it registers name with its unlock PIN and
// returns the kid's stable id. Creating a name that already exists is
// idempotent: it returns the existing id and leaves the existing profile
// (including its PIN) untouched, so a re-run of migrate is safe. The parent
// page adds kids through AddKid instead, which refuses a name that is taken.
func (b *Budget) CreateKid(ctx context.Context, name, pin string) (core.KidID, error) {
	if name == "" {
		return "", errors.New("budget: empty kid name")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow(`SELECT id FROM kids WHERE name = ?`, name).Scan(&id)
	if err == nil {
		return core.KidID(id), tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("budget: find kid by name: %w", err)
	}
	kid, err := b.insertKid(tx, name, pin)
	if err != nil {
		return "", fmt.Errorf("budget: create kid %q: %w", name, err)
	}
	return kid, tx.Commit()
}

// KidNameTakenError reports that another kid already has the name, compared
// without regard to case ("ava" and "Ava" are the same kid to a family).
// Name is that kid's name as stored, so the page can say which kid it is.
type KidNameTakenError struct {
	Name string
}

func (e *KidNameTakenError) Error() string {
	return fmt.Sprintf("budget: a kid called %q already exists", e.Name)
}

// AddKid registers a new kid for the parent page. Unlike CreateKid it is not
// idempotent: a name that already belongs to a kid, in any letter case, is
// refused with *KidNameTakenError, so a parent never believes a new PIN
// was given to a kid whose old PIN still applies.
func (b *Budget) AddKid(ctx context.Context, name, pin string) (core.KidID, error) {
	if name == "" {
		return "", errors.New("budget: empty kid name")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := nameFree(tx, "", name); err != nil {
		return "", err
	}
	kid, err := b.insertKid(tx, name, pin)
	if err != nil {
		return "", fmt.Errorf("budget: add kid %q: %w", name, err)
	}
	return kid, tx.Commit()
}

// nameFree reports *KidNameTakenError when a kid other than self already
// has name, compared case-insensitively with Unicode case folding (SQLite's
// NOCASE folds ASCII only). A household has a handful of kids, so reading
// every name is cheap.
func nameFree(tx *sql.Tx, self core.KidID, name string) error {
	rows, err := tx.Query(`SELECT id, name FROM kids`)
	if err != nil {
		return fmt.Errorf("budget: check kid names: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, have string
		if err := rows.Scan(&id, &have); err != nil {
			return fmt.Errorf("budget: check kid names: %w", err)
		}
		if id != string(self) && strings.EqualFold(have, name) {
			return &KidNameTakenError{Name: have}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("budget: check kid names: %w", err)
	}
	return nil
}

// FindKid implements core.Kids: it reports whether a kid with this display
// name is registered and, if so, its stable id.
func (b *Budget) FindKid(ctx context.Context, name string) (core.KidID, bool, error) {
	tx, err := b.begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow(`SELECT id FROM kids WHERE name = ?`, name).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("budget: find kid: %w", err)
	}
	return core.KidID(id), true, nil
}

// SetGrownupPIN implements core.Kids: it sets the household watch-together
// PIN; playback with it is free and unmetered, and it stays separate from
// the optional dashboard PIN.
func (b *Budget) SetGrownupPIN(ctx context.Context, pin string) error {
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE household SET grownup_pin = ? WHERE id = 1`, pin); err != nil {
		return fmt.Errorf("budget: set grown-up PIN: %w", err)
	}
	return tx.Commit()
}

// ListKids returns the registered kids ordered by display name. A kid
// without an animal yet gets one here, stored in the same transaction that
// read the list, so it is chosen once and every later listing agrees.
func (b *Budget) ListKids(ctx context.Context) ([]Kid, error) {
	kids, err := listKids(ctx, b.store.DB())
	if err != nil {
		return nil, err
	}
	for _, k := range kids {
		if k.Animal == "" {
			return b.listKidsAssigning(ctx)
		}
	}
	return kids, nil
}

// listKidsAssigning is ListKids' slow path: inside one transaction it reads
// the kids again and gives each kid that still has no animal one, in id
// order so the choice does not depend on names.
func (b *Budget) listKidsAssigning(ctx context.Context) ([]Kid, error) {
	tx, err := b.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	kids, err := listKids(ctx, tx)
	if err != nil {
		return nil, err
	}
	taken := make(map[string]bool)
	var bare []int
	for i, k := range kids {
		if k.Animal == "" {
			bare = append(bare, i)
		} else {
			taken[k.Animal] = true
		}
	}
	sort.Slice(bare, func(x, y int) bool { return kids[bare[x]].ID < kids[bare[y]].ID })
	for _, i := range bare {
		a := pickAnimal(string(kids[i].ID), taken)
		if _, err := tx.ExecContext(ctx, `UPDATE kids SET animal = ? WHERE id = ?`, a, kids[i].ID); err != nil {
			return nil, fmt.Errorf("budget: store kid animal: %w", err)
		}
		kids[i].Animal = a
		taken[a] = true
	}
	return kids, tx.Commit()
}

// listKids reads the kids ordered by display name through q (the database
// or a transaction).
func listKids(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) ([]Kid, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, name, animal FROM kids ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("budget: list kids: %w", err)
	}
	defer rows.Close()
	var kids []Kid
	for rows.Next() {
		var k Kid
		if err := rows.Scan(&k.ID, &k.Name, &k.Animal); err != nil {
			return nil, fmt.Errorf("budget: list kids: %w", err)
		}
		kids = append(kids, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("budget: list kids: %w", err)
	}
	return kids, nil
}

// RenameKid renames the kid profile with id; the stable id (and therefore the
// accounts, credits and settings keyed on it) is unchanged. The new name must
// not already belong to another kid in any letter case (*KidNameTakenError);
// the kid's own name in another case is fine ("ava" to "Ava").
func (b *Budget) RenameKid(ctx context.Context, id core.KidID, name string) error {
	if name == "" {
		return errors.New("budget: empty kid name")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := nameFree(tx, id, name); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE kids SET name = ? WHERE id = ?`, name, string(id))
	if err != nil {
		return fmt.Errorf("budget: rename kid: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("budget: rename kid: %w", err)
	}
	if n == 0 {
		return ErrNoSuchKid
	}
	return tx.Commit()
}

// SetKidPIN changes a kid's unlock PIN.
func (b *Budget) SetKidPIN(ctx context.Context, id core.KidID, pin string) error {
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE kids SET pin = ? WHERE id = ?`, pin, string(id))
	if err != nil {
		return fmt.Errorf("budget: set kid PIN: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("budget: set kid PIN: %w", err)
	}
	if n == 0 {
		return ErrNoSuchKid
	}
	return tx.Commit()
}

// RemoveKid deletes the kid profile and everything keyed on its id: the
// accounts, the per-kid settings and the credit records (the tables'
// ON DELETE CASCADE).
func (b *Budget) RemoveKid(ctx context.Context, id core.KidID) error {
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`DELETE FROM kids WHERE id = ?`, string(id))
	if err != nil {
		return fmt.Errorf("budget: remove kid: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("budget: remove kid: %w", err)
	}
	if n == 0 {
		return ErrNoSuchKid
	}
	return tx.Commit()
}

// VerifyKidPIN reports whether pin is the kid's unlock PIN, compared in
// constant time. An unknown kid, or a kid whose PIN is empty, never verifies.
func (b *Budget) VerifyKidPIN(ctx context.Context, id core.KidID, pin string) bool {
	var stored string
	err := b.store.DB().QueryRowContext(ctx, `SELECT pin FROM kids WHERE id = ?`, string(id)).Scan(&stored)
	if err != nil {
		return false
	}
	return stored != "" && pinsEqual(stored, pin)
}

// VerifyGrownupPIN reports whether pin is the household watch-together PIN,
// compared in constant time like VerifyKidPIN. An unset grown-up PIN never
// verifies, not even against an empty pin, so the shelf cannot unlock a
// household that has not configured one.
func (b *Budget) VerifyGrownupPIN(ctx context.Context, pin string) bool {
	var stored string
	err := b.store.DB().QueryRowContext(ctx, `SELECT grownup_pin FROM household WHERE id = 1`).Scan(&stored)
	if err != nil {
		return false
	}
	return stored != "" && pinsEqual(stored, pin)
}

// GrownupPINSet reports whether the household has configured the watch-
// together PIN. The kid page uses it to explain the keypad when no PIN
// exists yet, instead of showing a wrong-PIN error.
func (b *Budget) GrownupPINSet(ctx context.Context) bool {
	var stored string
	err := b.store.DB().QueryRowContext(ctx, `SELECT grownup_pin FROM household WHERE id = 1`).Scan(&stored)
	if err != nil {
		return false
	}
	return stored != ""
}

// pinsEqual compares two secrets in constant time by comparing their sha-256
// digests: the digest comparison never leaks how many leading characters
// matched, and the fixed digest length hides the length difference.
func pinsEqual(a, b string) bool {
	da := sha256.Sum256([]byte(a))
	db := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(da[:], db[:]) == 1
}

// newID returns a random 128-bit id, hex-encoded.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("budget: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// Compile-time checks that Budget implements the phase-1 interfaces it
// claims.
var (
	_ core.Kids           = (*Budget)(nil)
	_ core.Crediter       = (*Budget)(nil)
	_ core.BudgetImporter = (*Budget)(nil)
)
