package mediaindex

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// metaRow is one arr_meta row to store.
type metaRow struct {
	kind   string
	id     int64
	name   string
	detail string
}

// metaRows turns fetched metadata into arr_meta rows: root folders mapped and located (and the
// inaccessible ones remembered and warned about).
func (rn *run) metaRows(m meta) []metaRow {
	rows := []metaRow{}
	for _, q := range m.qualityProfiles {
		rows = append(rows, metaRow{kind: metaQualityProfile, id: q.ID, name: q.Name, detail: "{}"})
	}
	for _, q := range m.metadataProfiles {
		rows = append(rows, metaRow{kind: metaMetadataProfile, id: q.ID, name: q.Name, detail: "{}"})
	}
	for _, t := range m.tags {
		rows = append(rows, metaRow{kind: metaTag, id: t.ID, name: t.Label, detail: "{}"})
	}
	rn.inaccessible = nil
	rn.stats.InaccessibleRootFolders = []string{}
	rn.rootSources = map[int64]bool{}
	for _, rf := range m.rootFolders {
		d := rootFolderDetail{Accessible: rf.Accessible}
		root := strings.TrimRight(rf.Path, "/")
		if local, locs := rn.mapLocate(cleanArrPath(root)); local != "" {
			d.LocalPath = &local
			if len(locs) > 0 {
				id := locs[0].SourceID
				d.SourceID = &id
			}
			for _, l := range locs {
				rn.rootSources[l.SourceID] = true
			}
		}
		if !rf.Accessible {
			rn.inaccessible = append(rn.inaccessible, root)
			rn.stats.InaccessibleRootFolders = append(rn.stats.InaccessibleRootFolders, root)
			rn.warn(fmt.Sprintf("%s reports its root folder %s as not accessible (an unmounted share?): the index keeps the files under it until %s can read it again",
				rn.app, root, rn.app))
		}
		b, _ := json.Marshal(d)
		rows = append(rows, metaRow{kind: metaRootFolder, id: rf.ID, name: root, detail: string(b)})
	}
	return rows
}

// knownMeta is what the index knows of an *arr's metadata: the ids a targeted refresh checks.
type knownMeta struct {
	tags, qualityProfiles, metadataProfiles map[int64]bool
	inaccessible                            []string
}

func (s *Store) knownMeta(ctx context.Context, integrationID int64) (*knownMeta, error) {
	m, err := s.Meta(ctx, nil, integrationID)
	if err != nil {
		return nil, err
	}
	k := &knownMeta{tags: map[int64]bool{}, qualityProfiles: map[int64]bool{}, metadataProfiles: map[int64]bool{}}
	for _, t := range m.Tags {
		k.tags[t.ID] = true
	}
	for _, q := range m.QualityProfiles {
		k.qualityProfiles[q.ID] = true
	}
	for _, q := range m.MetadataProfiles {
		k.metadataProfiles[q.ID] = true
	}
	for _, rf := range m.RootFolders {
		if !rf.Accessible {
			k.inaccessible = append(k.inaccessible, rf.Path)
		}
	}
	return k, nil
}

// missing reports whether an item names a tag or profile the index does not know.
func (k *knownMeta) missing(items []fetched) bool {
	for _, it := range items {
		for _, t := range it.tags {
			if !k.tags[t] {
				return true
			}
		}
		if it.qp != 0 && !k.qualityProfiles[it.qp] {
			return true
		}
		if it.mp != 0 && !k.metadataProfiles[it.mp] {
			return true
		}
	}
	return false
}

