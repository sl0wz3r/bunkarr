package mediaindex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/httpread"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/maintainerr/maintainerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex/plextest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr"
	"github.com/sl0wz3r/bunkarr/internal/integrations/seerr/seerrtest"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli"
	"github.com/sl0wz3r/bunkarr/internal/integrations/tautulli/tautullitest"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

const plexToken = "plex-token-provider-tests"

// provEnv is a database with a Plex integration (library index on) pointing at a fake Plex that
// serves the slice 9 recording's library, sources holding that library's files (Plex's /data is
// <root>), and a refresh runner. Tautulli, Seerr and Maintainerr are added on demand.
type provEnv struct {
	t       *testing.T
	db      *db.DB
	ints    *integrations.Store
	cat     *catalog.Store
	scanner *catalog.Scanner
	runner  *Runner
	clock   *testClock
	root    string
	sources map[string]catalog.Source
	plexSrv *plextest.Server
	lib     *plextest.Library
	plexIt  integrations.Integration
	taut    *tautullitest.Server
	seerr   *seerrtest.Server
	maint   *maintainerrtest.Server
	jobID   int64
}

func newProvEnv(t *testing.T) *provEnv {
	t.Helper()
	e := &provEnv{t: t, db: openDB(t), clock: &testClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}, sources: map[string]catalog.Source{}}
	kr, err := config.NewKeyring(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	e.ints = integrations.NewStore(e.db, kr)
	e.cat = catalog.NewStore(e.db, catalog.StoreOptions{})
	e.scanner = catalog.NewScanner(e.cat, catalog.ScannerOptions{})
	e.root = resolvedTemp(t)
	e.plexSrv = plextest.NewServer(t, plexToken)
	e.lib = e.plexSrv.ServeLibrary(t)
	e.writeLibraryFiles()
	for _, name := range []string{"movies", "tv", "music"} {
		src, err := e.cat.Create(context.Background(), catalog.SourceInput{Name: name, Path: filepath.Join(e.root, name)})
		if err != nil {
			t.Fatal(err)
		}
		e.sources[name] = src
		if _, err := e.scanner.Scan(context.Background(), src.ID, nil); err != nil {
			t.Fatal(err)
		}
	}
	settings, _ := json.Marshal(map[string]any{"pathMappings": []map[string]string{{"plex": "/data", "local": e.root}}, "index": map[string]any{"enabled": true}})
	e.plexIt, err = e.ints.Create(context.Background(), integrations.Input{Type: integrations.TypePlex, Name: "Plex", URL: e.plexSrv.URL,
		APIKey: plexToken, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	e.runner = e.newRunner()
	return e
}

func (e *provEnv) newRunner() *Runner {
	e.t.Helper()
	r, err := NewRunner(RefreshOptions{DB: e.db, Integrations: e.ints, Catalog: e.cat, Now: e.clock.Now,
		Plex: plex.Options{HTTPClient: e.plexSrv.Client()}})
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

// writeLibraryFiles creates, under root, a sparse file of the listed size for every file of the
// recorded Plex library (/data/movies/... → <root>/movies/...).
func (e *provEnv) writeLibraryFiles() {
	e.t.Helper()
	for _, st := range [][2]any{{"1", plex.TypeMovie}, {"2", plex.TypeEpisode}, {"3", plex.TypeTrack}} {
		for _, raw := range e.lib.Rows(st[0].(string), st[1].(int)) {
			var row struct {
				Media []struct {
					Part []struct {
						File string `json:"file"`
						Size int64  `json:"size"`
					} `json:"Part"`
				} `json:"Media"`
			}
			if err := json.Unmarshal(raw, &row); err != nil {
				e.t.Fatal(err)
			}
			for _, m := range row.Media {
				for _, p := range m.Part {
					e.writeFile(p.File, p.Size)
				}
			}
		}
	}
}

// writeFile creates a sparse file at a Plex path (under root).
func (e *provEnv) writeFile(plexPath string, size int64) {
	e.t.Helper()
	p := filepath.Join(e.root, filepath.FromSlash(strings.TrimPrefix(plexPath, "/data/")))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Truncate(p, size); err != nil {
		e.t.Fatal(err)
	}
}

// addTautulli adds a Tautulli integration linked to the Plex integration.
func (e *provEnv) addTautulli() integrations.Integration {
	e.t.Helper()
	e.taut = tautullitest.NewServer(e.t)
	e.runner.o.Tautulli = tautulli.Options{Options: httpread.Options{HTTPClient: e.taut.Client()}}
	settings, _ := json.Marshal(map[string]any{"plexIntegrationId": e.plexIt.ID})
	it, err := e.ints.Create(context.Background(), integrations.Input{Type: integrations.TypeTautulli, Name: "Tautulli", URL: e.taut.URL,
		APIKey: tautullitest.Key, Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

// addSeerr adds a Seerr integration (linked to the Plex integration when linked).
func (e *provEnv) addSeerr(linked bool) integrations.Integration {
	e.t.Helper()
	e.seerr = seerrtest.NewServer(e.t)
	e.runner.o.Seerr = seerr.Options{Options: httpread.Options{HTTPClient: e.seerr.Client()}}
	s := map[string]any{}
	if linked {
		s["plexIntegrationId"] = e.plexIt.ID
	}
	settings, _ := json.Marshal(s)
	it, err := e.ints.Create(context.Background(), integrations.Input{Type: integrations.TypeSeerr, Name: "Seerr", URL: e.seerr.URL,
		APIKey: seerrtest.Key, Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

// addMaintainerr adds a Maintainerr integration of a recorded variant, linked to the Plex
// integration.
func (e *provEnv) addMaintainerr(variant string) integrations.Integration {
	e.t.Helper()
	e.maint = maintainerrtest.NewServer(e.t, variant)
	e.runner.o.Maintainerr = maintainerr.Options{Options: httpread.Options{HTTPClient: e.maint.Client()}}
	settings, _ := json.Marshal(map[string]any{"plexIntegrationId": e.plexIt.ID})
	it, err := e.ints.Create(context.Background(), integrations.Input{Type: integrations.TypeMaintainerr, Name: "Maintainerr", URL: e.maint.URL,
		Settings: settings})
	if err != nil {
		e.t.Fatal(err)
	}
	return it
}

// run runs a refresh of it.
func (e *provEnv) run(it integrations.Integration, p jobs.Params, dry bool) (jobs.Result, *memReporter, error) {
	e.t.Helper()
	e.jobID++
	p.IntegrationID = it.ID
	rep := &memReporter{}
	res, err := e.runner.Run(context.Background(), jobs.Job{ID: e.jobID, Type: jobs.TypeRefresh, Trigger: jobs.TriggerManual, DryRun: dry, Params: p, Attempt: 1},
		jobs.Env{Reporter: rep, Items: &memItems{}})
	return res, rep, err
}

// mustRun runs a refresh that must succeed and returns its stats.
func (e *provEnv) mustRun(it integrations.Integration) (ProviderStats, *memReporter) {
	e.t.Helper()
	res, rep, err := e.run(it, jobs.Params{}, false)
	if err != nil {
		e.t.Fatalf("refresh of %s: %v (log: %s)", it.Name, err, rep)
	}
	return res.Stats.(ProviderStats), rep
}

func (e *provEnv) state(id int64) State {
	e.t.Helper()
	st, err := e.runner.Store().State(context.Background(), nil, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *provEnv) fresh(it integrations.Integration) Freshness {
	e.t.Helper()
	cur, err := e.ints.Get(context.Background(), it.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	f, err := e.runner.Store().Freshness(context.Background(), nil, cur)
	if err != nil {
		e.t.Fatal(err)
	}
	return f
}

func (e *provEnv) count(table string, id int64) int64 {
	e.t.Helper()
	var n int64
	if err := e.db.Reader().QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE integration_id = ?`, id).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}
