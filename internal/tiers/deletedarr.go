package tiers

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// SettingDeletedArr is the settings key of the deleted *arr integrations whose folders stay
// unknown (S14): a JSON array of DeletedArr.
const SettingDeletedArr = "tiers.deletedArr"

// DeletedArr is a deleted *arr integration as the tier facts remember it. Deleting an *arr
// integration cascades its items, files and root folders, so without this record the files it
// managed would look unmanaged (arr.* false), which can only lower their protection. S14 says a
// deleted integration can only make a file more protected: a file under one of its folders that
// no live *arr claims stays unknown until the user confirms the removal (ConfirmDeletedArr).
type DeletedArr struct {
	// Key identifies the record (ids of deleted integrations can be reused).
	Key           string `json:"key"`
	IntegrationID int64  `json:"integrationId"`
	Name          string `json:"name"`
	App           string `json:"app"`
	// Folders are its mapped root folders, and the mapped item folders outside them, as Bunkarr
	// sees them.
	Folders []string `json:"folders"`
	// Unmapped are its root folders, and its item folders outside them, (as the *arr sees them)
	// that no path mapping covered: Bunkarr cannot tell which files were in them, so every file no
	// live *arr claims stays unknown.
	Unmapped  []string  `json:"unmapped,omitempty"`
	DeletedAt time.Time `json:"deletedAt"`
}

// maxDeletedArrFolders bounds the folders one record keeps (item folders outside every root
// folder are rare; a record that would exceed it keeps the root folders and marks the rest as
// unmapped, which is more protective).
const maxDeletedArrFolders = 2000

// RememberDeletedArr records an *arr integration that is about to be deleted (the API calls it
// before integrations.Store.Delete; on a failed delete it calls ForgetDeletedArr with the returned
// key). It is a no-op (zero DeletedArr) for other integration types.
func (e *Engine) RememberDeletedArr(ctx context.Context, it integrations.Integration) (DeletedArr, error) {
	if !it.Type.IsArr() {
		return DeletedArr{}, nil
	}
	d := DeletedArr{Key: newDeletedKey(), IntegrationID: it.ID, Name: it.Name, App: it.Type.AppName(), Folders: []string{}, DeletedAt: e.o.Now().UTC()}
	m, err := e.o.Index.Meta(ctx, nil, it.ID)
	if err != nil {
		return DeletedArr{}, fmt.Errorf("tiers: %w", err)
	}
	for _, rf := range m.RootFolders {
		if rf.LocalPath != nil && catalog.LocatablePath(*rf.LocalPath) {
			if !slices.Contains(d.Folders, *rf.LocalPath) {
				d.Folders = append(d.Folders, *rf.LocalPath)
			}
		} else if !slices.Contains(d.Unmapped, rf.Path) {
			d.Unmapped = append(d.Unmapped, rf.Path)
		}
	}
	roots := slices.Clone(d.Folders)
	unmappedRoots := slices.Clone(d.Unmapped)
	var arrRoots []string
	for _, rf := range m.RootFolders {
		arrRoots = append(arrRoots, rf.Path)
	}
	// An item folder that no path mapping covers (outside the unmapped root folders) keeps the
	// files unknown while the integration is live (newContext, unmappedItems); it must after the
	// delete too, or the file becomes unmanaged (S14). Without readable settings no item folder
	// maps: only the root folders cover them.
	s, serr := it.ArrSettings()
	overflow, unmappedOverflow := false, false
	err = e.o.Index.EachItem(ctx, nil, it.ID, false, func(item mediaindex.Item) error {
		local, ok := "", false
		if serr == nil {
			local, ok = s.MapPath(item.Path)
		}
		if !ok || !catalog.LocatablePath(local) {
			if item.Path == "" || underArrRoot(item.Path, unmappedRoots) || serr != nil && underArrRoot(item.Path, arrRoots) ||
				slices.Contains(d.Unmapped, item.Path) {
				return nil
			}
			if len(d.Unmapped) >= maxDeletedArrFolders {
				unmappedOverflow = true
				return nil
			}
			d.Unmapped = append(d.Unmapped, item.Path)
			return nil
		}
		if underAny(local, roots) || slices.Contains(d.Folders, local) {
			return nil
		}
		if len(d.Folders) >= maxDeletedArrFolders {
			overflow = true
			return nil
		}
		d.Folders = append(d.Folders, local)
		return nil
	})
	if err != nil {
		return DeletedArr{}, fmt.Errorf("tiers: %w", err)
	}
	if overflow {
		d.Unmapped = append(d.Unmapped, fmt.Sprintf("(more than %d item folders outside its root folders)", maxDeletedArrFolders))
	}
	if unmappedOverflow {
		d.Unmapped = append(d.Unmapped, fmt.Sprintf("(more than %d item folders without a path mapping)", maxDeletedArrFolders))
	}
	if len(d.Folders) == 0 && len(d.Unmapped) == 0 {
		return DeletedArr{}, nil
	}
	err = e.o.DB.Write(ctx, func(tx *sql.Tx) error {
		list, err := deletedArrQ(ctx, tx)
		if err != nil {
			return err
		}
		return setDeletedArrTx(ctx, tx, append(list, d), e.o.Now())
	})
	if err != nil {
		return DeletedArr{}, err
	}
	return d, nil
}

