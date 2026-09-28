package destinations

import (
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/netguard"
)

// Engine destinations: create, attach and test (phase4.md §4.5, S21, S25, S26, D32).

// PointCreateAfterInit is the fault point right after an engine call that initialized a
// repository (CreateResult.Initialized): the pending row and its sealed secret must survive a
// crash there (§4.5, §14.3).
const PointCreateAfterInit = "create.afterInit"

// pendingPrefix marks the marker_id of a create that has not finished (S25: it matches nothing).
const pendingPrefix = "pending:"

// engineCallBudget bounds a create, attach or test of an engine destination (S26).
const engineCallBudget = 60 * time.Second

// engine returns the available driver of an engine destination's engine.
func (s *Store) engine(engine string) (engines.Engine, error) {
	if s.opts.Engines != nil {
		if e, ok := s.opts.Engines(engines.Kind(engine)); ok && e != nil {
			return e, nil
		}
	}
	return nil, ValidationError(fmt.Sprintf("%s is not installed or not usable on this server (see the system status)", engine))
}

// checkKindEngine checks a kind and engine pair (§4.1) and returns them with their defaults:
// local defaults to filecopy; a remote kind needs restic or rclone.
func checkKindEngine(kind engines.DestKind, engine string) (engines.DestKind, string, error) {
	if kind == "" {
		kind = engines.Local
	}
	if !kind.Valid() {
		return "", "", ValidationError(fmt.Sprintf("kind %q: use local, sftp, s3 or b2", kind))
	}
	switch engine {
	case "":
		if kind.Remote() {
			return "", "", ValidationError(fmt.Sprintf("engine: a %s destination uses restic or rclone", kind))
		}
		engine = EngineFilecopy
	case EngineFilecopy:
		if kind.Remote() {
			return "", "", ValidationError(fmt.Sprintf("engine filecopy writes to local or mounted paths only; a %s destination uses restic or rclone", kind))
		}
	case EngineRestic:
	case EngineRclone:
		if !kind.Remote() {
			return "", "", ValidationError("engine rclone: a local path uses filecopy or restic")
		}
	default:
		return "", "", ValidationError(fmt.Sprintf("engine %q: use filecopy, restic or rclone", engine))
	}
	return kind, engine, nil
}

// hostRefusedError is a remote host that the outbound guard refused: a ValidationError (400) that
// also matches netguard.ErrBlocked.
func hostRefusedError(host string, err error) error {
	return fmt.Errorf("%w: %w", ValidationError(fmt.Sprintf("remote host %s is refused", host)), err)
}

