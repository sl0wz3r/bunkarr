package tiers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

// Flag kinds and names (item_flags).
const (
	// FlagIrreplaceable makes every file it covers full at every destination (§8.1).
	FlagIrreplaceable = "irreplaceable"
	// FlagKindArr is set on an *arr item and follows the item's external ids.
	FlagKindArr = "arr"
	// FlagKindPath is set on a file, or on every file under a folder, of a source.
	FlagKindPath = "path"
	// MaxNoteLen is the longest flag note, in characters.
	MaxNoteLen = 500
)

// Flag is a manual flag (ItemFlag of design §13). Resolved and Reason are set by
// Engine.Flags: whether the flag covers something now, and why not.
type Flag struct {
	ID   int64  `json:"id"`
	Flag string `json:"flag"`
	Kind string `json:"kind"`
	// *arr flags: the integration it was set on (nil once that is deleted), the item's kind and
	// *arr id (a cache), the external ids it follows, and the item's last located folder.
	IntegrationID *int64                 `json:"integrationId"`
	ArrKind       string                 `json:"arrKind,omitempty"`
	ArrID         int64                  `json:"arrId,omitempty"`
	ExternalIDs   mediaindex.ExternalIDs `json:"externalIds"`
	LastSourceID  *int64                 `json:"lastSourceId"`
	LastRelPath   *string                `json:"lastRelPath"`
	// Path flags: the source and the file or folder ("" is the whole source).
	SourceID  *int64    `json:"sourceId"`
	RelPath   *string   `json:"relPath"`
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Resolved  bool      `json:"resolved"`
	Reason    string    `json:"reason,omitempty"`
}

// FlagTarget is what a new flag is set on: an *arr item {integrationId, kind, arrId} or a path
// {sourceId, relPath}.
type FlagTarget struct {
	IntegrationID int64   `json:"integrationId,omitempty"`
	Kind          string  `json:"kind,omitempty"`
	ArrID         int64   `json:"arrId,omitempty"`
	SourceID      int64   `json:"sourceId,omitempty"`
	RelPath       *string `json:"relPath,omitempty"`
}

// FlagInput is POST /tiers/flags.
type FlagInput struct {
	Flag   string     `json:"flag"`
	Target FlagTarget `json:"target"`
	Note   string     `json:"note"`
}

const flagColumns = `id, flag, kind, integration_id, arr_kind, arr_id, external_ids, last_source_id, last_rel_path, source_id,
	rel_path, note, created_at, updated_at`

func scanFlag(r interface{ Scan(...any) error }) (Flag, error) {
	var (
		f                       Flag
		integ, arrID, last, src sql.NullInt64
		arrKind, lastRel, rel   sql.NullString
		ext, created, updated   string
	)
	if err := r.Scan(&f.ID, &f.Flag, &f.Kind, &integ, &arrKind, &arrID, &ext, &last, &lastRel, &src, &rel, &f.Note, &created, &updated); err != nil {
		return Flag{}, err
	}
	f.IntegrationID, f.LastSourceID, f.SourceID = optInt(integ), optInt(last), optInt(src)
	f.LastRelPath, f.RelPath = optStr(lastRel), optStr(rel)
	f.ArrKind, f.ArrID = arrKind.String, arrID.Int64
	if err := json.Unmarshal([]byte(ext), &f.ExternalIDs); err != nil {
		return Flag{}, fmt.Errorf("flag %d: external_ids: %w", f.ID, err)
	}
	var err error
	if f.CreatedAt, err = db.ParseTime(created); err != nil {
		return Flag{}, fmt.Errorf("flag %d: created_at: %w", f.ID, err)
	}
	if f.UpdatedAt, err = db.ParseTime(updated); err != nil {
		return Flag{}, fmt.Errorf("flag %d: updated_at: %w", f.ID, err)
	}
	return f, nil
}

func optInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func optStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	s := v.String
	return &s
}

