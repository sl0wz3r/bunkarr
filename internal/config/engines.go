package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"unicode"
)

// EngineBinary is the resolved program of one backup engine (restic or rclone).
type EngineBinary struct {
	// Path is the program's absolute path after symlinks are resolved (or, when Err is set, the
	// path that was checked, "" when none was found).
	Path string
	// Err is why the engine is unavailable ("" = usable): the reason shown in GET /system/status
	// and in the Create and Test answers ("restic is not installed").
	Err string
}

// Available reports whether the binary passed the checks.
func (b EngineBinary) Available() bool { return b.Err == "" && b.Path != "" }

// ResolveEngineBinaries finds and checks the engine programs at start-up (docs/design/phase4.md
// §4.4): BUNKARR_RESTIC_PATH / BUNKARR_RCLONE_PATH (Env.ResticPath, Env.RclonePath), else a PATH
// lookup. After filepath.EvalSymlinks each must be an absolute path to a regular, executable file
// without whitespace or quote characters (restic shell-splits -o rclone.program=) that the
// current user cannot write (a program Bunkarr could replace would receive every destination's
// secrets). A binary that fails a check is unavailable with the reason in Err; the other engine
// is unaffected.
func ResolveEngineBinaries(env Env) (restic, rclone EngineBinary) {
	return resolveEngineBinary("restic", "BUNKARR_RESTIC_PATH", env.ResticPath),
		resolveEngineBinary("rclone", "BUNKARR_RCLONE_PATH", env.RclonePath)
}

func resolveEngineBinary(name, variable, configured string) EngineBinary {
	p := configured
	if p == "" {
		found, err := exec.LookPath(name)
		if err != nil {
			return EngineBinary{Err: name + " is not installed"}
		}
		p = found
	} else if !filepath.IsAbs(p) {
		return EngineBinary{Path: p, Err: fmt.Sprintf("%s=%q is not an absolute path", variable, p)}
	}
	if reason := unsafePathChars(p); reason != "" {
		return EngineBinary{Path: p, Err: fmt.Sprintf("%s path %q %s", name, p, reason)}
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return EngineBinary{Path: p, Err: fmt.Sprintf("%s is not installed (%s does not exist)", name, p)}
		}
		return EngineBinary{Path: p, Err: fmt.Sprintf("%s: %v", name, err)}
	}
	if !filepath.IsAbs(resolved) {
		return EngineBinary{Path: resolved, Err: fmt.Sprintf("%s resolves to %q, which is not an absolute path", name, resolved)}
	}
	if reason := unsafePathChars(resolved); reason != "" {
		return EngineBinary{Path: resolved, Err: fmt.Sprintf("%s path %q %s", name, resolved, reason)}
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return EngineBinary{Path: resolved, Err: fmt.Sprintf("%s: %v", name, err)}
	}
	if !fi.Mode().IsRegular() {
		return EngineBinary{Path: resolved, Err: fmt.Sprintf("%s path %q is not a regular file", name, resolved)}
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return EngineBinary{Path: resolved, Err: fmt.Sprintf("%s path %q is not executable", name, resolved)}
	}
	if writableByUs(fi) {
		return EngineBinary{Path: resolved, Err: fmt.Sprintf("%s path %q is writable by Bunkarr's user (uid %d); install it read-only (e.g. owned by root, mode 0755)", name, resolved, os.Getuid())}
	}
	return EngineBinary{Path: resolved}
}

// unsafePathChars returns why p cannot be passed to an engine ("" when it can): whitespace,
// control characters, quotes and backslashes, which a shell-split would change.
func unsafePathChars(p string) string {
	for _, r := range p {
		switch {
		case unicode.IsSpace(r):
			return "contains whitespace"
		case unicode.IsControl(r):
			return "contains a control character"
		case r == '"' || r == '\'' || r == '`' || r == '\\':
			return "contains a quote or backslash"
		}
	}
	return ""
}

// writableByUs reports whether the current user can write the file through its permission bits:
// as its owner (except root, which can write any file and would otherwise never pass), through a
// group it belongs to, or as anyone.
func writableByUs(fi fs.FileInfo) bool {
	perm := fi.Mode().Perm()
	if perm&0o002 != 0 {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return perm&0o222 != 0
	}
	uid := os.Getuid()
	if uid != 0 && int(st.Uid) == uid && perm&0o200 != 0 {
		return true
	}
	if perm&0o020 != 0 {
		gid := int(st.Gid)
		if gid == os.Getgid() || gid == os.Getegid() {
			return true
		}
		if groups, err := os.Getgroups(); err == nil && slices.Contains(groups, gid) {
			return true
		}
		if uid == 0 {
			return true
		}
	}
	return false
}
