package tiers

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/mediaindex"
)

func (l *library) flagged(rel string) bool {
	l.t.Helper()
	got, _ := l.decisions(l.dest, l.movies)
	d, ok := got[rel]
	if !ok {
		l.t.Fatalf("no decision for %s", rel)
	}
	return d.RuleName == IrreplaceableRuleName || (d.Follows != "" && d.Tier == Full && d.RuleName == IrreplaceableRuleName)
}

func TestArrFlagFollowsTheItem(t *testing.T) {
	l := newLibrary(t)
	l.saveRules(RuleInput{Name: "skip everything", Action: Skip})
	f, err := l.eng.AddFlag(l.ctx, FlagInput{Target: FlagTarget{IntegrationID: l.radarr.ID, Kind: mediaindex.KindMovie, ArrID: 1}, Note: "  wedding  "})
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != FlagKindArr || f.ExternalIDs.TMDB != 949 || f.Note != "wedding" || f.LastRelPath == nil || *f.LastRelPath != "Heat (1995)" ||
		f.LastSourceID == nil || *f.LastSourceID != l.movies.ID {
		t.Fatalf("flag %+v", f)
	}
	heat := "Heat (1995)/Heat (1995).mkv"
	if !l.flagged(heat) || !l.flagged("Heat (1995)/Featurettes/Making of.mkv") || l.flagged("Charade (1963)/Charade (1963).mkv") {
		t.Fatal("the flag does not cover exactly Heat's folder")
	}
	if _, err := l.eng.AddFlag(l.ctx, FlagInput{Target: FlagTarget{IntegrationID: l.radarr.ID, Kind: mediaindex.KindMovie, ArrID: 1}}); !errors.Is(err, ErrConflict) {
		t.Errorf("a second flag on the item: %v", err)
	}

	t.Run("re-added in Radarr with a new id", func(t *testing.T) {
		l.exec(`UPDATE arr_items SET deleted_at = ? WHERE arr_id = 1`, "2026-01-01T00:00:00.000000000Z")
		id := l.item(l.radarr, itemSpec{arrID: 77, title: "Heat", path: "/movies/Heat (1995)", root: "/movies", ext: mediaindex.ExternalIDs{TMDB: 949}})
		l.arrFile(l.radarr, id, 771, "/movies/Heat (1995)/Heat (1995).mkv", "movies/Heat (1995)/Heat (1995).mkv", 5*mb, l.clock.Now())
		if !l.flagged(heat) {
			t.Error("the flag stopped applying after a re-add")
		}
		flags, err := l.eng.Flags(l.ctx)
		if err != nil || len(flags) != 1 || !flags[0].Resolved {
			t.Fatalf("flags %v %+v", err, flags)
		}
	})
	t.Run("an id reused by another movie", func(t *testing.T) {
		l.exec(`DELETE FROM arr_items WHERE arr_id = 1`) // the deleted item was purged
		l.exec(`UPDATE arr_items SET arr_id = 1, external_ids = '{"tmdb":5}', path = '/movies/Charade (1963)' WHERE arr_id = 2`)
		if l.flagged("Charade (1963)/Charade (1963).mkv") {
			t.Error("the flag followed the *arr id to another movie")
		}
	})
	t.Run("removed from Radarr with files kept", func(t *testing.T) {
		l.exec(`UPDATE arr_items SET deleted_at = ? WHERE arr_id = 77`, "2026-01-01T00:00:00.000000000Z")
		if !l.flagged(heat) {
			t.Error("the last folder does not cover the files")
		}
		flags, _ := l.eng.Flags(l.ctx)
		if flags[0].Resolved || !strings.Contains(flags[0].Reason, "no item with tmdb 949") || !strings.Contains(flags[0].Reason, "Heat (1995)") {
			t.Errorf("flag %+v", flags[0])
		}
		if id, ok, err := l.eng.Covered(l.ctx, l.movies.ID, heat); err != nil || !ok || id != f.ID {
			t.Errorf("covered %d %v %v", id, ok, err)
		}
		if _, ok, _ := l.eng.Covered(l.ctx, l.movies.ID, "Charade (1963)/Charade (1963).mkv"); ok {
			t.Error("Charade is covered")
		}
	})
	t.Run("the integration deleted", func(t *testing.T) {
		if err := l.ints.Delete(l.ctx, l.radarr.ID); err != nil {
			t.Fatal(err)
		}
		flags, err := l.eng.Store().Flags(l.ctx, nil)
		if err != nil || len(flags) != 1 || flags[0].IntegrationID != nil || flags[0].ExternalIDs.TMDB != 949 {
			t.Fatalf("flags after the delete %v %+v", err, flags)
		}
		if !l.flagged(heat) {
			t.Error("the flag stopped covering its last folder")
		}
	})
}

