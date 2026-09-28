package enginerun

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/snapshots"
	"github.com/sl0wz3r/bunkarr/internal/syncer"
)

// The engines.Engine implementations of restic and rclone (§3.1): Create and Test delegate to the
// drivers, and Open checks the destination's identity through the driver (S25, read-only: restic
// cat config --no-lock, the rclone marker) and returns this package's session, whose PlanFS
// answers from the records (§3.3) and whose VersionStore is the driver's (§8).

// Config version roots of rclone destinations (plexdb.PlexRoot, arrbackup.ArrRoot and
// manifest.Root; the layouts are those of internal/snapshots).
var (
	plexLayout   = snapshots.Layout{Root: filecopy.MetaDir + "/plex", DefaultSlug: "plex"}
	arrLayout    = snapshots.Layout{Root: filecopy.MetaDir + "/arr", DefaultSlug: "arr"}
	manifestRoot = filecopy.MetaDir + "/manifests"
)

// configFolders are the kind folders of config versions and their version kinds.
var configFolders = []struct{ folder, kind string }{
	{plexLayout.Root, engines.VersionPlexDB},
	{arrLayout.Root, engines.VersionArr},
	{manifestRoot, engines.VersionManifest},
}

// isVersionDir is the validator of rclone's purge (§8.3): a Plex DB or *arr version directory
// (snapshots.Layout.SplitVersionPath) or a manifest version (".bunkarr/manifests/<version>").
func isVersionDir(rel string) bool {
	for _, l := range []snapshots.Layout{plexLayout, arrLayout} {
		if _, _, ok := l.SplitVersionPath(rel); ok {
			return true
		}
	}
	dir, name := path.Split(rel)
	return dir == manifestRoot+"/" && snapshots.IsVersionName(name)
}

// runtime fills a job runtime's runner and run directories from the Service.
func (s *Service) runtime(rt engines.Runtime) engines.Runtime {
	if rt.Runner == nil {
		rt.Runner = s.o.Runner
	}
	if rt.RunDirs == nil {
		rt.RunDirs = s.o.RunDirs
	}
	if rt.Location == nil {
		rt.Location = s.loc
	}
	if rt.Now == nil {
		rt.Now = s.now
	}
	if rt.HostName == "" {
		rt.HostName = s.o.HostName
	}
	if rt.ProcessStart.IsZero() {
		rt.ProcessStart = s.o.ProcessStart
	}
	return rt
}

// --- restic ---

type resticEngine struct{ s *Service }

var _ engines.Engine = (*resticEngine)(nil)

func (e *resticEngine) Kind() engines.Kind { return engines.Restic }

func (e *resticEngine) Create(ctx context.Context, d engines.Destination, sec engines.Secrets, attach bool) (engines.CreateResult, error) {
	return e.s.o.Restic.Create(ctx, d, sec, attach)
}

func (e *resticEngine) Test(ctx context.Context, d engines.Destination, sec engines.Secrets) (engines.TestResult, error) {
	return e.s.o.Restic.Test(ctx, d, sec)
}

// Open checks the repository's identity (S25) and returns a session.
func (e *resticEngine) Open(ctx context.Context, d engines.Destination, sec engines.Secrets, rt engines.Runtime) (engines.Session, error) {
	repo, err := e.s.o.Restic.Connect(d, sec, e.s.runtime(rt))
	if err != nil {
		return nil, err
	}
	if err := repo.CheckIdentity(ctx); err != nil {
		return nil, err
	}
	return &resticSession{s: e.s, repo: repo, dest: d, ctx: ctx}, nil
}

// resticSession is one job's access to a restic repository.
type resticSession struct {
	s    *Service
	repo *restic.Repo
	dest engines.Destination
	// ctx is the job's context, for the PlanFS reads.
	ctx context.Context
}

func (x *resticSession) Caps() filecopy.Capabilities { return restic.Capabilities() }

func (x *resticSession) PlanFS() engines.PlanFS {
	return recordPlanFS{ctx: x.ctx, files: x.s.files, dest: x.dest.ID}
}