// checkHosts passes every host the engine will dial for the remote through the outbound guard
// (S25): the names engines.DialHosts gives (the endpoint host and <bucket>.<endpoint host>
// whatever forcePathStyle says, the AWS region endpoint and its bucket host,
// api.backblazeb2.com, the SFTP host).
func (s *Store) checkHosts(ctx context.Context, kind engines.DestKind, r engines.Remote) error {
	for _, h := range engines.DialHosts(kind, r) {
		if err := s.checkHost(ctx, h); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) checkHost(ctx context.Context, host string) error {
	check := s.opts.CheckHost
	if check == nil {
		check = netguard.CheckHost
	}
	if err := check(ctx, host); err != nil {
		return hostRefusedError(host, err)
	}
	return nil
}

// createEngine creates (or attaches) a restic or rclone destination (§4.5). Under the create
// lock with a 60 s budget: the input is validated; a local repository gets Phase 1's S3/S4 checks
// (allowLocal); a remote one its host checks, the B2 key check and the overlap rule. Then the row
// is inserted with enabled 0, marker_id "pending:<uuid>", the sealed credentials and encryption
// secret, secret_origin and (restic) a new engine_tag, in one transaction; the engine creates or
// attaches; and the row gets its marker and is enabled. When the engine call fails before it
// initialized anything the row is deleted; after a restic init the pending row stays (its secret
// opens the new repository and its kit can be exported), and a Create of the same location
// resumes it. A create whose location equals a pending row's resumes that row.
func (s *Store) createEngine(ctx context.Context, in Input, o CreateOptions, kind engines.DestKind, engine string) (Destination, error) {
	ctx, cancel := context.WithTimeout(ctx, s.createBudget)
	defer cancel()
	name, err := checkName(in.Name)
	if err != nil {
		return Destination{}, err
	}
	eng, err := s.engine(engine)
	if err != nil {
		return Destination{}, err
	}
	settings, retention := Settings{}, Retention{}
	if in.Settings != nil {
		settings = *in.Settings
	}
	if in.Retention != nil {
		retention = *in.Retention
	}
	if settings, err = settings.NormalizeFor(engine, kind); err != nil {
		return Destination{}, err
	}
	if retention, err = retention.NormalizeFor(engine); err != nil {
		return Destination{}, err
	}
	sj, rj, err := marshalConfig(settings, retention)
	if err != nil {
		return Destination{}, err
	}
	bw := sql.NullString{String: "{}", Valid: true}
	if in.Bandwidth != nil {
		if bw, err = bandwidthJSON(*in.Bandwidth); err != nil {
			return Destination{}, err
		}
	}
	plan, err := planEncryption(engine, kind, in.Encryption, o.Attach)
	if err != nil {
		return Destination{}, err
	}
	if err := in.Credentials.check(); err != nil {
		return Destination{}, err
	}
	var creds engines.Credentials
	if in.Credentials != nil {
		creds = in.Credentials.Credentials
	}
	if err := checkCompleteCredentials(kind, creds); err != nil {
		return Destination{}, err
	}
	remote, warnings, err := decodeRemote(in.Remote, kind, remoteCheck{ctx: ctx, lookup: s.opts.LookupHost, requireHostKeys: true})
	if err != nil {
		return Destination{}, err
	}
	if s.opts.Keyring == nil {
		return Destination{}, errNoKeyring
	}

	row := newRow{name: name, kind: kind, engine: engine, settings: sj, retention: rj, bandwidth: bw.String, plan: plan, creds: creds,
		remote: remote, enabled: in.Enabled == nil || *in.Enabled, sources: in.SourceIDs}
	var resume *Destination
	if kind == engines.Local {
		if resume, err = s.checkLocalRepository(ctx, &row, in.Target, engine, o); err != nil {
			return Destination{}, err
		}
	} else {
		if in.Target != "" {
			return Destination{}, ValidationError("target: a remote destination is located by its remote")
		}
		row.target, row.fsType = displayTarget(kind, remote), string(kind)
		if err := s.checkHosts(ctx, kind, remote); err != nil {
			return Destination{}, err
		}
		if resume, err = s.checkRemoteOverlap(ctx, kind, engine, remote); err != nil {
			return Destination{}, err
		}
		if kind == engines.B2 {
			w, err := s.authorizeB2(ctx, remote.B2.Bucket, creds)
			if err != nil {
				return Destination{}, err
			}
			warnings = append(warnings, w...)
		}
	}
	if resume != nil {
		return s.resumeCreate(ctx, *resume, row, eng, o, warnings)
	}
	if taken, err := s.nameTaken(ctx, name); err != nil {
		return Destination{}, err
	} else if taken {
		return Destination{}, ErrNameTaken
	}
	if engine == EngineRestic {
		row.engineTag = newEngineTag()
	}
	id, err := s.insertPending(ctx, row)
	if err != nil {
		return Destination{}, err
	}
	d, err := s.Get(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	sec := s.secretsOf(id, creds, plan.secret)
	registerSecrets(id, sec)
	res, err := eng.Create(ctx, d.EngineDestination(), sec, o.Attach)
	if res.Initialized {
		faultinject.Point(PointCreateAfterInit)
	}
	if err != nil {
		err = redactErr(err, sec.Values())
		if o.Attach && plan.mode == engines.EncryptionCrypt && plan.secret.CryptPassword2 == "" && errors.Is(err, rclone.ErrUndecryptable) {
			// The kit of a crypt remote whose passwords Bunkarr generated prints a password2 as well (§5.3).
			err = fmt.Errorf("%w; if the recovery kit lists a password2 (every crypt remote whose passwords Bunkarr "+
				"generated), give the recovery kit's \"rclone crypt password2\" as encryption.secret2", err)
		}
		if !res.Initialized {
			if kept := s.dropCreatedMarker(ctx, eng, d, sec, res.MarkerID, o.Attach, err); kept != nil {
				return Destination{}, kept
			}
			s.dropPending(id)
			return Destination{}, fmt.Errorf("create destination %q: %w", name, err)
		}
		s.log.Warn("a destination's repository was initialized but its create did not finish", "id", id, "name", name, "error", err.Error())
		return Destination{}, fmt.Errorf("create destination %q: the repository was initialized but the create did not finish (%w); "+
			"create it again to finish, or delete it with confirmLoseSecret; its recovery kit can be exported", name, err)
	}
	if err := s.finishCreate(ctx, id, res.MarkerID, row.enabled, o.Attach); err != nil {
		if res.Initialized {
			return Destination{}, err
		}
		if kept := s.dropCreatedMarker(ctx, eng, d, sec, res.MarkerID, o.Attach, err); kept != nil {
			return Destination{}, kept
		}
		s.dropPending(id)
		return Destination{}, err
	}
	s.log.Info("destination created", "id", id, "name", name, "engine", engine, "kind", string(kind), "target", row.target, "attached", o.Attach)
	out, err := s.Get(ctx, id)
	if err != nil {
		return Destination{}, err
	}
	out.Warnings = append(warnings, redactAll(res.Warnings, sec.Values())...)
	return out, nil
}

// newRow is an engine destination row to insert.
type newRow struct {
	name, engine, target, fsType string
	kind                         engines.DestKind
	settings, retention          string
	bandwidth                    string
	plan                         encryptionPlan
	creds                        engines.Credentials
	remote                       engines.Remote
	engineTag                    string
	rootDev                      uint64
	enabled                      bool
	sources                      []int64
}

// checkLocalRepository applies Phase 1's S3/S4 checks to a local restic repository's target
// (it must exist; the overlap rules; a local filesystem needs allowLocal) and fills the row's
// target, filesystem type and device. It returns the pending row of a create of the same target
// that did not finish, which the create then resumes.
func (s *Store) checkLocalRepository(ctx context.Context, row *newRow, target, engine string, o CreateOptions) (*Destination, error) {
	resolved, err := resolveTarget(target)
	if err != nil {
		return nil, err
	}
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		if d.Kind == engines.Local && d.Pending && d.Target == resolved && d.Engine == engine {
			return &d, nil
		}
	}
	if err := s.guard(ctx, resolved); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(resolved)
	if err != nil {
		return nil, fmt.Errorf("open target %s: %w", resolved, err)
	}
	defer root.Close()
	fsStat, err := s.statFS(root)
	if err != nil {
		return nil, err
	}
	rootStat, err := filecopy.RootStat(root)
	if err != nil {
		return nil, fmt.Errorf("stat target %s: %w", resolved, err)
	}
	if s.isLocal(fsStat.Type, rootStat.Dev) && !o.AllowLocal {
		return nil, fmt.Errorf("%w (%s, the same disk as the container or Bunkarr's config): a backup there does not survive a disk failure; set allowLocal to use it anyway", ErrLocalFilesystem, fsStat.Type)
	}
	row.target, row.fsType, row.rootDev = resolved, fsStat.Type, rootStat.Dev
	return nil, nil
}

// checkRemoteOverlap applies the overlap rule for remotes (§4.2): no two destinations share a
// storage location with one prefix equal to or inside the other, whatever their kinds. The
// pending row of a create of exactly this location with this engine is returned instead (the
// create resumes it).
func (s *Store) checkRemoteOverlap(ctx context.Context, kind engines.DestKind, engine string, r engines.Remote) (*Destination, error) {
	loc, _ := remoteLocation(kind, r)
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range all {
		other, ok := remoteLocation(d.Kind, d.Remote)
		if !ok || !loc.overlapsWith(other) {
			continue
		}
		if d.Pending && other == loc && d.Engine == engine && d.Kind == kind {
			return &d, nil
		}
		return nil, ValidationError(fmt.Sprintf("the location overlaps destination %q (%s): two destinations may not share a location", d.Name, d.Target))
	}
	return nil, nil
}

// insertPending inserts a pending engine destination in one transaction and returns its id. The
// id is chosen inside the transaction (AUTOINCREMENT's next value) because it is part of the
// sealed columns' AAD: encryption_secret is written by this INSERT only, never by an UPDATE.
func (s *Store) insertPending(ctx context.Context, r newRow) (int64, error) {
	remoteJSON, err := json.Marshal(r.remote)
	if err != nil {
		return 0, fmt.Errorf("encode remote: %w", err)
	}
	var id int64
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT MAX(COALESCE((SELECT seq FROM sqlite_sequence WHERE name = 'destinations'), 0),
			COALESCE((SELECT MAX(id) FROM destinations), 0)) + 1`).Scan(&id); err != nil {
			return fmt.Errorf("choose the destination id: %w", err)
		}
		creds, err := s.sealCredentials(id, r.creds)
		if err != nil {
			return err
		}
		secret, err := s.sealSecret(id, r.plan.secret)
		if err != nil {
			return err
		}
		now := db.FormatTime(s.now())
		_, err = tx.ExecContext(ctx, `INSERT INTO destinations (id, name, engine, target, marker_id, credentials, settings, retention,
			bandwidth, fs_type, root_dev, capabilities, enabled, created_at, updated_at, kind, remote, encryption, encryption_secret,
			secret_origin, engine_tag) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', 0, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, r.name, r.engine, r.target, pendingPrefix+newUUID(), creds, r.settings, r.retention, r.bandwidth, r.fsType, int64(r.rootDev),
			now, now, string(r.kind), string(remoteJSON), string(r.plan.mode), secret, nullString(r.plan.origin), nullString(r.engineTag))
		if err != nil {
			return mapConstraint(err)
		}
		return setSourcesTx(ctx, tx, id, r.sources)
	})
	if err != nil {
		return 0, err
	}
	return id, nil
}

