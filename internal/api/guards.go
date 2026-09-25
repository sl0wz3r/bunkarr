package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/catalog"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
)

// pathGuards are the overlap checks of safety rule S4 that span packages: the catalog asks
// whether a source path may be saved, the destinations store whether a target may be, and the
// scanner which directories to skip. Both stores are set after they are constructed (each one's
// guard needs the other store).
type pathGuards struct {
	// configDir is the resolved config directory.
	configDir string
	sources   *catalog.Store
	dests     *destinations.Store
}

// source checks a resolved source path: it may not be the config directory or inside it, and it
// may not equal, contain or be inside a destination target (a source inside a destination would
// back up the backups; a destination inside a source would make every sync scan its own
// output). A source containing the config directory is allowed: the scanner skips it (by
// device and inode).
func (g *pathGuards) source(ctx context.Context, p string) error {
	if within(p, g.configDir) {
		return fmt.Errorf("%s is Bunkarr's config directory or inside it (%s)", p, g.configDir)
	}
	list, err := g.dests.List(ctx)
	if err != nil {
		return fmt.Errorf("check the destinations: %w", err)
	}
	for _, d := range list {
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
// and inode, so bind-mount aliases are caught too): every destination target and the config
// directory.
func (g *pathGuards) forbiddenRoots(ctx context.Context) ([]string, error) {
	list, err := g.dests.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list destination targets: %w", err)
	}
	out := make([]string, 0, len(list)+1)
	out = append(out, g.configDir)
	for _, d := range list {
		out = append(out, d.Target)
	}
	return out, nil
}

// within reports whether p is dir or inside it (both absolute and clean).
func within(p, dir string) bool {
	if p == dir || dir == "/" {
		return true
	}
	return strings.HasPrefix(p, dir+"/")
}
