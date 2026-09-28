package rclone

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/proc"
)

// Engine-level operations of the rclone driver: Test and Create of §4.5 (both write nothing but
// the marker, and both run within TestBudget), the marker undo of a failed create, and the
// capabilities the shared planner assumes (§3.3).

// Capabilities is what the planner assumes of an rclone destination (§3.3): no hardlinks
// (other names are link_recorded and listed in links.tsv), case-sensitive names, no invalid
// characters (rclone's encoding maps what a backend rejects, S11), names ending in a dot or a
// space stored as given, and the backend's modification time granularity (S3 1 ns, B2 1 ms,
// SFTP 1 s). An encrypted destination (crypt) counts as enforcing modes (§8.4 step 6): its
// objects are encrypted at rest.
func Capabilities(d engines.Destination) filecopy.Capabilities {
	c := filecopy.Capabilities{TrailingDotSpace: true, MtimeGranularityNs: 1_000_000_000,
		EnforcesModes: d.Encryption == engines.EncryptionCrypt}
	switch d.Kind {
	case engines.S3:
		c.MtimeGranularityNs = 1
	case engines.B2:
		c.MtimeGranularityNs = 1_000_000
	}
	return c
}

// testConn binds dest for a Test or Create: the driver's runner, every command within
// TestBudget and TestRetryBudget.
func (d *Driver) testConn(dest engines.Destination, s engines.Secrets) (*Conn, error) {
	c, err := d.Connect(dest, s, engines.Runtime{RetryBudget: TestRetryBudget})
	if err != nil {
		return nil, err
	}
	c.test = true
	return c, nil
}

// noHostKeys reports an SFTP destination without pinned host keys: no command runs for it, so a
// password never reaches an unverified host (§4.5 step 2).
func noHostKeys(dest engines.Destination) bool {
	return dest.Kind == engines.SFTP && (dest.Remote.SFTP == nil || len(dest.Remote.SFTP.HostKeys) == 0)
}

// reachMessage explains why the storage root could not be listed ("" when it could).
func reachMessage(cmd command, out outcome) string {
	if out.status.Code == 0 && !out.status.Stopped() {
		return ""
	}
	e := fail(cmd, out)
	switch {
	case errors.Is(e, engines.ErrHostKeyChanged):
		return engines.ErrHostKeyChanged.Error() + ": the server presented a key that is not pinned"
	case errors.Is(e, ErrPathNotFound):
		return "bucket or path not found"
	case errors.Is(e, ErrBudget):
		return "the remote did not answer in time"
	case errors.Is(e, ErrRetryBudget):
		return "the remote kept failing: " + e.Message
	case out.status.Code == 1:
		return "cannot connect, or the credentials were refused: " + e.Message
	}
	return e.Error()
}

// Test probes a destination's location and writes nothing (§4.5 steps 3-4): the storage root is
// listed (rclone lsf --max-depth 1 BKDEST:<bucket>/<prefix>; exit 3 is "bucket or path not
// found", exit 1 the credentials or the connection) and its first 1000 top-level entries
// counted; a crypt destination lists its crypt root too, where names that do not decrypt mean a
// wrong password or another crypt remote (unreadable); then the marker is read: missing, ok (id
// and name), unreadable, or foreign when it differs from dest.MarkerID (a stored destination).
// The destinations package turns an ok marker that another row of its database holds into
// foreign. An SFTP destination without pinned host keys runs no command. Test answers negative
// findings in the result; the error is for a runner failure or a cancellation.
func (d *Driver) Test(ctx context.Context, dest engines.Destination, s engines.Secrets) (engines.TestResult, error) {
	res := engines.TestResult{EngineVersion: d.Version}
	if noHostKeys(dest) {
		res.Message = "confirm the SFTP server's host keys first"
		return res, nil
	}
	ctx, cancel := context.WithTimeout(ctx, TestBudget)
	defer cancel()
	c, err := d.testConn(dest, s)
	if err != nil {
		return res, err
	}
	n, out, cmd, err := c.countTop(ctx, StorageRoot(dest))
	if err != nil {
		return res, err
	}
	if msg := reachMessage(cmd, out); msg != "" {
		res.Message = msg
		return res, nil
	}
	res.Reachable, res.Entries = true, n
	if dest.Encryption == engines.EncryptionCrypt && n > 0 {
		_, cout, ccmd, err := c.countTop(ctx, Root(dest))
		if err != nil {
			return res, err
		}
		if cout.undecryptable {
			res.Marker, res.Message = engines.MarkerUnreadable, ErrUndecryptable.Error()
			return res, nil
		}
		if msg := reachMessage(ccmd, cout); msg != "" {
			res.Message = msg
			return res, nil
		}
	}
	m, state, err := c.readMarker(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return res, err
		}
		if errors.Is(err, ErrUndecryptable) {
			res.Marker = engines.MarkerUnreadable
		}
		res.Message = err.Error()
		return res, nil
	}
	switch {
	case state == markerMissing:
		res.Marker = engines.MarkerMissing
		if n > 0 {
			res.Warnings = append(res.Warnings, nonEmptyWarning(n))
		}
	case state == markerGarbage:
		res.Marker = engines.MarkerUnreadable
		res.Message = filecopy.MarkerRel + " exists but is not a Bunkarr marker"
	case dest.MarkerID != "" && m.ID != dest.MarkerID:
		res.Marker, res.ID, res.MarkerName = engines.MarkerForeign, m.ID, m.Name
		res.Message = engines.ErrMarkerMismatch.Error()
	default:
		res.Marker, res.ID, res.MarkerName = engines.MarkerOK, m.ID, m.Name
	}
	if dest.Kind == engines.SFTP {
		if a, err := c.About(ctx); err == nil && a.Free != nil {
			res.FreeBytes = a.Free
		}
	}
	res.OK = res.Marker == engines.MarkerMissing || res.Marker == engines.MarkerOK
	return res, nil
}