func (x *resticSession) List(ctx context.Context, f engines.ListFilter) ([]engines.Version, error) {
	var out []engines.Version
	if f.Kind == "" || f.Kind == engines.VersionMedia {
		media, err := x.s.List(ctx, x.dest.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range media {
			if f.SourceID == 0 || v.SourceID == f.SourceID {
				out = append(out, v)
			}
		}
	}
	cfg, err := listConfigVersions(ctx, x.Versions(), f)
	if err != nil {
		return nil, err
	}
	return append(out, cfg...), nil
}

func (x *resticSession) Versions() engines.VersionStore {
	return restic.NewVersionStore(x.repo, x.s.stagingDir(), func(ctx context.Context, tx *sql.Tx, snapshotID, kind, reason string) error {
		return x.s.RequestForget(ctx, tx, x.dest.ID, snapshotID, kind, reason)
	})
}

func (x *resticSession) Close() error { return nil }

// --- rclone ---

type rcloneEngine struct{ s *Service }

var _ engines.Engine = (*rcloneEngine)(nil)

func (e *rcloneEngine) Kind() engines.Kind { return engines.Rclone }

func (e *rcloneEngine) Create(ctx context.Context, d engines.Destination, sec engines.Secrets, attach bool) (engines.CreateResult, error) {
	return e.s.o.Rclone.Create(ctx, d, sec, attach)
}

func (e *rcloneEngine) Test(ctx context.Context, d engines.Destination, sec engines.Secrets) (engines.TestResult, error) {
	return e.s.o.Rclone.Test(ctx, d, sec)
}

// The destinations store removes the marker a Create wrote when the create then fails (§4.5).
var _ interface {
	RemoveMarker(context.Context, engines.Destination, engines.Secrets, string) error
} = (*rcloneEngine)(nil)

// RemoveMarker removes the marker a Create wrote while it still carries id.
func (e *rcloneEngine) RemoveMarker(ctx context.Context, d engines.Destination, sec engines.Secrets, id string) error {
	return e.s.o.Rclone.RemoveMarker(ctx, d, sec, id)
}

// Open checks the destination's marker (S25) and returns a session.
func (e *rcloneEngine) Open(ctx context.Context, d engines.Destination, sec engines.Secrets, rt engines.Runtime) (engines.Session, error) {
	conn, err := e.s.o.Rclone.Connect(d, sec, e.s.runtime(rt))
	if err != nil {
		return nil, err
	}
	if _, err := conn.CheckMarker(ctx); err != nil {
		return nil, err
	}
	return &rcloneSession{s: e.s, conn: conn, dest: d, ctx: ctx}, nil
}

// rcloneSession is one job's access to an rclone destination.
type rcloneSession struct {
	s    *Service
	conn *rclone.Conn
	dest engines.Destination
	ctx  context.Context
}

func (x *rcloneSession) Caps() filecopy.Capabilities { return rclone.Capabilities(x.dest) }

func (x *rcloneSession) PlanFS() engines.PlanFS {
	return recordPlanFS{ctx: x.ctx, files: x.s.files, dest: x.dest.ID}
}

func (x *rcloneSession) List(ctx context.Context, f engines.ListFilter) ([]engines.Version, error) {
	var out []engines.Version
	if f.Kind == "" || f.Kind == engines.VersionRetention {
		runs, err := x.s.files.RetentionRuns(ctx, x.dest.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			out = append(out, engines.Version{Kind: engines.VersionRetention, Ref: engines.Ref(r.Path), Path: r.Path,
				Time: r.RetainedAt, Complete: true, Files: r.Files, Bytes: r.Bytes})
		}
	}
	cfg, err := listConfigVersions(ctx, x.Versions(), f)
	if err != nil {
		return nil, err
	}
	return append(out, cfg...), nil
}

func (x *rcloneSession) Versions() engines.VersionStore {
	return rclone.NewVersionStore(x.conn, isVersionDir)
}

func (x *rcloneSession) Close() error { return nil }

// listConfigVersions lists the config versions of the kinds f asks for through vs.
func listConfigVersions(ctx context.Context, vs engines.VersionStore, f engines.ListFilter) ([]engines.Version, error) {
	var out []engines.Version
	for _, c := range configFolders {
		if f.Kind != "" && f.Kind != c.kind {
			continue
		}
		list, err := vs.List(ctx, c.folder)
		if err != nil {
			return nil, fmt.Errorf("list the %s versions: %w", c.kind, err)
		}
		for _, v := range list {
			if f.IntegrationID != 0 && v.IntegrationID != f.IntegrationID {
				continue
			}
			var size int64
			for _, n := range v.Files {
				size += n
			}
			out = append(out, engines.Version{Kind: c.kind, Ref: v.Ref, Path: v.LogicalPath, IntegrationID: v.IntegrationID,
				JobID: v.JobID, Time: v.Time, Complete: v.Complete, Files: int64(len(v.Files)), Bytes: size})
		}
	}
	return out, nil
}

// recordPlanFS is the planner's view of an engine destination (§3.3): the head/tail hash of a
// path is its live record's head_tail (engines.ErrNoHeadTail without one, so the pairing turns a
// rename into copy + retain), and no destination path is ever an unmanaged file (restic holds none;
// rclone moves an object in the way into retention itself, S23).
type recordPlanFS struct {
	ctx   context.Context
	files *syncer.Store
	dest  int64
}

func (p recordPlanFS) DestHeadTail(destRel string) (string, error) {
	rec, ok, err := p.files.LiveAt(p.ctx, p.dest, destRel)
	if err != nil {
		return "", err
	}
	if !ok || rec.HeadTail == "" || rec.State == syncer.StateMissing {
		return "", engines.ErrNoHeadTail
	}
	return rec.HeadTail, nil
}

func (p recordPlanFS) DestStat(string) (filecopy.Stat, bool, error) {
	return filecopy.Stat{}, false, nil
}

// --- config runners and the API ---

// OpenVersions implements engines.VersionOpener for the Plex DB, *arr and manifest runners
// (§8.4): the destination must be an enabled engine destination whose create finished, whose
// recovery kit custody is confirmed (unless rt.DryRun, S21) and whose engine is available; its
// hosts pass netguard, and its identity is checked (S25). No transfer window applies; the
// bandwidth limits do (they are part of the destination the driver is handed). The closer ends
// the session.
func (s *Service) OpenVersions(ctx context.Context, destinationID int64, rt engines.Runtime) (engines.VersionStore, io.Closer, error) {
	d, err := s.o.Destinations.Get(ctx, destinationID)
	if err != nil {
		return nil, nil, err
	}
	if !d.IsEngine() {
		return nil, nil, destinations.ErrNotEngine
	}
	if err := s.gate(d, rt.DryRun); err != nil {
		return nil, nil, err
	}
	if err := s.checkHosts(ctx, d); err != nil {
		return nil, nil, err
	}
	ed, sec, err := s.o.Destinations.SecretsFor(ctx, destinationID)
	if err != nil {
		return nil, nil, err
	}
	eng, ok := s.Engines()(ed.Engine)
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", engines.ErrEngineUnavailable, ed.Engine)
	}
	sess, err := eng.Open(ctx, ed, sec, rt)
	if err != nil {
		return nil, nil, err
	}
	return sess.Versions(), sess, nil
}