// nullString is s, or NULL when it is empty.
func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

// finishCreate records the engine's marker id on a pending row and enables it (as asked); an
// attach confirms the recovery kit's custody at once (the secret already opened the repository,
// S21).
func (s *Store) finishCreate(ctx context.Context, id int64, markerID string, enabled, attach bool) error {
	if markerID == "" || strings.HasPrefix(markerID, pendingPrefix) {
		return fmt.Errorf("create destination %d: the engine returned no marker id", id)
	}
	now := db.FormatTime(s.now())
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		var confirmed sql.NullString
		if attach {
			confirmed = sql.NullString{String: now, Valid: true}
		}
		res, err := tx.ExecContext(ctx, `UPDATE destinations SET marker_id = ?, enabled = ?, kit_confirmed_at = COALESCE(?, kit_confirmed_at),
			updated_at = ? WHERE id = ? AND marker_id LIKE 'pending:%'`, markerID, enabled, confirmed, now, id)
		if err != nil {
			return mapConstraint(err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("create destination %d: the pending row changed meanwhile", id)
		}
		return nil
	})
}

// markerRemover is an engine whose Create writes a marker that a create which then fails removes
// again (rclone.Driver.RemoveMarker, §4.5: "an rclone marker that was written is removed").
type markerRemover interface {
	RemoveMarker(ctx context.Context, dest engines.Destination, s engines.Secrets, id string) error
}