func nonEmptyWarning(n int) string {
	count := fmt.Sprint(n)
	if n >= MaxTestEntries {
		count = fmt.Sprintf("at least %d", MaxTestEntries)
	}
	return fmt.Sprintf("the location is not empty (%s entries): the first sync keeps objects that match the source and moves the others into retention", count)
}

// Create writes the marker of a new destination, or attaches an existing one (§4.5):
//   - attach requires a marker and adopts its id;
//   - otherwise the marker must be missing: a new one (a random id, dest.Name, now) is written
//     with rclone rcat <root>/.bunkarr/destination.json and read back; a written marker that
//     does not read back, or a write that failed, is removed again (dropFailedMarker). A
//     non-empty remote is accepted with a warning (the first sync adopts matching objects and
//     moves the others into retention, S23); an object at the marker's path that is not a
//     marker is never replaced.
//
// It runs within TestBudget. The caller removes the marker with RemoveMarker when a later step
// of its create fails, and also when Create fails with a MarkerID: the marker it wrote could not
// be removed, and with crypt only this create's secret reads it.
func (d *Driver) Create(ctx context.Context, dest engines.Destination, s engines.Secrets, attach bool) (engines.CreateResult, error) {
	var res engines.CreateResult
	if noHostKeys(dest) {
		return res, errors.New("rclone create: the SFTP server's host keys are not pinned")
	}
	ctx, cancel := context.WithTimeout(ctx, TestBudget)
	defer cancel()
	c, err := d.testConn(dest, s)
	if err != nil {
		return res, err
	}
	n, out, cmd, err := c.countTop(ctx, StorageRoot(dest))
	if err != nil {
		return res, err
	}
	if out.status.Code != 0 || out.status.Stopped() {
		return res, fail(cmd, out)
	}
	if dest.Encryption == engines.EncryptionCrypt && n > 0 {
		_, cout, ccmd, err := c.countTop(ctx, Root(dest))
		if err != nil {
			return res, err
		}
		if cout.status.Code != 0 || cout.status.Stopped() || cout.undecryptable {
			return res, fail(ccmd, cout)
		}
	}
	m, state, err := c.readMarker(ctx)
	if err != nil {
		return res, err
	}
	if attach {
		if state != markerPresent {
			return res, fmt.Errorf("%w: attach needs an existing Bunkarr destination at this location", engines.ErrMarkerMissing)
		}
		res.MarkerID = m.ID
		return res, nil
	}
	switch state {
	case markerPresent:
		return res, fmt.Errorf("a Bunkarr destination (%q) already exists at this location: attach it instead", m.Name)
	case markerGarbage:
		return res, fmt.Errorf("%s exists but is not a Bunkarr marker; it is never replaced", filecopy.MarkerRel)
	}
	mk := Marker{ID: newUUID(), Name: dest.Name, CreatedAt: d.now().UTC()}
	data, err := json.MarshalIndent(mk, "", "  ")
	if err != nil {
		return res, fmt.Errorf("rclone create: encode marker: %w", err)
	}
	if err := c.Rcat(ctx, filecopy.MarkerRel, append(data, '\n')); err != nil {
		// A write that failed can still have landed (a cancelled or timed-out rcat).
		res.MarkerID = d.dropFailedMarker(ctx, c, dest, mk.ID)
		return res, fmt.Errorf("rclone create: write marker: %w", err)
	}
	back, state, err := c.readMarker(ctx)
	if err != nil || state != markerPresent || back.ID != mk.ID {
		res.MarkerID = d.dropFailedMarker(ctx, c, dest, mk.ID)
		if err == nil {
			err = errors.New("the marker did not read back")
		}
		return res, fmt.Errorf("rclone create: %w", err)
	}
	if n > 0 {
		res.Warnings = append(res.Warnings, nonEmptyWarning(n))
	}
	res.MarkerID = mk.ID
	return res, nil
}

