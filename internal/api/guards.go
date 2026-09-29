package api

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// pathGuards are the overlap checks of safety rule S4 that span packages: the catalog asks
// whether a source path may be saved, the destinations store whether a target may be, and the
// scanner which directories to skip. Both stores are set after they are constructed (each one's
// guard needs the other store).
type pathGuards struct {
	// configDir is the resolved config directory.
	configDir string
	// runRoot is where engine commands keep their secret files while they run (S22:
	// <ShmDir>/bunkarr-run, resolved like a source path; "" for none).
	runRoot string
	sources *catalog.Store
	dests   *destinations.Store
}

// systemRoots are never sources, nor inside one: /proc holds every process's environment (an
// rclone child's carries its remote's credentials) and memory, /sys the kernel's state, /dev the
// devices and /dev/shm, where engine commands write their secret files. A scan of one would
// catalog those and a sync copy them to a share in the clear.
var systemRoots = []string{"/dev", "/proc", "/sys"}

// source checks a resolved source path: it may not be a system directory or inside one, may not
// equal, contain or be inside the secret run directory, may not be the config directory or inside
// it, and it may not equal, contain or be inside a destination target (a source inside a
// destination would back up the backups; a destination inside a source would make every sync scan
// its own output). A source containing the config directory is allowed: the scanner skips it (by
// device and inode, and any folder holding a copy of bunkarr.key with the same content, which
// catches another view of it such as /mnt/user vs a pool path on Unraid). One containing the run
// directory is not: that directory appears at the first engine command after a start, so a scan
// that began before would not know to skip it.
func (g *pathGuards) source(ctx context.Context, p string) error {
	for _, root := range systemRoots {
		if within(p, root) {
			return fmt.Errorf("%s is a system directory (%s holds devices, processes or the kernel's state, never media)", p, root)
		}
	}
	if g.runRoot != "" && (within(p, g.runRoot) || within(g.runRoot, p)) {
		return fmt.Errorf("%s holds or is inside the directory of Bunkarr's secret run files (%s)", p, g.runRoot)
	}
	if within(p, g.configDir) {
		return fmt.Errorf("%s is Bunkarr's config directory or inside it (%s)", p, g.configDir)
	}
	list, err := g.dests.List(ctx)
	if err != nil {
		return fmt.Errorf("check the destinations: %w", err)
	}
	for _, d := range list {
		if d.Kind.Remote() {
			continue // no local path: its target is a display location (phase4.md §4.2)
		}
		switch {
		case p == d.Target:
			return fmt.Errorf("%s is the target of destination %q", p, d.Name)
		case within(p, d.Target):
			return fmt.Errorf("%s is inside the target of destination %q (%s)", p, d.Name, d.Target)
		case within(d.Target, p):
			return fmt.Errorf("%s contains the target of destination %q (%s)", p, d.Name, d.Target)
		}
	}
	return nil
}

// destination checks a resolved destination target against the sources: it may not equal,
// contain or be inside any of them. (The destinations store itself refuses /, the config
// directory and other destinations.)
func (g *pathGuards) destination(ctx context.Context, target string) error {
	list, err := g.sources.List(ctx)
	if err != nil {
		return fmt.Errorf("check the sources: %w", err)
	}
	for _, s := range list {
		switch {
		case target == s.Path:
			return destinations.ValidationError(fmt.Sprintf("the target %s is source %q", target, s.Name))
		case within(target, s.Path):
			return destinations.ValidationError(fmt.Sprintf("the target %s is inside source %q (%s)", target, s.Name, s.Path))
		case within(s.Path, target):
			return destinations.ValidationError(fmt.Sprintf("the target %s contains source %q (%s)", target, s.Name, s.Path))
		}
	}
	return nil
}

// forbiddenRoots are the directories a scan skips when it meets them inside a source (by device
// and inode, so bind-mount aliases are caught too): every local destination target (filecopy
// shares and local restic repositories), the config directory and the secret run directory (for
// a source saved before source refused it). SFTP, S3 and B2 destinations have no local path.
func (g *pathGuards) forbiddenRoots(ctx context.Context) ([]string, error) {
	list, err := g.dests.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list destination targets: %w", err)
	}
	out := make([]string, 0, len(list)+2)
	out = append(out, g.configDir)
	if g.runRoot != "" {
		out = append(out, g.runRoot)
	}
	for _, d := range list {
		if d.Kind.Remote() {
			continue
		}
		out = append(out, d.Target)
	}
	return out, nil
}

// resolvedRunRoot is d's secret run directory with the symlinks of its parent resolved, as a
// source path is stored (S1), so the guard compares like with like; the directory itself may not
// exist yet (it is made at the first engine command).
func resolvedRunRoot(d *proc.RunDirs) string {
	root := filepath.Clean(d.SecretRoot())
	if parent, err := filepath.EvalSymlinks(filepath.Dir(root)); err == nil {
		return filepath.Join(parent, filepath.Base(root))
	}
	return root
}

// within reports whether p is dir or inside it (both absolute and clean).
func within(p, dir string) bool {
	if p == dir || dir == "/" {
		return true
	}
	return strings.HasPrefix(p, dir+"/")
}