// dropCreatedMarker removes the marker markerID that eng's create wrote for the pending row d
// before the create failed with cause: after Create returned, or inside it, when Create could not
// remove it itself (engines.CreateResult.MarkerID). rclone writes its marker first, and with crypt
// only d's secret reads it, so a marker left behind would block every later create of the
// location. It returns nil when the pending row may go (an attach, which wrote nothing; no
// marker; or it was removed). Otherwise the pending row stays, a create of the same location
// resumes it, and the error says so.
func (s *Store) dropCreatedMarker(ctx context.Context, eng engines.Engine, d Destination, sec engines.Secrets, markerID string, attach bool, cause error) error {
	if attach || markerID == "" {
		return nil
	}
	rmErr := s.removeCreatedMarker(ctx, eng, d.EngineDestination(), sec, markerID)
	if rmErr == nil {
		return nil
	}
	rmErr = redactErr(rmErr, sec.Values())
	s.log.Warn("a destination's marker was written but its create did not finish, and the marker could not be removed",
		"id", d.ID, "name", d.Name, "error", cause.Error(), "removeError", rmErr.Error())
	return fmt.Errorf("create destination %q: the marker was written but the create did not finish (%w); "+
		"create it again to finish, or delete it with confirmLoseSecret; its recovery kit can be exported", d.Name, cause)
}