// dropFailedMarker removes the marker id that a failing Create wrote, or may have written. It runs
// on a context that neither the create's budget nor its caller's cancellation ends (bounded by
// TestBudget), because an expired budget or a cancelled request is one way the write or its
// read-back fails. It returns id when the marker may still be there (its removal failed), which
// Create returns with its error; "" when the marker is gone, was never written, or is not ours.
func (d *Driver) dropFailedMarker(ctx context.Context, c *Conn, dest engines.Destination, id string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), TestBudget)
	defer cancel()
	if err := c.removeOwnMarker(ctx, id); err != nil {
		d.log().Warn("rclone create: could not remove the marker of a create that failed", "destination", dest.ID, "error", err)
		return id
	}
	return ""
}

// RemoveMarker removes the marker a Create wrote when a later step of the create failed (§4.5:
// "an rclone marker that was written is removed, as filecopy does"). It removes it only while it
// still carries id; any other marker, or none, is left alone.
func (d *Driver) RemoveMarker(ctx context.Context, dest engines.Destination, s engines.Secrets, id string) error {
	ctx, cancel := context.WithTimeout(ctx, TestBudget)
	defer cancel()
	c, err := d.testConn(dest, s)
	if err != nil {
		return err
	}
	return c.removeOwnMarker(ctx, id)
}

// removeOwnMarker deletes the marker when it reads back with id: the one delete outside
// retention and config versions, sanctioned by §4.5 for a create's own marker.
func (c *Conn) removeOwnMarker(ctx context.Context, id string) error {
	m, state, err := c.readMarker(ctx)
	if err != nil || state != markerPresent || m.ID != id {
		return err
	}
	cmd := command{words: []string{"deletefile"}, budget: c.drv.listingBudget(), args: func(*proc.RunDir) []string {
		return append([]string{c.remote(filecopy.MarkerRel), "--max-delete", "1"}, jsonLog...)
	}}
	out, err := c.run(ctx, cmd)
	if err != nil {
		return err
	}
	return check(cmd, out)
}

// newUUID returns a random (version 4) UUID, the marker's id.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Probe is the reachability of a destination's storage root (§4.5 step 3).
type Probe struct {
	Reachable bool
	// Entries counts the first MaxTestEntries top-level entries.
	Entries int
	// Message says why the root is not reachable ("" when it is).
	Message string
}

// ProbeStorage lists the top level of dest's storage root (rclone lsf --max-depth 1
// BKDEST:<bucket>/<prefix>, never through crypt) within TestBudget: restic's Test uses it for its
// remote repositories, which restic reaches through its rclone backend (D21). An SFTP destination
// without pinned host keys runs nothing. The error is for a runner failure or a cancellation.
func (d *Driver) ProbeStorage(ctx context.Context, dest engines.Destination, s engines.Secrets) (Probe, error) {
	if noHostKeys(dest) {
		return Probe{Message: "confirm the SFTP server's host keys first"}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, TestBudget)
	defer cancel()
	storage := dest
	storage.Encryption = engines.EncryptionNone
	c, err := d.testConn(storage, s)
	if err != nil {
		return Probe{}, err
	}
	n, out, cmd, err := c.countTop(ctx, StorageRoot(storage))
	if err != nil {
		return Probe{}, err
	}
	if msg := reachMessage(cmd, out); msg != "" {
		return Probe{Message: msg}, nil
	}
	return Probe{Reachable: true, Entries: n}, nil
}
