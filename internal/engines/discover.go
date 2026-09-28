package engines

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// Minimum engine versions (§10.1).
var (
	MinResticVersion = [2]int{0, 17}
	MinRcloneVersion = [2]int{1, 66}
)

// discoverBudget bounds each version command.
const discoverBudget = 60 * time.Second

// BinaryStatus is one engine binary as GET /system/status shows it.
type BinaryStatus struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	Path      string `json:"path,omitempty"`
	// Reason says why the engine is unavailable ("restic is not installed").
	Reason string `json:"reason,omitempty"`
}

// Availability is what Discover found for both engines.
type Availability struct {
	Restic BinaryStatus `json:"restic"`
	Rclone BinaryStatus `json:"rclone"`
}

// Of returns the status of engine k (filecopy is always available).
func (a Availability) Of(k Kind) BinaryStatus {
	switch k {
	case Restic:
		return a.Restic
	case Rclone:
		return a.Rclone
	case Filecopy:
		return BinaryStatus{Available: true}
	}
	return BinaryStatus{Reason: fmt.Sprintf("unknown engine %q", k)}
}

// Check returns nil when engine k is available, else ErrEngineUnavailable with the reason (the
// 400 of Create and Test, and the job gate of §11.1).
func (a Availability) Check(k Kind) error {
	st := a.Of(k)
	if st.Available {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrEngineUnavailable, st.Reason)
}

// Discover runs `restic version` and `rclone version` through r and checks the versions: restic
// 0.17 or newer, rclone 1.66 or newer (§10.1). A binary that config.ResolveEngineBinaries refused
// keeps its reason and is not run. r must be the runner built with the same paths.
func Discover(ctx context.Context, r proc.Runner, restic, rclone config.EngineBinary) Availability {
	return Availability{
		Restic: discover(ctx, r, proc.Restic, restic, MinResticVersion),
		Rclone: discover(ctx, r, proc.Rclone, rclone, MinRcloneVersion),
	}
}

var versionRe = map[proc.Binary]*regexp.Regexp{
	proc.Restic: regexp.MustCompile(`^restic (\d+)\.(\d+)\.(\d+)(\S*)`),
	proc.Rclone: regexp.MustCompile(`^rclone v(\d+)\.(\d+)\.(\d+)(\S*)`),
}

func discover(ctx context.Context, r proc.Runner, b proc.Binary, bin config.EngineBinary, minimum [2]int) BinaryStatus {
	name := string(b)
	st := BinaryStatus{Path: bin.Path}
	switch {
	case bin.Err != "":
		st.Reason = bin.Err
		return st
	case bin.Path == "":
		st.Reason = name + " is not installed"
		return st
	}
	c := proc.Cmd{Binary: b, Args: []string{"version"}, Budget: discoverBudget, IdleTimeout: -1}
	if b == proc.Rclone {
		c.Env = map[string]string{"RCLONE_CONFIG": "/dev/null"}
	}
	p, err := r.Start(ctx, c)
	if err != nil {
		if errors.Is(err, proc.ErrNoBinary) {
			st.Reason = name + " is not installed"
		} else {
			st.Reason = fmt.Sprintf("%s could not be started: %v", name, err)
		}
		return st
	}
	var first string
	for l := range p.Lines() {
		if !l.Stderr && first == "" && strings.TrimSpace(l.Text) != "" {
			first = strings.TrimSpace(l.Text)
		}
	}
	ex, err := p.Wait()
	switch {
	case err != nil:
		st.Reason = fmt.Sprintf("%s version: %v", name, err)
		return st
	case ex.Code != 0 || ex.Stopped():
		msg := fmt.Sprintf("%s version failed (exit %d)", name, ex.Code)
		if n := len(ex.StderrTail); n > 0 {
			msg += ": " + ex.StderrTail[n-1]
		}
		st.Reason = msg
		return st
	}
	m := versionRe[b].FindStringSubmatch(first)
	if m == nil {
		st.Reason = fmt.Sprintf("cannot read the version of %s from %q", name, first)
		return st
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	st.Version = m[1] + "." + m[2] + "." + m[3] + m[4]
	if major < minimum[0] || (major == minimum[0] && minor < minimum[1]) {
		st.Reason = fmt.Sprintf("%s %s.%s.%s is older than %d.%d", name, m[1], m[2], m[3], minimum[0], minimum[1])
		return st
	}
	st.Available = true
	return st
}