// removeCreatedMarker removes the marker markerID that eng's Create wrote for dest. It runs on a
// context that the create's budget does not cancel (the engine bounds the call itself), because
// an expired budget is one way the create fails after the marker was written.
func (s *Store) removeCreatedMarker(ctx context.Context, eng engines.Engine, dest engines.Destination, sec engines.Secrets, markerID string) error {
	rm, ok := eng.(markerRemover)
	if !ok {
		return fmt.Errorf("the %s engine cannot remove the marker it wrote", eng.Kind())
	}
	return rm.RemoveMarker(context.WithoutCancel(ctx), dest, sec, markerID)
}

// dropPending deletes a pending row whose engine call initialized nothing (and releases its
// secrets from the redaction).
func (s *Store) dropPending(id int64) {
	err := s.db.Write(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM destinations WHERE id = ? AND marker_id LIKE 'pending:%'`, id)
		return err
	})
	if err != nil {
		s.log.Error("could not delete the row of a destination that was not created", "id", id, "error", err.Error())
		return
	}
	logging.SetSecrets(secretOwner(id))
}

// resumeCreate finishes the create of a pending row (§4.5): with its own sealed secret, it
// attaches the repository an earlier create initialized (or, when that create stopped before
// the engine ran, initializes it now). The request's name, settings, retention, bandwidth,
// sources and storage credentials replace the pending row's; its encryption must be the same.
func (s *Store) resumeCreate(ctx context.Context, pending Destination, row newRow, eng engines.Engine, o CreateOptions, warnings []string) (Destination, error) {
	_, stored, err := s.SecretsFor(ctx, pending.ID)
	if err != nil {
		return Destination{}, err
	}
	switch {
	case row.plan.mode != pending.Encryption.Mode:
		return Destination{}, ValidationError(fmt.Sprintf("a create of this location did not finish (destination %q) with encryption %s: finish it with the same encryption, or delete it",
			pending.Name, pending.Encryption.Mode))
	case row.plan.origin == OriginUser &&
		subtle.ConstantTimeCompare([]byte(secretText(row.plan.secret)), []byte(secretText(stored.Encryption))) != 1:
		return Destination{}, ValidationError(fmt.Sprintf("a create of this location did not finish (destination %q) with another secret: finish it without a secret (its own is kept), or delete it",
			pending.Name))
	}
	if row.name != pending.Name {
		if taken, err := s.nameTaken(ctx, row.name); err != nil {
			return Destination{}, err
		} else if taken {
			return Destination{}, ErrNameTaken
		}
	}
	remoteJSON, err := json.Marshal(row.remote)
	if err != nil {
		return Destination{}, fmt.Errorf("encode remote: %w", err)
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		creds, err := s.sealCredentials(pending.ID, row.creds)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE destinations SET name = ?, settings = ?, retention = ?, bandwidth = ?, remote = ?,
			credentials = ?, updated_at = ? WHERE id = ? AND marker_id LIKE 'pending:%'`,
			row.name, row.settings, row.retention, row.bandwidth, string(remoteJSON), creds, db.FormatTime(s.now()), pending.ID)
		if err != nil {
			return mapConstraint(err)
		}
		return setSourcesTx(ctx, tx, pending.ID, row.sources)
	})
	if err != nil {
		return Destination{}, err
	}
	d, err := s.Get(ctx, pending.ID)
	if err != nil {
		return Destination{}, err
	}
	sec := s.secretsOf(d.ID, row.creds, stored.Encryption)
	registerSecrets(d.ID, sec)
	s.log.Info("resuming the create of a destination that did not finish", "id", d.ID, "name", d.Name)
	res, err := eng.Create(ctx, d.EngineDestination(), sec, true)
	if err != nil && !o.Attach && (errors.Is(err, engines.ErrRepositoryMissing) || errors.Is(err, engines.ErrMarkerMissing)) {
		// The earlier create stopped before its engine call: nothing to attach, initialize now.
		res, err = eng.Create(ctx, d.EngineDestination(), sec, false)
		if res.Initialized {
			faultinject.Point(PointCreateAfterInit)
		}
	}
	if err != nil {
		return Destination{}, fmt.Errorf("finish the create of destination %q: %w (it stays pending)", d.Name, redactErr(err, sec.Values()))
	}
	if err := s.finishCreate(ctx, d.ID, res.MarkerID, row.enabled, o.Attach); err != nil {
		return Destination{}, err
	}
	out, err := s.Get(ctx, d.ID)
	if err != nil {
		return Destination{}, err
	}
	out.Warnings = append(warnings, redactAll(res.Warnings, sec.Values())...)
	return out, nil
}