// gate refuses a job of d (S21, S25, §11.1): disabled, a create that did not finish, recovery
// kit custody not confirmed (dry runs excepted), engine unavailable.
func (s *Service) gate(d destinations.Destination, dryRun bool) error {
	switch {
	case d.Pending:
		return engines.ErrPending
	case !d.Enabled:
		return fmt.Errorf("destination %q is disabled", d.Name)
	case !dryRun && destinations.Blocked(d) != "":
		return errors.New(destinations.Blocked(d))
	}
	return s.availability().Check(engines.Kind(d.Engine))
}

// checkHosts checks every host the engine dials (S25).
func (s *Service) checkHosts(ctx context.Context, d destinations.Destination) error {
	for _, h := range engines.DialHosts(d.Kind, d.Remote) {
		if err := s.o.CheckHost(ctx, h); err != nil {
			return fmt.Errorf("destination %q: host %s refused: %w", d.Name, h, err)
		}
	}
	return nil
}

// Unlock removes restic locks of a destination (POST /destinations/{id}/unlock): the identity
// check first (S25, --no-lock), then restic unlock --remove-all when removeAll (the user's explicit
// request), else the guarded unlock of stale locks (§6.7: never a lock that may belong to a live
// process with this container's host name). The API holds dest:<id> and refuses while a job of
// the destination is running or deferred.
func (s *Service) Unlock(ctx context.Context, destinationID int64, removeAll bool) error {
	d, err := s.o.Destinations.Get(ctx, destinationID)
	if err != nil {
		return err
	}
	if engines.Kind(d.Engine) != engines.Restic {
		return errors.New("only restic destinations have locks")
	}
	if d.Pending {
		return engines.ErrPending
	}
	if err := s.availability().Check(engines.Restic); err != nil {
		return err
	}
	if err := s.checkHosts(ctx, d); err != nil {
		return err
	}
	ed, sec, err := s.o.Destinations.SecretsFor(ctx, destinationID)
	if err != nil {
		return err
	}
	repo, err := s.o.Restic.Connect(ed, sec, s.runtime(engines.Runtime{RetryBudget: s.retryBudget(ctx)}))
	if err != nil {
		return err
	}
	if err := repo.CheckIdentity(ctx); err != nil {
		return err
	}
	if removeAll {
		return repo.Unlock(ctx, true)
	}
	return repo.GuardedUnlock(ctx, s.o.HostName, s.o.ProcessStart, nil)
}