// ForgetDeletedArr removes a deleted *arr record (the delete failed, so the integration is still
// there): the files under its folders are then decided by the live integrations alone. A key that
// names no record is not an error.
func (e *Engine) ForgetDeletedArr(ctx context.Context, key string) error {
	_, _, err := e.removeDeletedArr(ctx, key)
	return err
}

// ConfirmDeletedArr records the user's confirmation that a deleted *arr integration is gone for
// good (DELETE /integrations/deleted/{key}, §8.3): its record is removed, so the files under its
// folders are decided by the live integrations alone. A file there that no live *arr claims is
// then unmanaged (arr.* false) once every *arr cache is fresh and mapped, and the rules may lower
// its tier; what the destinations hold for it is kept (S15) until a confirmed release. It returns
// the removed record, or ErrNotFound when key names none.
func (e *Engine) ConfirmDeletedArr(ctx context.Context, key string) (DeletedArr, error) {
	d, ok, err := e.removeDeletedArr(ctx, key)
	if err == nil && !ok {
		err = fmt.Errorf("deleted *arr integration %q: %w", key, ErrNotFound)
	}
	return d, err
}

// removeDeletedArr removes the record key names, in one write, and returns it (ok false when
// there is none).
func (e *Engine) removeDeletedArr(ctx context.Context, key string) (DeletedArr, bool, error) {
	var gone DeletedArr
	var ok bool
	if key == "" {
		return gone, false, nil
	}
	err := e.o.DB.Write(ctx, func(tx *sql.Tx) error {
		gone, ok = DeletedArr{}, false
		list, err := deletedArrQ(ctx, tx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(list, func(d DeletedArr) bool { return d.Key == key })
		if i < 0 {
			return nil
		}
		gone, ok = list[i], true
		return setDeletedArrTx(ctx, tx, slices.Delete(list, i, i+1), e.o.Now())
	})
	if err != nil {
		return DeletedArr{}, false, err
	}
	return gone, ok, nil
}

// DeletedArrs returns the deleted *arr records.
func (e *Engine) DeletedArrs(ctx context.Context) ([]DeletedArr, error) {
	return deletedArrQ(ctx, e.o.DB.Reader())
}

func deletedArrQ(ctx context.Context, q Queryer) ([]DeletedArr, error) {
	var v string
	err := q.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, SettingDeletedArr).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return []DeletedArr{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the deleted *arr integrations: %w", err)
	}
	out := []DeletedArr{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("read the deleted *arr integrations: %w", err)
	}
	return out, nil
}

func setDeletedArrTx(ctx context.Context, tx *sql.Tx, list []DeletedArr, now time.Time) error {
	b, err := json.Marshal(list)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value, encrypted, updated_at) VALUES (?, ?, 0, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value, encrypted = 0, updated_at = excluded.updated_at`,
		SettingDeletedArr, string(b), db.FormatTime(now))
	if err != nil {
		return fmt.Errorf("save the deleted *arr integrations: %w", err)
	}
	return nil
}

func newDeletedKey() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24+)
	return hex.EncodeToString(b[:])
}

// underAny reports whether p is one of dirs or lies under one of them.
func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if p == d || d == "/" || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// addDeleted adds the deleted *arr records to the fact context: their folders, and a notice for
// each record that still decides something (a folder no live *arr root folder covers, or an
// unmapped root folder).
func (fc *factContext) addDeleted(list []DeletedArr) {
	var liveRoots []string
	for r := range fc.roots {
		liveRoots = append(liveRoots, r)
	}
	for i := range list {
		d := &list[i]
		for _, f := range d.Folders {
			fc.deletedFolders[f] = append(fc.deletedFolders[f], d)
		}
		if len(d.Unmapped) > 0 {
			fc.deletedUnmapped = append(fc.deletedUnmapped, d)
		}
		uncovered := len(d.Unmapped) > 0
		for _, f := range d.Folders {
			uncovered = uncovered || !underAny(f, liveRoots)
		}
		if uncovered {
			fc.unknown = append(fc.unknown, UnknownSource{IntegrationID: d.IntegrationID, Name: d.Name,
				Reason: fmt.Sprintf("%s (%s) was deleted: the files in its folders that no other *arr manages stay unknown (so full) until you confirm its removal (Settings → Tiers, Deleted *arr integrations)", d.Name, d.App)})
		}
	}
}