// Flags returns every flag, by id (without Resolved; see Engine.Flags). q may be nil.
func (s *Store) Flags(ctx context.Context, q Queryer) ([]Flag, error) {
	rows, err := s.reader(q).QueryContext(ctx, `SELECT `+flagColumns+` FROM item_flags ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read flags: %w", err)
	}
	defer rows.Close()
	out := []Flag{}
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, fmt.Errorf("read flags: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Flag returns one flag, or ErrNotFound.
func (s *Store) Flag(ctx context.Context, id int64) (Flag, error) {
	f, err := scanFlag(s.db.Reader().QueryRowContext(ctx, `SELECT `+flagColumns+` FROM item_flags WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Flag{}, fmt.Errorf("flag %d: %w", id, ErrNotFound)
	}
	return f, err
}

// DeleteFlag removes a flag (ErrNotFound when there is none). Nothing else changes: a file it
// protected is kept where it is backed up (S15) and simply follows the rules again.
func (s *Store) DeleteFlag(ctx context.Context, id int64) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM item_flags WHERE id = ?`, id)
		if err != nil {
			return fmt.Errorf("delete flag %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("flag %d: %w", id, ErrNotFound)
		}
		return nil
	})
}

// validRelPath reports whether p is "" (a whole source) or a clean relative slash path.
func validRelPath(p string) bool {
	if p == "" {
		return true
	}
	if p == "." || p == ".." || path.IsAbs(p) || strings.HasPrefix(p, "../") || strings.ContainsRune(p, 0) {
		return false
	}
	return path.Clean(p) == p
}

// checkFlagInput validates the parts of a flag that need no index.
func checkFlagInput(in FlagInput) (FlagInput, error) {
	if in.Flag == "" {
		in.Flag = FlagIrreplaceable
	}
	if in.Flag != FlagIrreplaceable {
		return in, invalid(-1, -1, "unknown flag %q (irreplaceable is the only one)", in.Flag)
	}
	in.Note = strings.TrimSpace(in.Note)
	if utf8.RuneCountInString(in.Note) > MaxNoteLen {
		return in, invalid(-1, -1, "the note is longer than %d characters", MaxNoteLen)
	}
	t := in.Target
	isArr := t.IntegrationID != 0 || t.Kind != "" || t.ArrID != 0
	isPath := t.SourceID != 0 || t.RelPath != nil
	switch {
	case isArr && isPath:
		return in, invalid(-1, -1, "a flag's target is an *arr item {integrationId, kind, arrId} or a path {sourceId, relPath}, not both")
	case isArr:
		if t.IntegrationID < 1 || t.ArrID < 1 || (t.Kind != mediaindex.KindMovie && t.Kind != mediaindex.KindSeries && t.Kind != mediaindex.KindArtist) {
			return in, invalid(-1, -1, "an *arr flag needs integrationId, kind (movie, series or artist) and arrId")
		}
	case isPath:
		if t.SourceID < 1 || t.RelPath == nil || !validRelPath(*t.RelPath) {
			return in, invalid(-1, -1, `a path flag needs sourceId and relPath (a clean relative path, or "" for the whole source)`)
		}
	default:
		return in, invalid(-1, -1, "a flag needs a target")
	}
	return in, nil
}

// insertFlag stores a new flag (ErrConflict when the same target is flagged already).
func (s *Store) insertFlag(ctx context.Context, f Flag) (Flag, error) {
	ext, err := json.Marshal(f.ExternalIDs)
	if err != nil {
		return Flag{}, err
	}
	now := db.FormatTime(s.now())
	var id int64
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if f.SourceID != nil {
			var n int64
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sources WHERE id = ?`, *f.SourceID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return invalid(-1, -1, "source %d does not exist", *f.SourceID)
			}
		}
		var dup int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM item_flags WHERE flag = ? AND ((kind = 'path' AND source_id IS ? AND rel_path IS ?)
			OR (kind = 'arr' AND integration_id IS ? AND arr_kind IS ? AND arr_id IS ?))`,
			f.Flag, f.SourceID, f.RelPath, f.IntegrationID, nullStr(f.ArrKind), nullInt(f.ArrID)).Scan(&dup); err != nil {
			return err
		}
		if dup > 0 {
			return fmt.Errorf("this target is already flagged %s: %w", f.Flag, ErrConflict)
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO item_flags (flag, kind, integration_id, arr_kind, arr_id, external_ids,
			last_source_id, last_rel_path, source_id, rel_path, note, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			f.Flag, f.Kind, f.IntegrationID, nullStr(f.ArrKind), nullInt(f.ArrID), string(ext), f.LastSourceID, f.LastRelPath,
			f.SourceID, f.RelPath, f.Note, now, now)
		if err != nil {
			return fmt.Errorf("save the flag: %w", err)
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Flag{}, err
	}
	return s.Flag(ctx, id)
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

// MoveFlagsTx makes an exact-file path flag follow a paired move (§8.7): the syncer calls it in
// the transaction that records the move of source sourceID's file from to to. A flag already at
// to absorbs the moved one.
func MoveFlagsTx(ctx context.Context, tx *sql.Tx, sourceID int64, from, to string, now time.Time) error {
	if from == to || from == "" {
		return nil
	}
	var exists int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM item_flags WHERE kind = 'path' AND source_id = ? AND rel_path = ?`,
		sourceID, to).Scan(&exists); err != nil {
		return fmt.Errorf("move flags: %w", err)
	}
	var err error
	if exists > 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM item_flags WHERE kind = 'path' AND source_id = ? AND rel_path = ?`, sourceID, from)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE item_flags SET rel_path = ?, updated_at = ? WHERE kind = 'path' AND source_id = ? AND rel_path = ?`,
			to, db.FormatTime(now), sourceID, from)
	}
	if err != nil {
		return fmt.Errorf("move flags: %w", err)
	}
	return nil
}

