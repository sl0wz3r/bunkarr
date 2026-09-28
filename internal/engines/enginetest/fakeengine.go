package enginetest

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// EngineCall is one call of a FakeEngine.
type EngineCall struct {
	// Method is Open, Create or Test.
	Method  string
	Dest    engines.Destination
	Secrets engines.Secrets
	Attach  bool
	Runtime engines.Runtime
}

// FakeEngine is a programmable engines.Engine that records its calls. The On* functions, when
// set, answer instead of the fixed results.
type FakeEngine struct {
	EngineKind engines.Kind

	CreateResult engines.CreateResult
	CreateErr    error
	TestResult   engines.TestResult
	TestErr      error
	// Session is what Open returns (a new FakeSession when nil).
	Session engines.Session
	OpenErr error

	OnCreate func(ctx context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error)
	OnTest   func(ctx context.Context, d engines.Destination, s engines.Secrets) (engines.TestResult, error)
	OnOpen   func(ctx context.Context, d engines.Destination, s engines.Secrets, rt engines.Runtime) (engines.Session, error)

	mu    sync.Mutex
	calls []EngineCall
}

var _ engines.Engine = (*FakeEngine)(nil)

func (e *FakeEngine) record(c EngineCall) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, c)
}

// Calls returns the calls so far.
func (e *FakeEngine) Calls() []EngineCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

// Kind implements engines.Engine.
func (e *FakeEngine) Kind() engines.Kind { return e.EngineKind }

