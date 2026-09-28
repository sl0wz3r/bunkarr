//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The upgrade tests (upgrade_test.go, upgrade_phase3_test.go) run a previous release's binary on a
// fresh database and this binary on a copy. The release is built from its commit. Each release has
// two commits with the same tree: the private one and its public counterpart, which
// scripts/public/sync.sh publishes to publicRepository as a new squashed commit. A CI checkout is
// shallow (actions/checkout fetches one commit), so in CI the commit is fetched by hash. An upgrade
// test that cannot get its release skips on a developer machine but fails in CI, because a test
// that skips on every push checks nothing.

// publicRepository is the public repository scripts/public/sync.sh publishes to.
const publicRepository = "https://github.com/sl0wz3r/bunkarr.git"

// releaseSources are where CI fetches a release commit from: the checkout's own origin (any
// commit of that repository), then the public repository (the public counterparts, anonymously).
var releaseSources = []string{"origin", publicRepository}

// releaseFetchTimeout bounds one fetch of one commit from one source.
const releaseFetchTimeout = 2 * time.Minute

// releaseFetchWaitDelay is how long a fetch that ran out of time may keep its output open once git
// is killed. git's transport helper (git-remote-http) outlives git and holds the stderr pipe, so
// without it the timeout kills git but the fetch still waits for the helper, which a source that
// accepts the connection and never answers keeps alive.
const releaseFetchWaitDelay = time.Second

// releaseBuild is a previous release's binary, built once on first use; TestMain removes dir.
type releaseBuild struct {
	name    string   // "Phase 2", for messages and file names
	env     string   // names a prebuilt binary of the release to use instead
	commits []string // the release: its private commit, then its public counterpart
	sources []string // where a missing commit is fetched from in CI
	root    string   // the repository the release is built from; empty is this module's (tests set it)

	once sync.Once
	dir  string
	path string
	skip string
	err  error
}