func TestPathFlagsFollowMoves(t *testing.T) {
	l := newLibrary(t)
	l.saveRules(RuleInput{Name: "skip everything", Action: Skip})
	file, err := l.eng.AddFlag(l.ctx, FlagInput{Target: FlagTarget{SourceID: l.movies.ID, RelPath: ptr("Charade (1963)/Charade (1963).mkv")}})
	if err != nil {
		t.Fatal(err)
	}
	folder, err := l.eng.AddFlag(l.ctx, FlagInput{Target: FlagTarget{SourceID: l.movies.ID, RelPath: ptr("Stray (2020)")}})
	if err != nil {
		t.Fatal(err)
	}
	if !l.flagged("Charade (1963)/Charade (1963).mkv") || !l.flagged("Stray (2020)/Stray (2020).mkv") || l.flagged("Heat (1995)/Heat (1995).mkv") {
		t.Fatal("path flags cover the wrong files")
	}
	var ve *ValidationError
	for _, bad := range []FlagInput{
		{Target: FlagTarget{SourceID: l.movies.ID, RelPath: ptr("../etc")}},
		{Target: FlagTarget{SourceID: l.movies.ID}},
		{Target: FlagTarget{SourceID: 999, RelPath: ptr("x")}},
		{Target: FlagTarget{SourceID: l.movies.ID, RelPath: ptr("x"), ArrID: 1}},
		{Flag: "favourite", Target: FlagTarget{SourceID: l.movies.ID, RelPath: ptr("x")}},
		{Target: FlagTarget{IntegrationID: l.radarr.ID, Kind: mediaindex.KindSeries, ArrID: 1}},
		{Target: FlagTarget{IntegrationID: l.radarr.ID, Kind: mediaindex.KindMovie, ArrID: 404}},
	} {
		if _, err := l.eng.AddFlag(l.ctx, bad); !errors.As(err, &ve) {
			t.Errorf("flag %+v: %v", bad, err)
		}
	}

	// A paired move of the exact file moves its flag, in the move's transaction.
	err = l.db.Write(l.ctx, func(tx *sql.Tx) error {
		return l.eng.MoveFlagsTx(l.ctx, tx, l.movies.ID, "Charade (1963)/Charade (1963).mkv", "Charade (1963)/Charade.mkv")
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.eng.Store().Flag(l.ctx, file.ID)
	if err != nil || *got.RelPath != "Charade (1963)/Charade.mkv" {
		t.Fatalf("moved flag %v %+v", err, got)
	}
	if err := os.Rename(filepath.Join(l.root, "movies/Charade (1963)/Charade (1963).mkv"), filepath.Join(l.root, "movies/Charade (1963)/Charade.mkv")); err != nil {
		t.Fatal(err)
	}

	// A folder rename: files moved from Stray (2020)/ to Stray (2021)/ with the same remainder,
	// and nothing live left under the old folder.
	moves := []Move{{From: "Stray (2020)/Stray (2020).mkv", To: "Stray (2021)/Stray (2020).mkv"}}
	if n, err := l.eng.Store().FollowFolderMoves(l.ctx, l.movies.ID, moves); err != nil || n != 0 {
		t.Fatalf("followed while the old folder is still live: %d %v", n, err)
	}
	if err := os.Rename(filepath.Join(l.root, "movies/Stray (2020)"), filepath.Join(l.root, "movies/Stray (2021)")); err != nil {
		t.Fatal(err)
	}
	l.movies = l.scan(l.movies)
	if n, err := l.eng.Store().FollowFolderMoves(l.ctx, l.movies.ID, []Move{moves[0], {From: "Other/x.mkv", To: "Else/x.mkv"}}); err != nil || n != 1 {
		t.Fatalf("followed %d %v", n, err)
	}
	got, _ = l.eng.Store().Flag(l.ctx, folder.ID)
	if *got.RelPath != "Stray (2021)" || !l.flagged("Stray (2021)/Stray (2020).mkv") {
		t.Fatalf("folder flag %+v", got)
	}
	// Moves that disagree on the new folder move nothing.
	if _, ok := renamedFolder("A", []Move{{From: "A/1", To: "B/1"}, {From: "A/2", To: "C/2"}}); ok {
		t.Error("disagreeing moves followed")
	}
	if to, ok := renamedFolder("A", []Move{{From: "A/s/1", To: "B/s/1"}}); !ok || to != "B" {
		t.Errorf("nested remainder: %q %v", to, ok)
	}

	// Deleting a flag: the file follows the rules again.
	if err := l.eng.Store().DeleteFlag(l.ctx, folder.ID); err != nil {
		t.Fatal(err)
	}
	if l.flagged("Stray (2021)/Stray (2020).mkv") {
		t.Error("a deleted flag still applies")
	}
	if err := l.eng.Store().DeleteFlag(l.ctx, folder.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	flags, err := l.eng.Flags(l.ctx)
	if err != nil || len(flags) != 1 || !flags[0].Resolved {
		t.Fatalf("flags %v %+v", err, flags)
	}
	// A path flag whose file is gone is not resolved.
	l.exec(`UPDATE item_flags SET rel_path = 'Nowhere' WHERE id = ?`, file.ID)
	flags, _ = l.eng.Flags(l.ctx)
	if flags[0].Resolved || flags[0].Reason != "folder not found" {
		t.Errorf("gone path flag %+v", flags[0])
	}
}