// Open implements engines.Engine.
func (e *FakeEngine) Open(ctx context.Context, d engines.Destination, s engines.Secrets, rt engines.Runtime) (engines.Session, error) {
	e.record(EngineCall{Method: "Open", Dest: d, Secrets: s, Runtime: rt})
	if e.OnOpen != nil {
		return e.OnOpen(ctx, d, s, rt)
	}
	if e.OpenErr != nil {
		return nil, e.OpenErr
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Session == nil {
		e.Session = NewFakeSession()
	}
	return e.Session, nil
}

// Create implements engines.Engine.
func (e *FakeEngine) Create(ctx context.Context, d engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
	e.record(EngineCall{Method: "Create", Dest: d, Secrets: s, Attach: attach})
	if e.OnCreate != nil {
		return e.OnCreate(ctx, d, s, attach)
	}
	return e.CreateResult, e.CreateErr
}

// Test implements engines.Engine.
func (e *FakeEngine) Test(ctx context.Context, d engines.Destination, s engines.Secrets) (engines.TestResult, error) {
	e.record(EngineCall{Method: "Test", Dest: d, Secrets: s})
	if e.OnTest != nil {
		return e.OnTest(ctx, d, s)
	}
	return e.TestResult, e.TestErr
}

// FakePlanFS is an in-memory engines.PlanFS: head/tail hashes and destination stats by path.
type FakePlanFS struct {
	mu        sync.Mutex
	HeadTails map[string]string
	Stats     map[string]filecopy.Stat
}

// DestHeadTail implements engines.PlanFS.
func (p *FakePlanFS) DestHeadTail(destRel string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h, ok := p.HeadTails[destRel]; ok {
		return h, nil
	}
	return "", engines.ErrNoHeadTail
}

// DestStat implements engines.PlanFS.
func (p *FakePlanFS) DestStat(destRel string) (filecopy.Stat, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.Stats[destRel]
	return st, ok, nil
}

// FakeSession is a programmable engines.Session.
type FakeSession struct {
	Capabilities filecopy.Capabilities
	FS           *FakePlanFS
	Store        *FakeVersionStore
	// Listed is what List filters and returns; ListErr fails it.
	Listed  []engines.Version
	ListErr error

	mu     sync.Mutex
	closed int
}

var _ engines.Session = (*FakeSession)(nil)

// NewFakeSession returns a session with case-sensitive nanosecond capabilities, an empty PlanFS
// and an empty version store.
func NewFakeSession() *FakeSession {
	return &FakeSession{
		Capabilities: filecopy.Capabilities{MtimeGranularityNs: 1, EnforcesModes: true},
		FS:           &FakePlanFS{HeadTails: map[string]string{}, Stats: map[string]filecopy.Stat{}},
		Store:        NewFakeVersionStore(),
	}
}

// Caps implements engines.Session.
func (s *FakeSession) Caps() filecopy.Capabilities { return s.Capabilities }

// PlanFS implements engines.Session.
func (s *FakeSession) PlanFS() engines.PlanFS { return s.FS }

// List implements engines.Session: Listed filtered by f.
func (s *FakeSession) List(_ context.Context, f engines.ListFilter) ([]engines.Version, error) {
	if s.ListErr != nil {
		return nil, s.ListErr
	}
	var out []engines.Version
	for _, v := range s.Listed {
		if (f.Kind == "" || v.Kind == f.Kind) && (f.SourceID == 0 || v.SourceID == f.SourceID) &&
			(f.IntegrationID == 0 || v.IntegrationID == f.IntegrationID) {
			out = append(out, v)
		}
	}
	return out, nil
}

// Versions implements engines.Session.
func (s *FakeSession) Versions() engines.VersionStore { return s.Store }

// Close implements engines.Session.
func (s *FakeSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed++
	return nil
}

// Closed counts the Close calls.
func (s *FakeSession) Closed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// RemoveCall is one FakeVersionStore.Remove.
type RemoveCall struct {
	Ref  engines.Ref
	Kind string
	// Tx is the caller's transaction (restic's forget request is written in it, D28).
	Tx *sql.Tx
}

// FakeVersionStore is an in-memory engines.VersionStore: versions with their files, leftovers
// injected with AddLeftover, per-method failures (FailNext, FailAlways), and every Remove with its
// transaction.
type FakeVersionStore struct {
	// RefPrefix starts every reference Put returns (default "fake-").
	RefPrefix string

	mu       sync.Mutex
	versions []*fakeVersion
	n        int
	next     map[string][]error
	always   map[string]error
	removed  []RemoveCall
}

type fakeVersion struct {
	stored engines.StoredVersion
	files  map[string][]byte
}

var _ engines.VersionStore = (*FakeVersionStore)(nil)

// NewFakeVersionStore returns an empty store.
func NewFakeVersionStore() *FakeVersionStore {
	return &FakeVersionStore{RefPrefix: "fake-", next: map[string][]error{}, always: map[string]error{}}
}

// FailNext makes the next call of method (Put, List, ReadFile, Fetch, Remove) fail with err.
func (s *FakeVersionStore) FailNext(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.next[method] = append(s.next[method], err)
}

// FailAlways makes every call of method fail with err (nil: stop failing).
func (s *FakeVersionStore) FailAlways(method string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.always, method)
	} else {
		s.always[method] = err
	}
}

// failure returns the injected error of method; the caller holds mu.
func (s *FakeVersionStore) failure(method string) error {
	if errs := s.next[method]; len(errs) > 0 {
		s.next[method] = errs[1:]
		return errs[0]
	}
	return s.always[method]
}

// AddLeftover stores a version directly (complete or not): an unrecorded version of a crashed
// run, or a leftover without manifest.json. An empty Ref gets a new one, which is returned.
func (s *FakeVersionStore) AddLeftover(v engines.StoredVersion, files map[string][]byte) engines.Ref {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.Ref == "" {
		s.n++
		v.Ref = engines.Ref(fmt.Sprintf("%s%d", s.RefPrefix, s.n))
	}
	if v.Version == "" {
		v.Version = path.Base(v.LogicalPath)
	}
	if v.Files == nil {
		v.Files = map[string]int64{}
		for name, b := range files {
			v.Files[name] = int64(len(b))
		}
	}
	s.versions = append(s.versions, &fakeVersion{stored: v, files: files})
	return v.Ref
}