// redactAll redacts values from each text.
func redactAll(texts []string, values []string) []string {
	out := make([]string, 0, len(texts))
	for _, t := range texts {
		out = append(out, logging.RedactValues(t, values...))
	}
	return out
}

// TestInput is POST /destinations/test for an engine destination (§4.5): the connection fields of
// a destination to create. It never names a stored destination, so no stored secret is used.
type TestInput struct {
	Kind   engines.DestKind `json:"kind"`
	Engine string           `json:"engine"`
	// Target is a local restic repository's directory.
	Target      string            `json:"target"`
	Remote      json.RawMessage   `json:"remote"`
	Credentials *CredentialsInput `json:"credentials"`
	Encryption  *EncryptionInput  `json:"encryption"`
}

// TestRemote probes a restic or rclone location before it is created and writes nothing (§4.5).
// It never takes a destination id or reads a stored secret: only in's. The outbound guard checks
// the dial hosts first; an sftp remote without hostKeys answers the server's presented keys
// (ok false) and runs no engine command, so a typed password never reaches an unverified host;
// a B2 key is checked with b2_authorize_account. The engine's answer (and every error) has the
// request's secrets, clear and obscured, redacted. Problems with the location are reported in
// the result; the error is for invalid input and internal failures.
func (s *Store) TestRemote(ctx context.Context, in TestInput) (engines.TestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.createBudget)
	defer cancel()
	kind, engine, err := checkKindEngine(in.Kind, in.Engine)
	if err != nil {
		return engines.TestResult{}, err
	}
	if engine == EngineFilecopy {
		return engines.TestResult{}, ValidationError("test a filecopy target with its target only")
	}
	eng, err := s.engine(engine)
	if err != nil {
		return engines.TestResult{}, err
	}
	remote, warnings, err := decodeRemote(in.Remote, kind, remoteCheck{ctx: ctx, lookup: s.opts.LookupHost})
	if err != nil {
		return engines.TestResult{}, err
	}
	if err := in.Credentials.check(); err != nil {
		return engines.TestResult{}, err
	}
	var creds engines.Credentials
	if in.Credentials != nil {
		creds = in.Credentials.Credentials
	}
	if err := checkCompleteCredentials(kind, creds); err != nil {
		return engines.TestResult{}, err
	}
	plan, err := planEncryption(engine, kind, in.Encryption, false)
	if err != nil {
		return engines.TestResult{}, err
	}
	sec := requestSecrets(creds, plan.secret)
	values := sec.Values()
	d := engines.Destination{Name: "test", Engine: engines.Kind(engine), Kind: kind, Remote: remote, Encryption: plan.mode,
		FSType: string(kind), Transfers: DefaultTransfers}
	fail := func(msg string) (engines.TestResult, error) {
		return engines.TestResult{Message: logging.RedactValues(msg, values...), Warnings: redactAll(warnings, values)}, nil
	}
	if kind == engines.Local {
		resolved, err := resolveTarget(in.Target)
		if err != nil {
			return fail(err.Error())
		}
		if err := s.guard(ctx, resolved); err != nil {
			return fail(err.Error())
		}
		d.Target = resolved
	} else {
		d.Target = displayTarget(kind, remote)
		if err := s.checkHosts(ctx, kind, remote); err != nil {
			return fail(err.Error())
		}
		if kind == engines.SFTP && len(remote.SFTP.HostKeys) == 0 {
			keys, err := s.scanHostKeys(ctx, remote.SFTP.Host, remote.SFTP.Port)
			if err != nil {
				return fail(fmt.Sprintf("could not read the server's host keys: %v", err))
			}
			return engines.TestResult{Reachable: true, HostKeys: keys, Warnings: warnings,
				Message: "confirm the server's host key fingerprints, pin them in remote.hostKeys and test again"}, nil
		}
		if kind == engines.B2 {
			w, err := s.authorizeB2(ctx, remote.B2.Bucket, creds)
			if err != nil {
				return fail(err.Error())
			}
			warnings = append(warnings, w...)
		}
	}
	res, err := eng.Test(ctx, d, sec)
	if err != nil {
		return engines.TestResult{}, redactErr(err, values)
	}
	res.Message = logging.RedactValues(res.Message, values...)
	res.Warnings = append(redactAll(warnings, values), redactAll(res.Warnings, values)...)
	return res, nil
}

