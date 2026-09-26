package manifest

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// TestBuildWithTheTierEngine: the real tier engine plugged in through TierEngine, evaluated in the
// builder's read transaction, with the spec preset over the recorded Radarr and Sonarr: the tagged
// movie and its sidecar and extra are full, the other items are manifest, and the file outside the
// mapped root folders is full (Radarr's unmapped /movies-4k root makes it unknown); with no rules
// everything is full.
func TestBuildWithTheTierEngine(t *testing.T) {
	e := newEnv(t)
	eng := tiers.New(tiers.Options{DB: e.db, Catalog: e.cat, Index: e.index.Store(), Integrations: e.ints, Now: e.clock.Now})
	r, err := NewRunner(Options{DB: e.db, Catalog: e.cat, Integrations: e.ints, Destinations: e.dests, Index: e.index.Store(),
		Tiers: TierEngine{Engine: eng}, ConfigDir: e.config, Now: e.clock.Now, Location: time.UTC, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	e.runner = r
	d := e.dest
	m := e.build(&d)
	if m.Summary.Tiers.Manifest.Files != 0 || m.Summary.Tiers.Skip.Files != 0 || m.Summary.Tiers.Full.Files != m.Summary.Files-1 {
		t.Fatalf("no rules: summary %+v", m.Summary)
	}

	if _, _, err := eng.SaveRules(e.ctx, 0, tiers.Presets()[1].Rules); err != nil {
		t.Fatal(err)
	}
	m = e.build(&d)
	tierOf := func(title string) Tier {
		t.Helper()
		it := itemByTitle(t, m, title)
		if len(it.Files) == 0 || it.Files[0].Tier == nil {
			t.Fatalf("%s has no file with a tier: %+v", title, it)
		}
		return *it.Files[0].Tier
	}
	if got := tierOf("Night of the Living Dead"); got != TierFull {
		t.Errorf("tagged movie: %s", got)
	}
	notld := itemByTitle(t, m, "Night of the Living Dead")
	if f := notld.Files[0]; f.Rule == nil || f.Rule.Name != "Tagged bunkarr-full" || f.Rule.ID == 0 {
		t.Errorf("rule %+v", f.Rule)
	}
	for _, x := range notld.ExtraFiles {
		if x.Tier == nil || *x.Tier != TierFull {
			t.Errorf("extra %s: %v", x.RelPath, x.Tier)
		}
	}
	for _, title := range []string{"His Girl Friday", "Charade"} {
		if got := tierOf(title); got != TierManifest {
			t.Errorf("%s: %s", title, got)
		}
	}
	// The recorded Radarr's /movies-4k root folder has no path mapping, so Bunkarr cannot tell
	// whether the file outside the mapped root folders is in it: unknown, so full (S14), not
	// unmanaged.
	if len(m.OtherFiles) != 1 || m.OtherFiles[0].Tier == nil || *m.OtherFiles[0].Tier != TierFull {
		t.Errorf("a file outside every mapped root folder: %+v", m.OtherFiles)
	}
	// A stale Radarr makes its facts unknown: the tag rule is more protective, so full.
	e.clock.Advance(25 * time.Hour)
	m = e.build(&d)
	if got := tierOf("Charade"); got != TierFull {
		t.Errorf("stale Radarr, Charade: %s", got)
	}
}

// barrierTiers holds the first n Decide calls until n builds are in it at once, each holding its
// read transaction's connection, then passes them all on to next; later calls pass at once.
type barrierTiers struct {
	next    Tiers
	n       int
	mu      sync.Mutex
	arrived int
	full    chan struct{}
}

func (b *barrierTiers) Decide(ctx context.Context, r TierRead, destinationID, sourceID int64) (func(int64) Decision, error) {
	b.mu.Lock()
	b.arrived++
	if b.arrived == b.n {
		close(b.full)
	}
	b.mu.Unlock()
	select {
	case <-b.full:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return b.next.Decide(ctx, r, destinationID, sourceID)
}

// tierEngineWithRules is the real tier engine with the Plex/Tautulli/Seerr/Maintainerr provider,
// the Maintainerr preset (a provider field) and the spec preset (arr.tag) saved.
func tierEngineWithRules(t *testing.T, e *testEnv) *tiers.Engine {
	t.Helper()
	prov := tiers.NewIndexProvider(e.index.Store(), e.ints, nil)
	eng := tiers.New(tiers.Options{DB: e.db, Catalog: e.cat, Index: e.index.Store(), Integrations: e.ints,
		Providers: []tiers.Provider{prov}, Now: e.clock.Now})
	rules := append([]tiers.RuleInput{}, tiers.Presets()[2].Rules...)
	rules = append(rules, tiers.Presets()[1].Rules...)
	if _, _, err := eng.SaveRules(e.ctx, 0, rules); err != nil {
		t.Fatal(err)
	}
	return eng
}

// TestConcurrentBuildsDoNotExhaustTheReadPool: more builds at once than the read pool has
// connections (jobs.workers >= 4 exporting manifests), with the real tier engine. Each build's read
// transaction holds a connection; a tier decision that read through the pool too would wait for a
// second one while every connection is held by a build waiting the same way, and every read of
// the app would hang. The builds finish, with the manifest a lone build makes.
func TestConcurrentBuildsDoNotExhaustTheReadPool(t *testing.T) {
	e := newEnv(t)
	pool := e.db.Reader().Stats().MaxOpenConnections
	if pool != 4 {
		t.Fatalf("the read pool has %d connections, want 4", pool)
	}
	eng := tierEngineWithRules(t, e)
	lone, err := NewBuilder(BuilderOptions{DB: e.db, Catalog: e.cat, Integrations: e.ints, Index: e.index.Store(),
		Tiers: TierEngine{Engine: eng}, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	d := e.dest
	want, err := lone.Build(e.ctx, BuildScope{Destination: &d})
	if err != nil {
		t.Fatal(err)
	}
	if want.Summary.Tiers.Manifest.Files == 0 || want.Summary.Tiers.Full.Files == 0 {
		t.Fatalf("the rules decide nothing: %+v", want.Summary.Tiers)
	}
	wantHash, _ := ContentHash(want)

	const builds = 6 // > pool
	gate := &barrierTiers{next: TierEngine{Engine: eng}, n: pool, full: make(chan struct{})}
	b, err := NewBuilder(BuilderOptions{DB: e.db, Catalog: e.cat, Integrations: e.ints, Index: e.index.Store(), Tiers: gate, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	type result struct {
		m   *Manifest
		err error
	}
	results := make(chan result, builds)
	for range builds {
		go func() {
			m, err := b.Build(ctx, BuildScope{Destination: &d})
			results <- result{m, err}
		}()
	}
	for i := range builds {
		select {
		case r := <-results:
			if errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("build %d: deadlocked on the read pool: %v", i, r.err)
			}
			if r.err != nil {
				t.Fatalf("build %d: %v", i, r.err)
			}
			if h, _ := ContentHash(r.m); h != wantHash {
				t.Errorf("build %d: the manifest differs from a lone build's (%+v, want %+v)", i, r.m.Summary.Tiers, want.Summary.Tiers)
			}
		case <-time.After(45 * time.Second):
			t.Fatalf("only %d of %d builds finished: deadlocked on the read pool", i, builds)
		}
	}
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.arrived != builds {
		t.Fatalf("%d decisions, want %d", gate.arrived, builds)
	}
}

// changeMidBuild runs change once, after the build's read transaction read the catalog and
// before the first decision: as the settings pages and a refresh would, concurrently.
type changeMidBuild struct {
	next   Tiers
	change func(ctx context.Context) error
	once   sync.Once
}

func (c *changeMidBuild) Decide(ctx context.Context, r TierRead, destinationID, sourceID int64) (func(int64) Decision, error) {
	var err error
	c.once.Do(func() { err = c.change(ctx) })
	if err != nil {
		return nil, err
	}
	return c.next.Decide(ctx, r, destinationID, sourceID)
}

// TestTierDecisionsReadTheBuildSnapshot: the tier decisions describe the state the rest of the
// manifest does (S20): rules saved, an *arr disabled and a tag removed while the manifest is built
// are not seen by its decisions, which match its own integrations list; the next build sees them.
func TestTierDecisionsReadTheBuildSnapshot(t *testing.T) {
	e := newEnv(t)
	eng := tierEngineWithRules(t, e)
	var skipRev int64
	mid := &changeMidBuild{next: TierEngine{Engine: eng}, change: func(ctx context.Context) error {
		rev, err := eng.Revision(ctx)
		if err != nil {
			return err
		}
		rs, _, err := eng.SaveRules(ctx, rev, []tiers.RuleInput{{Name: "Skip everything", Conditions: []tiers.Condition{}, Action: tiers.Skip}})
		if err != nil {
			return err
		}
		skipRev = rs.Revision
		off := false
		if _, err := e.ints.Update(ctx, e.rad.ID, integrations.Input{Name: e.rad.Name, URL: e.rad.URL, Enabled: &off}); err != nil {
			return err
		}
		return e.db.Write(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `UPDATE arr_items SET tags = '[]' WHERE integration_id = ?`, e.rad.ID)
			return err
		})
	}}
	b, err := NewBuilder(BuilderOptions{DB: e.db, Catalog: e.cat, Integrations: e.ints, Index: e.index.Store(), Tiers: mid, Now: e.clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	d := e.dest
	m, err := b.Build(e.ctx, BuildScope{Destination: &d})
	if err != nil {
		t.Fatal(err)
	}
	if skipRev == 0 {
		t.Fatal("nothing changed during the build")
	}
	radarrListed := false
	for _, it := range m.Integrations {
		radarrListed = radarrListed || it.ID == e.rad.ID
	}
	if !radarrListed {
		t.Fatal("the manifest does not list Radarr, enabled when the build began")
	}
	file := func(title string) File {
		t.Helper()
		it := itemByTitle(t, m, title)
		if len(it.Files) == 0 || it.Files[0].Tier == nil || it.Files[0].Rule == nil {
			t.Fatalf("%s has no file with a tier: %+v", title, it)
		}
		return it.Files[0]
	}
	// Radarr enabled and fresh, as the manifest lists it: Charade is manifest by the old rules (a
	// disabled Radarr would make its facts unknown, so full), and the tagged movie is still full.
	if f := file("Charade"); *f.Tier != TierManifest || f.Rule.Name != "Everything else" {
		t.Errorf("Charade: %s by %+v", *f.Tier, f.Rule)
	}
	if f := file("Night of the Living Dead"); *f.Tier != TierFull || f.Rule.Name != "Tagged bunkarr-full" {
		t.Errorf("the tagged movie: %s by %+v", *f.Tier, f.Rule)
	}
	if m.Summary.Tiers.Skip.Files != 0 {
		t.Errorf("the rules saved during the build were applied: %+v", m.Summary.Tiers)
	}
	// The next build sees every change.
	m, err = b.Build(e.ctx, BuildScope{Destination: &d})
	if err != nil {
		t.Fatal(err)
	}
	if m.Summary.Tiers.Skip.Files != m.Summary.Files {
		t.Errorf("after the changes: %+v", m.Summary.Tiers)
	}
}