// Stored returns every version held, in insertion order.
func (s *FakeVersionStore) Stored() []engines.StoredVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]engines.StoredVersion, len(s.versions))
	for i, v := range s.versions {
		out[i] = v.stored
	}
	return out
}

// Removed returns the Remove calls so far.
func (s *FakeVersionStore) Removed() []RemoveCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.removed)
}

func (s *FakeVersionStore) find(ref engines.Ref) *fakeVersion {
	for _, v := range s.versions {
		if v.stored.Ref == ref {
			return v
		}
	}
	return nil
}

// Put implements engines.VersionStore: it reads every file under v.Dir (manifest.json required)
// and returns a new reference.
func (s *FakeVersionStore) Put(_ context.Context, v engines.PutVersion) (engines.Ref, error) {
	s.mu.Lock()
	if err := s.failure("Put"); err != nil {
		s.mu.Unlock()
		return "", err
	}
	s.mu.Unlock()
	files := map[string][]byte{}
	err := filepath.WalkDir(v.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(v.Dir, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		files[filepath.ToSlash(rel)] = b
		return err
	})
	if err != nil {
		return "", fmt.Errorf("fake put: %w", err)
	}
	if _, ok := files["manifest.json"]; !ok {
		return "", errors.New("fake put: the version has no manifest.json")
	}
	return s.AddLeftover(engines.StoredVersion{LogicalPath: v.LogicalPath, Time: v.Time, Complete: true,
		JobID: v.JobID, IntegrationID: v.IntegrationID}, files), nil
}

// List implements engines.VersionStore: the versions under kindFolder, by logical path.
func (s *FakeVersionStore) List(_ context.Context, kindFolder string) ([]engines.StoredVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure("List"); err != nil {
		return nil, err
	}
	prefix := strings.TrimSuffix(kindFolder, "/") + "/"
	var out []engines.StoredVersion
	for _, v := range s.versions {
		if strings.HasPrefix(v.stored.LogicalPath, prefix) {
			out = append(out, v.stored)
		}
	}
	slices.SortStableFunc(out, func(a, b engines.StoredVersion) int { return strings.Compare(a.LogicalPath, b.LogicalPath) })
	return out, nil
}

// ReadFile implements engines.VersionStore.
func (s *FakeVersionStore) ReadFile(_ context.Context, ref engines.Ref, name string, limit int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure("ReadFile"); err != nil {
		return nil, err
	}
	v := s.find(ref)
	if v == nil {
		return nil, fmt.Errorf("fake read %s: version %s: %w", name, ref, fs.ErrNotExist)
	}
	b, ok := v.files[name]
	if !ok {
		return nil, fmt.Errorf("fake read %s of %s: %w", name, ref, fs.ErrNotExist)
	}
	if int64(len(b)) > limit {
		b = b[:limit]
	}
	return slices.Clone(b), nil
}

// Fetch implements engines.VersionStore: the file is written to dstDir/<base name> (0600).
func (s *FakeVersionStore) Fetch(ctx context.Context, ref engines.Ref, name, dstDir string) error {
	s.mu.Lock()
	err := s.failure("Fetch")
	s.mu.Unlock()
	if err != nil {
		return err
	}
	b, err := s.ReadFile(ctx, ref, name, 1<<62)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dstDir, path.Base(name)), b, 0o600)
}

// Remove implements engines.VersionStore: it records the call (with tx) and drops the version.
func (s *FakeVersionStore) Remove(_ context.Context, tx *sql.Tx, ref engines.Ref, kind string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.failure("Remove"); err != nil {
		return err
	}
	s.removed = append(s.removed, RemoveCall{Ref: ref, Kind: kind, Tx: tx})
	s.versions = slices.DeleteFunc(s.versions, func(v *fakeVersion) bool { return v.stored.Ref == ref })
	return nil
}