// TestStored tests a stored engine destination with its stored location and secrets only
// (SecretsFor): POST /destinations/{id}/test, which accepts no connection fields (§4.5).
func (s *Store) TestStored(ctx context.Context, id int64) (engines.TestResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.createBudget)
	defer cancel()
	d, sec, err := s.SecretsFor(ctx, id)
	if err != nil {
		return engines.TestResult{}, err
	}
	eng, err := s.engine(string(d.Engine))
	if err != nil {
		return engines.TestResult{}, err
	}
	if d.Kind.Remote() {
		if err := s.checkHosts(ctx, d.Kind, d.Remote); err != nil {
			return engines.TestResult{Message: err.Error()}, nil
		}
	}
	res, err := eng.Test(ctx, d, sec)
	if err != nil {
		return engines.TestResult{}, redactErr(err, sec.Values())
	}
	res.Message = logging.RedactValues(res.Message, sec.Values()...)
	res.Warnings = redactAll(res.Warnings, sec.Values())
	if strings.HasPrefix(d.MarkerID, pendingPrefix) {
		res.OK = false
		res.Warnings = append(res.Warnings, "the create of this destination did not finish: create it again to finish it")
	} else if res.OK && !identityMatches(d, res) {
		res.OK = false
		res.Message = "another repository or destination at this location (the identity check failed)"
	}
	return res, nil
}