// Move is a paired move a sync executed: a file of a source renamed from From to To.
type Move struct {
	From string
	To   string
}

// FollowFolderMoves makes folder flags follow a folder rename (§8.7): when the moves take files
// from under folder F to under G with the same remainder, every move from under F agrees on G,
// and no live catalog file remains under F, a path flag on F, and an *arr flag whose last folder
// is F, move to G. It returns how many flags moved.
func (s *Store) FollowFolderMoves(ctx context.Context, sourceID int64, moves []Move) (int, error) {
	if len(moves) == 0 {
		return 0, nil
	}
	flags, err := s.Flags(ctx, nil)
	if err != nil {
		return 0, err
	}
	moved := 0
	for _, f := range flags {
		var folder string
		switch {
		case f.Kind == FlagKindPath && f.SourceID != nil && *f.SourceID == sourceID && f.RelPath != nil:
			folder = *f.RelPath
		case f.Kind == FlagKindArr && f.LastSourceID != nil && *f.LastSourceID == sourceID && f.LastRelPath != nil:
			folder = *f.LastRelPath
		default:
			continue
		}
		if folder == "" {
			continue // the whole source never moves
		}
		to, ok := renamedFolder(folder, moves)
		if !ok {
			continue
		}
		live, err := anyLiveUnder(ctx, s.db.Reader(), sourceID, folder)
		if err != nil {
			return moved, err
		}
		if live {
			continue
		}
		col := "rel_path"
		if f.Kind == FlagKindArr {
			col = "last_rel_path"
		}
		err = s.db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE item_flags SET `+col+` = ?, updated_at = ? WHERE id = ?`, to, db.FormatTime(s.now()), f.ID)
			return err
		})
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				continue // the new folder is flagged already
			}
			return moved, fmt.Errorf("move flag %d: %w", f.ID, err)
		}
		moved++
	}
	return moved, nil
}

// renamedFolder returns G when every move from under folder lands under one G with the same
// remainder, and at least one does.
func renamedFolder(folder string, moves []Move) (string, bool) {
	to := ""
	for _, m := range moves {
		rest, ok := strings.CutPrefix(m.From, folder+"/")
		if !ok {
			continue
		}
		g, ok := strings.CutSuffix(m.To, "/"+rest)
		if !ok || g == "" || g == folder {
			return "", false
		}
		if to != "" && to != g {
			return "", false
		}
		to = g
	}
	return to, to != ""
}

// setLastFolder records the located folder of a resolved *arr flag.
func (s *Store) setLastFolder(ctx context.Context, id, sourceID int64, rel string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE item_flags SET last_source_id = ?, last_rel_path = ?, updated_at = ? WHERE id = ? AND kind = 'arr'`,
			sourceID, rel, db.FormatTime(s.now()), id)
		return err
	})
}
