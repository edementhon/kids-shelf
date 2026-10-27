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
	"time"

	"github.com/edementhon/kids-shelf/internal/core"
)

// Kid is one registered kid profile as listed for the dashboard: the stable
// id and the display name. PINs are never returned.
type Kid struct {
	ID   core.KidID
	Name string
}

// CreateKid implements core.Kids: it registers name with its unlock PIN and
// returns the kid's stable id. Creating a name that already exists is
// idempotent: it returns the existing id and leaves the existing profile
// (including its PIN) untouched, so a re-run of migrate is safe.
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
	id, err = newID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO kids (id, name, pin, created) VALUES (?, ?, ?, ?)`,
		id, name, pin, b.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return "", fmt.Errorf("budget: create kid %q: %w", name, err)
	}
	return core.KidID(id), tx.Commit()
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

// ListKids returns the registered kids ordered by display name.
func (b *Budget) ListKids(ctx context.Context) ([]Kid, error) {
	rows, err := b.store.DB().QueryContext(ctx, `SELECT id, name FROM kids ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("budget: list kids: %w", err)
	}
	defer rows.Close()
	var kids []Kid
	for rows.Next() {
		var k Kid
		if err := rows.Scan(&k.ID, &k.Name); err != nil {
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
// not already belong to another kid.
func (b *Budget) RenameKid(ctx context.Context, id core.KidID, name string) error {
	if name == "" {
		return errors.New("budget: empty kid name")
	}
	tx, err := b.begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
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