// identityMatches reports whether a test result names the destination's repository or marker
// (S25): "restic:<id>" for restic, the marker's id for rclone.
func identityMatches(d engines.Destination, res engines.TestResult) bool {
	if res.ID == "" {
		return false
	}
	if d.Engine == engines.Restic {
		return d.MarkerID == "restic:"+res.ID || d.MarkerID == res.ID
	}
	return d.MarkerID == res.ID
}

// updatedRemote decodes an update's remote (empty: unchanged) and checks that only the fields
// that may change do: an sftp remote's hostKeys and an s3 remote's caCert (§4.2, §12).
func (s *Store) updatedRemote(ctx context.Context, cur Destination, raw json.RawMessage) (engines.Remote, bool, []string, error) {
	if t := bytes.TrimSpace(raw); len(t) == 0 || string(t) == "null" {
		return cur.Remote, false, nil, nil
	}
	n, warnings, err := decodeRemote(raw, cur.Kind, remoteCheck{ctx: ctx, lookup: s.opts.LookupHost, requireHostKeys: true})
	if err != nil {
		return engines.Remote{}, false, nil, err
	}
	loc := func(r engines.Remote) engines.Remote {
		switch {
		case r.SFTP != nil:
			v := *r.SFTP
			v.HostKeys = nil
			return engines.Remote{SFTP: &v}
		case r.S3 != nil:
			v := *r.S3
			v.CACert = ""
			return engines.Remote{S3: &v}
		}
		return r
	}
	if !reflect.DeepEqual(loc(n), loc(cur.Remote)) {
		return engines.Remote{}, false, nil, ValidationError("the location of a destination cannot be changed (only remote.hostKeys and remote.caCert may); create a new destination instead")
	}
	return n, !reflect.DeepEqual(n, cur.Remote), warnings, nil
}

// testUpdate tests an update's connection changes (new credentials merged over the stored ones,
// new host keys or CA certificate) against the stored location before they are saved: the S25
// identity check must pass with them (§4.3).
func (s *Store) testUpdate(ctx context.Context, cur Destination, remote engines.Remote, update engines.Credentials) ([]string, error) {
	if cur.Pending {
		return nil, ValidationError("the create of this destination did not finish: create it again with the same location to finish it, or delete it")
	}
	ctx, cancel := context.WithTimeout(ctx, s.createBudget)
	defer cancel()
	d, stored, err := s.SecretsFor(ctx, cur.ID)
	if err != nil {
		return nil, err
	}
	merged := mergeCredentials(cur.Kind, stored.Credentials, update)
	if err := checkCompleteCredentials(cur.Kind, merged); err != nil {
		return nil, err
	}
	eng, err := s.engine(cur.Engine)
	if err != nil {
		return nil, err
	}
	d.Remote = remote
	if err := s.checkHosts(ctx, d.Kind, d.Remote); err != nil {
		return nil, err
	}
	var warnings []string
	if d.Kind == engines.B2 && !update.Empty() {
		if warnings, err = s.authorizeB2(ctx, d.Remote.B2.Bucket, merged); err != nil {
			return nil, err
		}
	}
	sec := s.secretsOf(cur.ID, merged, stored.Encryption)
	values := sec.Values()
	res, err := eng.Test(ctx, d, sec)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ValidationError("the new connection settings were not saved: the test failed"), redactErr(err, values))
	}
	if !res.OK || !identityMatches(d, res) {
		msg := logging.RedactValues(res.Message, values...)
		if msg == "" {
			msg = "the destination's repository or marker was not found with them"
		}
		return nil, ValidationError("the new connection settings were not saved: " + msg)
	}
	return append(warnings, redactAll(res.Warnings, values)...), nil
}