// begin is the attempt's first write: a cache built from another URL is replaced (its rows are
// deleted and refreshed_at cleared, so a partial new cache is never fresh), the metadata replaced
// when metaRows is not nil, and the attempt recorded.
func (rn *run) begin(ctx context.Context, version string, metaRows []metaRow) error {
	id := rn.it.ID
	err := rn.r.o.DB.Write(ctx, func(tx *sql.Tx) error {
		if rn.instanceChanged {
			if _, err := tx.ExecContext(ctx, `DELETE FROM arr_items WHERE integration_id = ?`, id); err != nil {
				return fmt.Errorf("replace the cache of another instance: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM arr_meta WHERE integration_id = ?`, id); err != nil {
				return fmt.Errorf("replace the cache of another instance: %w", err)
			}
		}
		if metaRows != nil {
			if err := replaceMeta(ctx, tx, id, metaRows); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO index_state (integration_id, status, attempted_at, instance_id, app_version)
			VALUES (?, 'never', ?, ?, ?)
			ON CONFLICT (integration_id) DO UPDATE SET attempted_at = excluded.attempted_at, instance_id = excluded.instance_id,
				app_version = CASE WHEN excluded.app_version <> '' THEN excluded.app_version ELSE index_state.app_version END,
				refreshed_at = CASE WHEN ? THEN NULL ELSE index_state.refreshed_at END`,
			id, db.FormatTime(rn.runAt), rn.it.URL, version, rn.instanceChanged)
		if err != nil {
			return fmt.Errorf("record the refresh: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if rn.instanceChanged {
		rn.info("The index described another URL of this integration; it is replaced by this refresh")
	}
	rn.started = true
	return nil
}

func replaceMeta(ctx context.Context, tx *sql.Tx, id int64, rows []metaRow) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM arr_meta WHERE integration_id = ?`, id); err != nil {
		return fmt.Errorf("replace the metadata: %w", err)
	}
	for _, m := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO arr_meta (integration_id, kind, arr_id, name, detail) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (integration_id, kind, arr_id) DO UPDATE SET name = excluded.name, detail = excluded.detail`,
			id, m.kind, m.id, m.name, m.detail); err != nil {
			return fmt.Errorf("replace the metadata: %w", err)
		}
	}
	return nil
}

// add queues a fetched item for the next batch write.
func (rn *run) add(ctx context.Context, f fetched) error {
	rn.batch = append(rn.batch, f)
	rn.batchRows += 1 + len(f.files)
	if rn.batchRows >= rn.r.o.BatchSize {
		return rn.flush(ctx)
	}
	return nil
}

// itemRow is an arr_items row as stored (JSON columns as text), for writing and comparing.
type itemRow struct {
	kind, title, ext, path, root, tags, genres, added, detail string
	arrID, qp, mp                                             int64
	year                                                      int
	monitored                                                 bool
}

func toItemRow(f fetched) itemRow {
	ext, _ := json.Marshal(f.ext)
	tags, _ := json.Marshal(f.tags)
	genres, _ := json.Marshal(f.genres)
	detail, _ := json.Marshal(f.detail)
	r := itemRow{kind: f.kind, arrID: f.arrID, title: f.title, year: f.year, ext: string(ext), path: f.path, root: f.rootFolder,
		qp: f.qp, mp: f.mp, monitored: f.monitored, tags: string(tags), genres: string(genres), detail: string(detail)}
	if f.added != nil {
		r.added = db.FormatTime(*f.added)
	}
	return r
}

// fileRow is an arr_files row to write.
type fileRow struct {
	arrFileID int64
	path      string
	local     string
	loc       *catalog.Location
	size      int64
	quality   string
	dateAdded string
	detail    string
}

// existingItem is what the index holds for an item before the batch is written.
type existingItem struct {
	id      int64
	row     itemRow
	deleted bool
	// files maps the *arr file id to its path.
	files map[int64]string
}

// existing reads the index rows of the batch's items.
func (s *Store) existing(ctx context.Context, integrationID int64, kind string, arrIDs []int64) (map[int64]*existingItem, error) {
	out := make(map[int64]*existingItem, len(arrIDs))
	byRow := map[int64]*existingItem{}
	q := s.db.Reader()
	for rest := arrIDs; len(rest) > 0; {
		n := min(len(rest), 500)
		chunk := rest[:n]
		rest = rest[n:]
		args := []any{integrationID, kind}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := q.QueryContext(ctx, `SELECT id, arr_id, title, year, external_ids, path, root_folder, quality_profile_id,
			metadata_profile_id, monitored, tags, genres, COALESCE(added_at, ''), detail, deleted_at IS NOT NULL
			FROM arr_items WHERE integration_id = ? AND kind = ? AND arr_id IN (`+placeholders(n)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read indexed items: %w", err)
		}
		for rows.Next() {
			e := &existingItem{files: map[int64]string{}}
			r := &e.row
			r.kind = kind
			if err := rows.Scan(&e.id, &r.arrID, &r.title, &r.year, &r.ext, &r.path, &r.root, &r.qp, &r.mp, &r.monitored,
				&r.tags, &r.genres, &r.added, &r.detail, &e.deleted); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("read indexed items: %w", err)
			}
			out[r.arrID] = e
			byRow[e.id] = e
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("read indexed items: %w", err)
		}
	}
	ids := slices.Collect(maps.Keys(byRow))
	for len(ids) > 0 {
		n := min(len(ids), 500)
		chunk := ids[:n]
		ids = ids[n:]
		args := make([]any, n)
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := q.QueryContext(ctx, `SELECT item_id, arr_file_id, path FROM arr_files WHERE item_id IN (`+placeholders(n)+`)`, args...)
		if err != nil {
			return nil, fmt.Errorf("read indexed files: %w", err)
		}
		for rows.Next() {
			var (
				itemID, fileID int64
				p              string
			)
			if err := rows.Scan(&itemID, &fileID, &p); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("read indexed files: %w", err)
			}
			if e := byRow[itemID]; e != nil {
				e.files[fileID] = p
			}
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("read indexed files: %w", err)
		}
	}
	return out, nil
}

// sameFiles reports whether an item's (file id, path) set is unchanged.
func sameFiles(old map[int64]string, files []fetchedFile) bool {
	if len(old) != len(files) {
		return false
	}
	for _, f := range files {
		if p, ok := old[f.arrFileID]; !ok || p != f.path {
			return false
		}
	}
	return true
}

// flush writes the batch: it compares each item with the index (counts and the changed set),
// maps and locates its files, stores the batch's intents, then upserts items and files in one
// transaction. A dry run only counts.
func (rn *run) flush(ctx context.Context) error {
	if len(rn.batch) == 0 {
		return nil
	}
	batch := rn.batch
	rn.batch, rn.batchRows = nil, 0
	var existing map[int64]*existingItem
	if !(rn.dry && rn.instanceChanged) {
		ids := make([]int64, len(batch))
		for i, f := range batch {
			ids[i] = f.arrID
		}
		var err error
		if existing, err = rn.r.store.existing(ctx, rn.it.ID, rn.kind, ids); err != nil {
			return err
		}
	}
	targeted := rn.stats.Targeted
	type prepared struct {
		row   itemRow
		files []fileRow
	}
	prep := make([]prepared, len(batch))
	var (
		intents []intent
		located []File
	)
	for i, f := range batch {
		row := toItemRow(f)
		ex := existing[f.arrID]
		switch {
		case ex == nil:
			rn.stats.ItemsAdded++
		case ex.deleted || ex.row != row:
			rn.stats.ItemsUpdated++
		}
		hasFiles := f.detail.HasFile || len(f.files) > 0
		switch {
		case targeted:
			in := intent{Kind: f.kind, ArrID: f.arrID, Title: f.title, NewFolder: f.path, RootFolder: f.rootFolder, HasFiles: hasFiles, Reason: "requested"}
			if ex != nil && ex.row.path != f.path {
				in.OldFolder = ex.row.path
			}
			intents = append(intents, in)
		case rn.first:
		case ex == nil && hasFiles:
			intents = append(intents, intent{Kind: f.kind, ArrID: f.arrID, Title: f.title, NewFolder: f.path, RootFolder: f.rootFolder, HasFiles: true, Reason: "added"})
		case ex != nil && ex.row.path != f.path:
			intents = append(intents, intent{Kind: f.kind, ArrID: f.arrID, Title: f.title, OldFolder: ex.row.path, NewFolder: f.path, RootFolder: f.rootFolder, HasFiles: hasFiles, Reason: "moved"})
		case ex != nil && (!sameFiles(ex.files, f.files) || ex.deleted && hasFiles):
			intents = append(intents, intent{Kind: f.kind, ArrID: f.arrID, Title: f.title, NewFolder: f.path, RootFolder: f.rootFolder, HasFiles: hasFiles, Reason: "files"})
		}
		prep[i].row = row
		if targeted && rn.dry && ex != nil {
			// What the write would remove: the item's files the *arr no longer lists.
			listed := map[int64]bool{}
			for _, ff := range f.files {
				listed[ff.arrFileID] = true
			}
			for id, p := range ex.files {
				if !listed[id] && !rn.underInaccessible(p) {
					rn.stats.FilesDeleted++
				}
			}
		}
		for _, ff := range f.files {
			rn.stats.Files++
			d, _ := json.Marshal(ff.detail)
			fr := fileRow{arrFileID: ff.arrFileID, path: ff.path, size: ff.size, quality: ff.quality, detail: string(d)}
			if ff.dateAdded != nil {
				fr.dateAdded = db.FormatTime(*ff.dateAdded)
			}
			local, locs := rn.mapLocate(cleanArrPath(ff.path))
			fr.local = local
			switch {
			case local == "":
				rn.stats.FilesUnmapped++
				rn.countUnmapped(f.rootFolder, ReasonUnmapped)
			case len(locs) == 0:
				rn.stats.FilesUnmapped++
				rn.countUnmapped(f.rootFolder, ReasonNoSource)
			default:
				rn.stats.FilesMapped++
				l := locs[0]
				fr.loc = &l
				located = append(located, File{LocalPath: local, Size: ff.size})
			}
			prep[i].files = append(prep[i].files, fr)
			if rn.dry {
				rn.seenFiles[ff.arrFileID] = true
			}
		}
		if rn.dry {
			rn.seenItems[f.arrID] = true
		}
	}
	if len(located) > 0 {
		m, err := newMatcher(ctx, rn.r.o.Catalog, rn.loc, located)
		if err != nil {
			return err
		}
		for _, f := range located {
			if ok, _ := m.match(f.LocalPath, f.Size); !ok {
				rn.stats.FilesMismatched++
			}
		}
	}
	if !targeted {
		rn.stats.ChangedItems += int64(len(intents))
	}
	rn.intents = append(rn.intents, intents...)
	if rn.dry {
		return nil
	}
	if rn.wantIntents && len(intents) > 0 {
		if err := rn.persistIntents(ctx, intents); err != nil {
			return err
		}
	}
	seen := db.FormatTime(rn.runAt)
	err := rn.r.o.DB.Write(ctx, func(tx *sql.Tx) error {
		itemStmt, err := tx.PrepareContext(ctx, `INSERT INTO arr_items (integration_id, kind, arr_id, title, year, external_ids, path,
				root_folder, quality_profile_id, metadata_profile_id, monitored, tags, genres, added_at, detail, seen_at, deleted_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)
			ON CONFLICT (integration_id, kind, arr_id) DO UPDATE SET title = excluded.title, year = excluded.year,
				external_ids = excluded.external_ids, path = excluded.path, root_folder = excluded.root_folder,
				quality_profile_id = excluded.quality_profile_id, metadata_profile_id = excluded.metadata_profile_id,
				monitored = excluded.monitored, tags = excluded.tags, genres = excluded.genres, added_at = excluded.added_at,
				detail = excluded.detail, seen_at = excluded.seen_at, deleted_at = NULL
			RETURNING id`)
		if err != nil {
			return fmt.Errorf("write indexed items: %w", err)
		}
		defer itemStmt.Close()
		fileStmt, err := tx.PrepareContext(ctx, `INSERT INTO arr_files (integration_id, item_id, arr_file_id, path, local_path,
				source_id, rel_path, size, quality, date_added, detail, seen_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (integration_id, arr_file_id) DO UPDATE SET item_id = excluded.item_id, path = excluded.path,
				local_path = excluded.local_path, source_id = excluded.source_id, rel_path = excluded.rel_path,
				size = excluded.size, quality = excluded.quality, date_added = excluded.date_added, detail = excluded.detail,
				seen_at = excluded.seen_at`)
		if err != nil {
			return fmt.Errorf("write indexed files: %w", err)
		}
		defer fileStmt.Close()
		for _, p := range prep {
			r := p.row
			var itemID int64
			if err := itemStmt.QueryRowContext(ctx, rn.it.ID, r.kind, r.arrID, r.title, r.year, r.ext, r.path, r.root, r.qp, r.mp,
				r.monitored, r.tags, r.genres, nullText(r.added), r.detail, seen).Scan(&itemID); err != nil {
				return fmt.Errorf("write indexed item %d: %w", r.arrID, err)
			}
			for _, fr := range p.files {
				var (
					src any
					rel any
				)
				if fr.loc != nil {
					src, rel = fr.loc.SourceID, fr.loc.Rel
				}
				if _, err := fileStmt.ExecContext(ctx, rn.it.ID, itemID, fr.arrFileID, fr.path, nullText(fr.local), src, rel,
					fr.size, fr.quality, nullText(fr.dateAdded), fr.detail, seen); err != nil {
					return fmt.Errorf("write indexed file %d: %w", fr.arrFileID, err)
				}
			}
			if targeted {
				// A targeted refresh read the item's complete file list: files it did not see are
				// gone (a same-path replacement has a new id), except under an inaccessible root.
				n, err := rn.deleteUnseenFilesOf(ctx, tx, itemID, seen)
				if err != nil {
					return err
				}
				rn.stats.FilesDeleted += n
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	faultinject.Point(PointAfterBatch)
	rn.env.Reporter.Progress(jobs.Progress{Phase: "refreshing", FilesDone: rn.stats.Items, CurrentFile: batch[len(batch)-1].title})
	return nil
}

// deleteUnseenFilesOf deletes an item's files that this attempt did not see, except those under
// an inaccessible root folder.
func (rn *run) deleteUnseenFilesOf(ctx context.Context, tx *sql.Tx, itemID int64, seen string) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, path FROM arr_files WHERE item_id = ? AND seen_at <> ?`, itemID, seen)
	if err != nil {
		return 0, fmt.Errorf("read indexed files: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var (
			id int64
			p  string
		)
		if err := rows.Scan(&id, &p); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("read indexed files: %w", err)
		}
		if !rn.underInaccessible(p) {
			ids = append(ids, id)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("read indexed files: %w", err)
	}
	if err := deleteIDs(ctx, tx, `DELETE FROM arr_files WHERE id IN (%s)`, ids); err != nil {
		return 0, fmt.Errorf("remove indexed files: %w", err)
	}
	return int64(len(ids)), nil
}

// underInaccessible reports whether an *arr path lies under a root folder the *arr reports as
// not accessible.
func (rn *run) underInaccessible(p string) bool {
	for _, root := range rn.inaccessible {
		if under(p, root) {
			return true
		}
	}
	return false
}

// deleteIDs runs a statement with "%s" replaced by the placeholders of each chunk of ids.
func deleteIDs(ctx context.Context, tx *sql.Tx, stmt string, ids []int64) error {
	for len(ids) > 0 {
		n := min(len(ids), 500)
		args := make([]any, n)
		for i, id := range ids[:n] {
			args[i] = id
		}
		ids = ids[n:]
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(stmt, placeholders(n)), args...); err != nil {
			return err
		}
	}
	return nil
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// goneItem is an indexed item the complete fetch did not see.
type goneItem struct {
	id       int64
	kind     string
	arrID    int64
	title    string
	path     string
	root     string
	hadFiles bool
}

// goneFile is an indexed file the complete fetch did not see.
type goneFile struct {
	id     int64
	itemID int64
	path   string
}

type goneSet struct {
	items []goneItem
	files []goneFile
}

// goneRows returns what the complete fetch did not see: rows whose seen_at is not this attempt's
// (a dry run, which writes nothing, compares with the ids it saw). A dry run of a cache that is
// replaced (another instance) removes nothing.
func (rn *run) goneRows(ctx context.Context) (goneSet, error) {
	var out goneSet
	if rn.dry && rn.instanceChanged {
		return out, nil
	}
	q := rn.r.store.db.Reader()
	seen := db.FormatTime(rn.runAt)
	itemQuery := `SELECT i.id, i.kind, i.arr_id, i.title, i.path, i.root_folder, i.detail,
			EXISTS (SELECT 1 FROM arr_files f WHERE f.item_id = i.id)
		FROM arr_items i WHERE i.integration_id = ? AND i.deleted_at IS NULL AND i.seen_at <> ? ORDER BY i.arr_id`
	fileQuery := `SELECT id, item_id, path, arr_file_id FROM arr_files WHERE integration_id = ? AND seen_at <> ? ORDER BY id`
	args := []any{rn.it.ID, seen}
	if rn.dry {
		itemQuery = strings.Replace(itemQuery, "AND i.seen_at <> ?", "AND ? <> ''", 1)
		fileQuery = strings.Replace(fileQuery, "AND seen_at <> ?", "AND ? <> ''", 1)
	}
	rows, err := q.QueryContext(ctx, itemQuery, args...)
	if err != nil {
		return out, fmt.Errorf("read indexed items: %w", err)
	}
	for rows.Next() {
		var (
			g      goneItem
			detail string
			files  bool
		)
		if err := rows.Scan(&g.id, &g.kind, &g.arrID, &g.title, &g.path, &g.root, &detail, &files); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("read indexed items: %w", err)
		}
		if rn.dry && rn.seenItems[g.arrID] {
			continue
		}
		var d ItemDetail
		_ = json.Unmarshal([]byte(detail), &d)
		g.hadFiles = files || d.HasFile
		out.items = append(out.items, g)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("read indexed items: %w", err)
	}
	rows, err = q.QueryContext(ctx, fileQuery, args...)
	if err != nil {
		return out, fmt.Errorf("read indexed files: %w", err)
	}
	for rows.Next() {
		var (
			g      goneFile
			fileID int64
		)
		if err := rows.Scan(&g.id, &g.itemID, &g.path, &fileID); err != nil {
			_ = rows.Close()
			return out, fmt.Errorf("read indexed files: %w", err)
		}
		if rn.dry && rn.seenFiles[fileID] {
			continue
		}
		out.files = append(out.files, g)
	}
	if err := rows.Close(); err != nil {
		return out, fmt.Errorf("read indexed files: %w", err)
	}
	return out, nil
}

// guardDecision is what the full refresh removes from the index, and why it holds the rest.
type guardDecision struct {
	items []goneItem
	files []goneFile
	held  string
}

// guard applies the refresh guard (S10): files under an inaccessible root folder are never
// removed; more than half of the items (and more than GuardMinimum), an empty item list while the
// index has items, or more than half of the files (and more than GuardMinimum) are held unless
// the job has AllowChanges. Held items keep their files.
func (rn *run) guard(g goneSet, itemsBefore, filesBefore int64) guardDecision {
	var d guardDecision
	allow := rn.job.Params.AllowChanges
	var reasons []string
	nItems := int64(len(g.items))
	noun := map[string]string{KindMovie: "movies", KindSeries: "series", KindArtist: "artists"}[rn.kind]
	itemsHeld := false
	switch {
	case nItems == 0:
	case rn.stats.Items == 0 && itemsBefore > 0:
		itemsHeld = true
		reasons = append(reasons, fmt.Sprintf("%s listed no %s while the index has %d", rn.app, noun, itemsBefore))
	case nItems*2 > itemsBefore && nItems > GuardMinimum:
		itemsHeld = true
		reasons = append(reasons, fmt.Sprintf("%d of %d %s would be marked deleted", nItems, itemsBefore, noun))
	}
	heldItems := map[int64]bool{}
	if itemsHeld && !allow {
		for _, it := range g.items {
			heldItems[it.id] = true
		}
	} else {
		d.items = g.items
	}
	var files []goneFile
	for _, f := range g.files {
		if rn.underInaccessible(f.path) || heldItems[f.itemID] {
			continue
		}
		files = append(files, f)
	}
	nFiles := int64(len(files))
	filesHeld := nFiles > 0 && nFiles*2 > filesBefore && nFiles > GuardMinimum
	if filesHeld {
		reasons = append(reasons, fmt.Sprintf("%d of %d files would be removed", nFiles, filesBefore))
	}
	if !filesHeld || allow {
		d.files = files
	}
	if len(reasons) > 0 {
		if allow {
			rn.info("Applying the removals the refresh guard would hold (allowChanges): " + strings.Join(reasons, "; "))
		} else {
			d.held = strings.Join(reasons, "; ")
		}
	}
	return d
}

// storedStats are index_state.stats: the counts of the last full refresh and its notes.
type storedStats struct {
	Items                   int64            `json:"items"`
	Files                   int64            `json:"files"`
	FilesMapped             int64            `json:"filesMapped"`
	FilesUnmapped           int64            `json:"filesUnmapped"`
	FilesMismatched         int64            `json:"filesMismatched"`
	UnmappedFolders         []UnmappedFolder `json:"unmappedFolders"`
	InaccessibleRootFolders []string         `json:"inaccessibleRootFolders"`
	RecycleBin              *RecycleBin      `json:"recycleBin"`
	FileDate                string           `json:"fileDate,omitempty"`
	GuardHeld               bool             `json:"guardHeld"`
}

// finishFull applies the guard's decision and completes the full refresh in one transaction:
// items marked deleted, files removed, items deleted for PurgeAfter purged, and the state set
// (refreshed_at moves unless the guard held something).
func (rn *run) finishFull(ctx context.Context, d guardDecision) error {
	st := storedStats{Items: rn.stats.Items, Files: rn.stats.Files, FilesMapped: rn.stats.FilesMapped, FilesUnmapped: rn.stats.FilesUnmapped,
		FilesMismatched: rn.stats.FilesMismatched, UnmappedFolders: rn.stats.UnmappedFolders,
		InaccessibleRootFolders: rn.stats.InaccessibleRootFolders, RecycleBin: rn.stats.RecycleBin, FileDate: rn.stats.FileDate,
		GuardHeld: d.held != ""}
	stats, err := json.Marshal(st)
	if err != nil {
		return err
	}
	seen := db.FormatTime(rn.runAt)
	var purged int64
	err = rn.r.o.DB.Write(ctx, func(tx *sql.Tx) error {
		ids := make([]int64, len(d.items))
		for i, it := range d.items {
			ids[i] = it.id
		}
		if err := deleteIDs(ctx, tx, `UPDATE arr_items SET deleted_at = '`+seen+`' WHERE id IN (%s)`, ids); err != nil {
			return fmt.Errorf("mark indexed items deleted: %w", err)
		}
		fids := make([]int64, len(d.files))
		for i, f := range d.files {
			fids[i] = f.id
		}
		if err := deleteIDs(ctx, tx, `DELETE FROM arr_files WHERE id IN (%s)`, fids); err != nil {
			return fmt.Errorf("remove indexed files: %w", err)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM arr_items WHERE integration_id = ? AND deleted_at IS NOT NULL AND deleted_at < ?`,
			rn.it.ID, db.FormatTime(rn.runAt.Add(-PurgeAfter)))
		if err != nil {
			return fmt.Errorf("purge deleted items: %w", err)
		}
		purged, _ = res.RowsAffected()
		var errText any
		if d.held != "" {
			errText = "Refresh guard held the removals: " + d.held
		}
		_, err = tx.ExecContext(ctx, `UPDATE index_state SET status = 'ok', error = ?, stats = ?, instance_id = ?,
				refreshed_at = CASE WHEN ? THEN refreshed_at ELSE ? END
			WHERE integration_id = ?`, errText, string(stats), rn.it.URL, d.held != "", seen, rn.it.ID)
		if err != nil {
			return fmt.Errorf("record the refresh: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if purged > 0 {
		rn.info("Purged items deleted in the *arr more than 30 days ago", "items", purged)
	}
	return nil
}

// markTargetedGone marks the items a targeted refresh got 404 for deleted, with their files.
func (rn *run) markTargetedGone(ctx context.Context, gone []int64) error {
	var (
		ids     []int64
		intents []intent
	)
	for _, arrID := range gone {
		it, err := rn.r.store.Item(ctx, nil, rn.it.ID, rn.kind, arrID)
		if errors.Is(err, ErrItemNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if it.DeletedAt != nil || rn.instanceChanged {
			continue
		}
		ids = append(ids, it.ID)
		rn.stats.ItemsDeleted++
		intents = append(intents, intent{Kind: it.Kind, ArrID: it.ArrID, Title: it.Title, OldFolder: it.Path, RootFolder: it.RootFolder, Reason: "deleted"})
	}
	rn.intents = append(rn.intents, intents...)
	if rn.dry || len(ids) == 0 {
		return nil
	}
	if rn.wantIntents && len(intents) > 0 {
		if err := rn.persistIntents(ctx, intents); err != nil {
			return err
		}
	}
	seen := db.FormatTime(rn.runAt)
	return rn.r.o.DB.Write(ctx, func(tx *sql.Tx) error {
		if err := deleteIDs(ctx, tx, `UPDATE arr_items SET deleted_at = '`+seen+`' WHERE id IN (%s)`, ids); err != nil {
			return fmt.Errorf("mark indexed items deleted: %w", err)
		}
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `DELETE FROM arr_files WHERE item_id = ?`, id)
			if err != nil {
				return fmt.Errorf("remove indexed files: %w", err)
			}
			n, _ := res.RowsAffected()
			rn.stats.FilesDeleted += n
		}
		return nil
	})
}

// recordTargeted completes a targeted refresh: the attempt succeeded; refreshed_at stays.
func (s *Store) recordTargeted(ctx context.Context, id int64, runAt time.Time, version string) error {
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE index_state SET status = 'ok', error = NULL, attempted_at = ?,
			app_version = CASE WHEN ? <> '' THEN ? ELSE app_version END WHERE integration_id = ?`,
			db.FormatTime(runAt), version, version, id)
		if err != nil {
			return fmt.Errorf("record the refresh: %w", err)
		}
		return nil
	})
}

// maxErrorLen bounds a recorded refresh error.
const maxErrorLen = 1000

// recordFailure records a failed attempt; the cache, refreshed_at and the instance stay.
func (s *Store) recordFailure(ctx context.Context, id int64, runAt time.Time, cause error) error {
	msg := cause.Error()
	if len(msg) > maxErrorLen {
		msg = msg[:maxErrorLen]
	}
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO index_state (integration_id, status, attempted_at, error) VALUES (?, 'failed', ?, ?)
			ON CONFLICT (integration_id) DO UPDATE SET status = 'failed', attempted_at = excluded.attempted_at, error = excluded.error`,
			id, db.FormatTime(runAt), msg)
		return err
	})
}

// counts returns an integration's live items and its files.
func (s *Store) counts(ctx context.Context, id int64) (items, files int64, err error) {
	q := s.db.Reader()
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM arr_items WHERE integration_id = ? AND deleted_at IS NULL`, id).Scan(&items); err != nil {
		return 0, 0, fmt.Errorf("count indexed items: %w", err)
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM arr_files WHERE integration_id = ?`, id).Scan(&files); err != nil {
		return 0, 0, fmt.Errorf("count indexed files: %w", err)
	}
	return items, files, nil
}

// strandedIntentJobs returns, in id order, the refresh jobs of an integration other than jobID
// that ended with reconcile intents or a root-folder sync mark (an overflow refresh) still
// pending: failed, or cancelled without syncAfter (a cancelled webhook refresh has its events
// re-armed, and the refresh they queue follows up every requested item). A runner that sees its
// job fail or get cancelled follows its intents and mark up; these
// ended without the runner: start-up recovery failed them after too many crashes, or a resumed
// job was cancelled before it started again (jobqueue finishes both without running them).
func (s *Store) strandedIntentJobs(ctx context.Context, integrationID, jobID int64) ([]int64, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT j.id FROM jobs j
		WHERE j.integration_id = ? AND j.type = 'refresh' AND j.dry_run = 0 AND j.id <> ?
			AND (j.status = 'failed' OR (j.status = 'cancelled'
				AND COALESCE(CASE WHEN json_valid(j.params) THEN json_extract(j.params, '$.syncAfter') END, 0) = 0))
			AND EXISTS (SELECT 1 FROM job_items ji WHERE ji.job_id = j.id AND ji.status = 'pending' AND ji.action = 'skip')
		ORDER BY j.id`, integrationID, jobID)
	if err != nil {
		return nil, fmt.Errorf("read earlier refreshes with pending changes: %w", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read earlier refreshes with pending changes: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read earlier refreshes with pending changes: %w", err)
	}
	return ids, nil
}

// lastSeen returns the latest seen_at of an integration's rows and its last attempt (the zero
// time when there is none).
func (s *Store) lastSeen(ctx context.Context, id int64) (time.Time, error) {
	var last sql.NullString
	err := s.db.Reader().QueryRowContext(ctx, `SELECT MAX(t) FROM (
			SELECT MAX(seen_at) AS t FROM arr_items WHERE integration_id = ?
			UNION ALL SELECT MAX(seen_at) FROM arr_files WHERE integration_id = ?
			UNION ALL SELECT attempted_at FROM index_state WHERE integration_id = ?)`, id, id, id).Scan(&last)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the last refresh: %w", err)
	}
	if !last.Valid || last.String == "" {
		return time.Time{}, nil
	}
	t, err := db.ParseTime(last.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("read the last refresh: %w", err)
	}
	return t, nil
}