// binary returns the release binary: $env when set, else one of commits exported with `git
// archive` (the worktree is not touched) and built with -tags e2e.
// It skips when the release cannot be had, except in CI, where it fails.
func (r *releaseBuild) binary(t *testing.T) string {
	t.Helper()
	path, skip, err := r.resolve()
	if skip != "" {
		t.Skip(skip)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// resolve does the work of binary once and returns its outcome: the binary, a reason to skip, or
// an error.
func (r *releaseBuild) resolve() (path, skip string, err error) {
	r.once.Do(func() {
		if p := os.Getenv(r.env); p != "" {
			r.path, r.err = filepath.Abs(p)
			return
		}
		root := r.root
		if root == "" {
			var err error
			if root, err = moduleRoot(); err != nil {
				r.err = err
				return
			}
		}
		r.build(root, ciRun())
	})
	return r.path, r.skip, r.err
}

// ciRun reports whether the tests run in CI ($CI, which GitHub Actions and Gitea Actions set).
func ciRun() bool {
	v, err := strconv.ParseBool(os.Getenv("CI"))
	return err == nil && v
}

// unavailable records that the release cannot be had: an error when required (CI), else a skip.
func (r *releaseBuild) unavailable(why string, required bool) {
	hint := fmt.Sprintf("set %s to a %s binary", r.env, r.name)
	if !required {
		r.skip = fmt.Sprintf("%s (%s)", why, hint)
		return
	}
	r.err = fmt.Errorf("%s: an upgrade test must not skip in CI (CI=true); check out the full history (actions/checkout fetch-depth: 0) or %s", why, hint)
}

// build builds the release from the repository at root into r.path. required (CI) fetches a
// missing commit and turns every reason to skip into an error.
func (r *releaseBuild) build(root string, required bool) {
	if _, err := exec.LookPath("git"); err != nil {
		r.unavailable("git is not installed", required)
		return
	}
	commit, err := releaseCommit(root, r.commits, r.sources, required)
	if err != nil {
		r.unavailable(fmt.Sprintf("the %s release is not in this checkout: %v", r.name, err), required)
		return
	}
	slug := strings.ToLower(strings.ReplaceAll(r.name, " ", ""))
	dir, err := os.MkdirTemp("", "bunkarr-e2e-"+slug+"-")
	if err != nil {
		r.err = err
		return
	}
	r.dir = dir
	src := filepath.Join(dir, "src")
	if err := os.Mkdir(src, 0o755); err != nil {
		r.err = err
		return
	}
	tarball := filepath.Join(dir, slug+".tar")
	if out, err := exec.Command("git", "-C", root, "archive", "--format=tar", "-o", tarball, commit).CombinedOutput(); err != nil {
		r.err = fmt.Errorf("git archive %.12s: %w\n%s", commit, err, out)
		return
	}
	if out, err := exec.Command("tar", "-x", "-f", tarball, "-C", src).CombinedOutput(); err != nil {
		r.err = fmt.Errorf("tar: %w\n%s", err, out)
		return
	}
	out := filepath.Join(dir, "bunkarr-"+slug)
	build := exec.Command("go", "build", "-trimpath", "-tags", "e2e", "-o", out, "./cmd/bunkarr")
	build.Dir = src
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := build.CombinedOutput(); err != nil {
		r.err = fmt.Errorf("build the %s binary from %.12s: %w\n%s", r.name, commit, err, b)
		return
	}
	r.path = out
}

// releaseCommit returns the first of commits present in the repository at root. When none is and
// fetch is set, it fetches each from each of sources in turn, by hash (so exactly that commit
// arrives), and returns the first that arrived. Fetching adds objects to the repository, so it is
// only done in CI, whose checkout is disposable; a shallow checkout stays shallow (--depth=1).
func releaseCommit(root string, commits, sources []string, fetch bool) (string, error) {
	have := func(c string) bool {
		return exec.Command("git", "-C", root, "cat-file", "-e", c+"^{commit}").Run() == nil
	}
	for _, c := range commits {
		if have(c) {
			return c, nil
		}
	}
	short := make([]string, len(commits))
	for i, c := range commits {
		short[i] = fmt.Sprintf("%.12s", c)
	}
	missing := fmt.Errorf("none of the commits %s is present", strings.Join(short, ", "))
	if !fetch {
		return "", missing
	}
	shallow, err := exec.Command("git", "-C", root, "rev-parse", "--is-shallow-repository").Output()
	if err != nil {
		return "", fmt.Errorf("%w; git rev-parse --is-shallow-repository: %v", missing, err)
	}
	errs := []error{missing}
	for _, src := range sources {
		for _, c := range commits {
			if err := fetchCommit(root, src, c, strings.TrimSpace(string(shallow)) == "true", releaseFetchTimeout); err != nil {
				errs = append(errs, fmt.Errorf("fetch %.12s from %s: %w", c, src, err))
				continue
			}
			if have(c) {
				return c, nil
			}
			errs = append(errs, fmt.Errorf("fetch %.12s from %s: the commit did not arrive", c, src))
		}
	}
	return "", errors.Join(errs...)
}

// fetchCommit fetches one commit by hash from src (a remote name or URL) into the repository at
// root, without tags or ref updates and without ever prompting for credentials, and gives up after
// timeout.
func fetchCommit(root, src, commit string, shallow bool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{"-C", root, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules"}
	if shallow {
		args = append(args, "--depth=1")
	}
	args = append(args, src, commit)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if os.Getenv("GIT_SSH_COMMAND") == "" {
		cmd.Env = append(cmd.Env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes")
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = releaseFetchWaitDelay
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// gitRepo runs git in dir for the tests below, never with the user's hooks or signing.
func gitRepo(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false",
		"-c", "user.name=e2e", "-c", "user.email=e2e@example.invalid"}, args...)
	out, err := exec.Command("git", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// releaseFixture is a repository with a release commit under a later head, and a shallow clone of
// it that has only the head: the shape of a CI checkout.
func releaseFixture(t *testing.T) (clone, release string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	remote := t.TempDir()
	gitRepo(t, remote, "init", "-q", "-b", "main")
	writeFile(t, remote, "release.txt", []byte("release"))
	gitRepo(t, remote, "add", "-A")
	gitRepo(t, remote, "commit", "-q", "-m", "release")
	release = gitRepo(t, remote, "rev-parse", "HEAD")
	writeFile(t, remote, "later.txt", []byte("later"))
	gitRepo(t, remote, "add", "-A")
	gitRepo(t, remote, "commit", "-q", "-m", "later")
	clone = filepath.Join(t.TempDir(), "clone")
	gitRepo(t, remote, "clone", "-q", "--depth=1", "file://"+remote, clone)
	if exec.Command("git", "-C", clone, "cat-file", "-e", release+"^{commit}").Run() == nil {
		t.Fatal("the shallow clone already has the release commit")
	}
	return clone, release
}

// A CI checkout has only its own commit: the release commit is fetched by hash, after a source
// that does not have it and a commit (the other twin) that does not exist anywhere.
func TestReleaseCommitFetchedInCI(t *testing.T) {
	clone, release := releaseFixture(t)
	absent := strings.Repeat("0", 39) + "1"
	commits := []string{absent, release}
	sources := []string{filepath.Join(t.TempDir(), "no-such-repository"), "origin"}

	if c, err := releaseCommit(clone, commits, sources, false); c != "" || err == nil {
		t.Fatalf("without fetching: got %q, %v; want no commit and an error", c, err)
	}
	c, err := releaseCommit(clone, commits, sources, true)
	if err != nil || c != release {
		t.Fatalf("with fetching: got %q, %v; want %s", c, err, release)
	}
	if gitRepo(t, clone, "rev-parse", "--is-shallow-repository") != "true" {
		t.Fatal("fetching the release made the CI checkout unshallow")
	}
	if out := gitRepo(t, clone, "archive", "--format=tar", release); out == "" {
		t.Fatal("git archive of the fetched release is empty")
	}
}

// Acceptance 8's upgrade tests skipped in CI because the checkout never has the release: in CI a
// release that cannot be had is an error, on a developer machine still a skip.
func TestReleaseUnavailableFailsInCI(t *testing.T) {
	clone, _ := releaseFixture(t)
	absent := strings.Repeat("0", 39) + "1"
	newBuild := func() *releaseBuild {
		return &releaseBuild{name: "Phase 0", env: "BUNKARR_E2E_PHASE0_BINARY", commits: []string{absent},
			sources: []string{filepath.Join(t.TempDir(), "no-such-repository")}}
	}

	ci := newBuild()
	ci.build(clone, true)
	if ci.skip != "" || ci.err == nil || ci.path != "" {
		t.Fatalf("in CI: skip %q, err %v, path %q; want an error and no skip", ci.skip, ci.err, ci.path)
	}
	if !strings.Contains(ci.err.Error(), "BUNKARR_E2E_PHASE0_BINARY") || !strings.Contains(ci.err.Error(), "fetch-depth: 0") {
		t.Fatalf("the CI error does not say how to fix it: %v", ci.err)
	}

	dev := newBuild()
	dev.build(clone, false)
	if dev.skip == "" || dev.err != nil || dev.path != "" {
		t.Fatalf("on a developer machine: skip %q, err %v, path %q; want a skip", dev.skip, dev.err, dev.path)
	}
}

// In CI the upgrade tests reach build through binary, which must ask for the release (ciRun):
// resolve fails in CI and skips elsewhere, for a release whose binary is not set and whose commits
// are nowhere.
func TestReleaseBinaryUnavailableFailsInCI(t *testing.T) {
	clone, _ := releaseFixture(t)
	absent := strings.Repeat("0", 39) + "1"
	const env = "BUNKARR_E2E_PHASE0_BINARY"
	t.Setenv(env, "")
	for _, tc := range []struct {
		ci       string
		required bool
	}{{"true", true}, {"1", true}, {"", false}, {"false", false}} {
		t.Setenv("CI", tc.ci)
		r := &releaseBuild{name: "Phase 0", env: env, commits: []string{absent},
			sources: []string{filepath.Join(t.TempDir(), "no-such-repository")}, root: clone}
		path, skip, err := r.resolve()
		switch {
		case path != "":
			t.Errorf("CI=%q: got the binary %q of a release that is nowhere", tc.ci, path)
		case tc.required && (skip != "" || err == nil):
			t.Errorf("CI=%q: skip %q, err %v; want an error and no skip", tc.ci, skip, err)
		case !tc.required && (skip == "" || err != nil):
			t.Errorf("CI=%q: skip %q, err %v; want a skip", tc.ci, skip, err)
		}
	}
}

// A source that accepts the connection and never answers must not hang the fetch past its timeout
// (and CI until go test's own timeout): git is killed, and its transport helper, which still holds
// the output, is not waited for.
func TestReleaseFetchStalledSourceTimesOut(t *testing.T) {
	clone, release := releaseFixture(t)
	// Nothing between git and the listener: no proxy, no user or system git configuration.
	for _, v := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY"} {
		t.Setenv(v, "")
	}
	t.Setenv("no_proxy", "*")
	t.Setenv("NO_PROXY", "*")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	accepted := make(chan struct{}, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c) // held open, never read or answered
			mu.Unlock()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() { // closing the connections lets the orphaned helper exit
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})

	const timeout = 2 * time.Second
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- fetchCommit(clone, "http://"+ln.Addr().String()+"/bunkarr.git", release, true, timeout)
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a fetch from a source that never answers succeeded")
		}
		select {
		case <-accepted:
		default:
			t.Fatalf("the fetch failed without reaching the stalled source: %v", err)
		}
		t.Logf("gave up after %v: %v", time.Since(start).Round(100*time.Millisecond), err)
	case <-time.After(timeout + 30*time.Second):
		t.Fatalf("the fetch is still running %v after its %v timeout", time.Since(start).Round(time.Second)-timeout, timeout)
	}
}
