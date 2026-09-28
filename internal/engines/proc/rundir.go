package proc

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
)

// tmpfsMagic is statfs's f_type of tmpfs (TMPFS_MAGIC, linux/magic.h).
const tmpfsMagic = 0x01021994

// DefaultShmDir is where secret run directories go when it is a tmpfs.
const DefaultShmDir = "/dev/shm"

// runSubdir is the directory under ShmDir and under the config directory that holds the run
// directories.
const runSubdir = "bunkarr-run"

// RunDirOptions configures NewRunDirs.
type RunDirOptions struct {
	// ShmDir is the tmpfs mount for secret files (default /dev/shm).
	ShmDir string
	// StatFS returns the filesystem type (statfs f_type) of a path. Default: statfs(2) on Linux;
	// on darwin nothing is ever tmpfs. Tests set it.
	StatFS func(path string) (int64, error)
}

// RunDirs creates the per-command run directories (S22): secret files on tmpfs
// (<ShmDir>/bunkarr-run/<jobId>-<random>/, 0700) when statfs reports TMPFS_MAGIC, else in
// <config>/run/<jobId>-<random>/ with a warning (an appdata pool is snapshotted and backed up by
// plugins, so a secret on that disk can outlive the job); large non-secret files (include lists,
// excludes, samples, combined check output) always in <config>/run/<jobId>-<random>-data/.
type RunDirs struct {
	runDir string // <config>/run
	shm    string // <ShmDir>/bunkarr-run
	statfs func(string) (int64, error)
}

// NewRunDirs returns the run directories of a config directory.
func NewRunDirs(configDir string, o RunDirOptions) *RunDirs {
	shm := o.ShmDir
	if shm == "" {
		shm = DefaultShmDir
	}
	statfs := o.StatFS
	if statfs == nil {
		statfs = statFSType
	}
	return &RunDirs{runDir: filepath.Join(configDir, "run"), shm: shm, statfs: statfs}
}

// onTmpfs reports whether the shm directory is a tmpfs.
func (d *RunDirs) onTmpfs() bool {
	t, err := d.statfs(d.shm)
	return err == nil && t == tmpfsMagic
}

// New creates the run directory of one command of job jobID (0 for a Test or Create).
func (d *RunDirs) New(jobID int64) (*RunDir, error) {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return nil, fmt.Errorf("run directory: %w", err)
	}
	name := strconv.FormatInt(jobID, 10) + "-" + hex.EncodeToString(rnd[:])
	if err := os.MkdirAll(d.runDir, 0o700); err != nil {
		return nil, fmt.Errorf("create %s: %w", d.runDir, err)
	}
	r := &RunDir{data: filepath.Join(d.runDir, name+"-data")}
	secretParent := d.runDir
	if d.onTmpfs() {
		secretParent = filepath.Join(d.shm, runSubdir)
		if err := mkdirPrivate(secretParent); err != nil {
			return nil, err
		}
	} else {
		r.warning = fmt.Sprintf("%s is not a tmpfs: secret files of this command are written to %s (give the container a tmpfs at /dev/shm to keep them off disk)", d.shm, d.runDir)
	}
	r.secret = filepath.Join(secretParent, name)
	if err := os.Mkdir(r.secret, 0o700); err != nil {
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	if err := os.Mkdir(r.data, 0o700); err != nil {
		_ = os.RemoveAll(r.secret)
		return nil, fmt.Errorf("create run directory: %w", err)
	}
	// Mkdir applies the umask; make sure of 0700.
	for _, p := range []string{r.secret, r.data} {
		if err := os.Chmod(p, 0o700); err != nil {
			_ = r.Remove()
			return nil, fmt.Errorf("run directory: %w", err)
		}
	}
	return r, nil
}

// mkdirPrivate creates dir (0700) unless it exists as a real directory owned by us.
func mkdirPrivate(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("check %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to another user (uid %d)", dir, st.Uid)
	}
	if fi.Mode().Perm() != 0o700 {
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}
	return nil
}

// Sweep removes every run directory left in both places (a crash or kill -9 during a command).
// Call it at start-up only, before any command runs: it removes the directories of running
// commands too.
func (d *RunDirs) Sweep() error {
	var errs []error
	for _, parent := range []string{d.runDir, filepath.Join(d.shm, runSubdir)} {
		entries, err := os.ReadDir(parent)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("sweep %s: %w", parent, err))
			continue
		}
		for _, e := range entries {
			if err := os.RemoveAll(filepath.Join(parent, e.Name())); err != nil {
				errs = append(errs, fmt.Errorf("sweep: %w", err))
			}
		}
	}
	return errors.Join(errs...)
}

// RunDir is one command's run directory: a secret directory and a data directory.
type RunDir struct {
	secret, data string
	warning      string
	mu           sync.Mutex
	removed      bool
}

// Warning is the fallback warning when the secret directory is not on tmpfs ("" when it is).
func (r *RunDir) Warning() string { return r.warning }

// SecretDir is the secret directory (0700).
func (r *RunDir) SecretDir() string { return r.secret }

// DataDir is the data directory (0700); the child's HOME.
func (r *RunDir) DataDir() string { return r.data }

// SecretPath is the path of secret file name (password, known_hosts, ca.pem). name must be a
// plain file name; anything else is a programming error and panics.
func (r *RunDir) SecretPath(name string) string { return filepath.Join(r.secret, plainName(name)) }

// DataPath is the path of data file name (files, excludes, sample, combined).
func (r *RunDir) DataPath(name string) string { return filepath.Join(r.data, plainName(name)) }

func plainName(name string) string {
	if !fileNameRe.MatchString(name) || name == "." || name == ".." {
		panic(fmt.Sprintf("proc: run file name %q is not a plain file name", name))
	}
	return name
}

// WriteSecret creates secret file name with O_CREATE|O_EXCL|O_NOFOLLOW and mode 0600 (S22).
func (r *RunDir) WriteSecret(name string, data []byte) error {
	return writeNew(r.SecretPath(name), data)
}

// WriteData creates data file name (0600, O_EXCL|O_NOFOLLOW).
func (r *RunDir) WriteData(name string, data []byte) error {
	return writeNew(r.DataPath(name), data)
}

func writeNew(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	// The umask may have narrowed, never widened, the mode; make it exactly 0600.
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("chmod %s: %w", filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

// Remove deletes both directories and everything in them. It is safe to call more than once.
func (r *RunDir) Remove() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.removed {
		return nil
	}
	err := errors.Join(os.RemoveAll(r.secret), os.RemoveAll(r.data))
	if err == nil {
		r.removed = true
	}
	return err
}
