package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strconv"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
)

// Off-site targets need a fresh password (docs/design/phase4.md S29, D33). Phase 4 is the first
// time Bunkarr can send data to an arbitrary internet host, so the credentials that can run jobs
// (the API key, and the local-address bypass of disabled_for_local_addresses) cannot choose where
// data goes or relax the encryption and mode checks: those requests need a UI session plus the
// user's password in the body (currentPassword), checked like a login and counted by the login
// limiter. The routes and the conditions are exactly S29's:
//   - POST /destinations with a kind other than local, or with sources linking a source to a
//     destination whose kind is not local, or with encryption.acceptUnencrypted;
//   - PUT /destinations/{id} that changes remote.hostKeys, remote.caCert or credentials, or links a
//     source to a destination whose kind is not local;
//   - POST and PUT /integrations whose backup targets add a destination whose kind is not local, or
//     set acceptInsecureModes on a target;
//   - any body with acceptUnencrypted or acceptInsecureModes true.
// Running existing jobs (sync, verify, retention, backups) stays allowed with the API key.

// Texts of the fresh-password check.
const (
	// msgOffsiteSession is the 403 of S29 for the API key and the local bypass.
	msgOffsiteSession = "log in and confirm your password to send data off-site"
	// msgWrongPassword is the 400 of a wrong currentPassword (counted by the login limiter).
	msgWrongPassword = "current password is incorrect"
	// msgTooManyFailures is the 429 of a client the login limiter blocks, as login answers it.
	msgTooManyFailures = "too many failed logins; try again later"
)

// sessionOnly answers 403 with forbidden unless the request comes from a UI session with a user
// (not the API key, not the local bypass). It reports whether the request may go on.
func (s *Server) sessionOnly(w http.ResponseWriter, r *http.Request, forbidden string) (auth.Principal, bool) {
	p, _ := auth.PrincipalFrom(r.Context())
	if p.Kind != auth.KindSession || p.User == nil {
		writeError(w, http.StatusForbidden, forbidden)
		return p, false
	}
	return p, true
}

// limited answers a client the login limiter blocks as login does (429 with Retry-After) and
// reports whether it did.
func (s *Server) limited(w http.ResponseWriter, r *http.Request) bool {
	if blocked, wait := s.auth.Limiter.Blocked(auth.ClientIP(r).String()); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, msgTooManyFailures)
		return true
	}
	return false
}

// freshPassword is S29's check (and the recovery kit's, §5.2): a UI session with a user (else 403
// with forbidden), a client the login limiter does not block (else 429), and the user's password
// (else 400 "current password is incorrect", counted by the limiter like a failed login). It
// writes the refusal and reports whether the request may go on; action names the request in the
// process log.
func (s *Server) freshPassword(w http.ResponseWriter, r *http.Request, password, forbidden, action string) bool {
	p, ok := s.sessionOnly(w, r, forbidden)
	if !ok {
		s.log.Warn("Refused without a login session and password", "action", action, "via", p.Kind, "remote", auth.ClientIP(r).String())
		return false
	}
	if s.limited(w, r) {
		return false
	}
	client := auth.ClientIP(r).String()
	err := s.auth.VerifyPassword(r.Context(), p.User.ID, password)
	if errors.Is(err, auth.ErrInvalidCredentials) {
		s.auth.Limiter.Fail(client)
		s.log.Warn("Wrong password for a protected change", "action", action, "remote", client)
		writeError(w, http.StatusBadRequest, msgWrongPassword)
		return false
	}
	if err != nil {
		s.fail(w, r, action, err)
		return false
	}
	s.auth.Limiter.Success(client)
	return true
}

// offsite applies S29 when need is set: it is freshPassword with S29's 403 text. It reports
// whether the request may go on (always, when need is false).
func (s *Server) offsite(w http.ResponseWriter, r *http.Request, need bool, password, action string) bool {
	if !need {
		return true
	}
	return s.freshPassword(w, r, password, msgOffsiteSession, action)
}

// createNeedsFreshPassword reports whether POST /destinations needs S29: a kind other than local
// (whatever the engine: every remote kind sends data off the machine, and its sources are linked
// by the same request), or acceptUnencrypted.
func createNeedsFreshPassword(in destinations.Input) bool {
	if in.Kind != "" && in.Kind != engines.Local {
		return true
	}
	return in.Encryption != nil && in.Encryption.AcceptUnencrypted
}

// updateNeedsFreshPassword reports whether PUT /destinations/{id} of cur needs S29: it sends
// credentials, changes remote.hostKeys or remote.caCert, links a source that is not linked yet
// to a destination whose kind is not local, or sets acceptUnencrypted.
func updateNeedsFreshPassword(cur destinations.Destination, in destinations.Input) bool {
	if in.Credentials != nil || (in.Encryption != nil && in.Encryption.AcceptUnencrypted) {
		return true
	}
	if remoteChangesTrust(cur, in.Remote) {
		return true
	}
	if cur.Kind != "" && cur.Kind != engines.Local {
		for _, id := range in.SourceIDs {
			if !slices.Contains(cur.SourceIDs, id) {
				return true
			}
		}
	}
	return false
}

// remoteChangesTrust reports whether an update's remote changes what the engines trust: the
// pinned SFTP host keys or the S3 CA certificate. The store replaces the whole remote with the one
// sent, so a field the remote leaves out or sets to null is empty, as the store decodes it: an S3
// remote without caCert removes a pinned CA (the endpoint is then checked against the system
// roots), which is a change of trust. A remote that does not decode counts as a change (the
// store refuses it anyway).
func remoteChangesTrust(cur destinations.Destination, raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return false
	}
	var v struct {
		HostKeys []engines.HostKey `json:"hostKeys"`
		CACert   string            `json:"caCert"`
	}
	if err := json.Unmarshal(t, &v); err != nil {
		return true
	}
	var storedKeys []engines.HostKey
	if cur.Remote.SFTP != nil {
		storedKeys = cur.Remote.SFTP.HostKeys
	}
	if len(v.HostKeys) != len(storedKeys) || (len(storedKeys) > 0 && !reflect.DeepEqual(v.HostKeys, storedKeys)) {
		return true
	}
	storedCA := ""
	if cur.Remote.S3 != nil {
		storedCA = cur.Remote.S3.CACert
	}
	return v.CACert != storedCA
}

// backupTargetsOf returns the effective backup targets of an integration's settings of type typ
// (none for other types and settings that do not parse; the store reports those).
func backupTargetsOf(typ integrations.Type, raw json.RawMessage) []integrations.BackupTarget {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return nil
	}
	switch {
	case typ == integrations.TypePlex:
		if ps, err := integrations.ParsePlexSettings(raw); err == nil {
			return ps.Backup.EffectiveTargets()
		}
	case typ.IsArr():
		if as, err := integrations.ParseArrSettings(raw); err == nil {
			return as.Backup.EffectiveTargets()
		}
	}
	return nil
}

// integrationNeedsFreshPassword reports whether saving an integration of type typ with settings
// raw needs S29 (§8.5): a backup target that is not among cur's targets (nil cur: a create, every
// target is added) on a destination whose kind is not local, or acceptInsecureModes on a target
// that did not have it. A destination that cannot be read does not need it here: the store
// refuses the settings.
func (s *Server) integrationNeedsFreshPassword(r *http.Request, typ integrations.Type, raw json.RawMessage, cur *integrations.Integration) bool {
	stored := map[int64]integrations.BackupTarget{}
	if cur != nil {
		for _, t := range backupTargetsOf(cur.Type, cur.Settings) {
			stored[t.DestinationID] = t
		}
	}
	for _, t := range backupTargetsOf(typ, raw) {
		old, had := stored[t.DestinationID]
		if t.AcceptInsecureModes && !(had && old.AcceptInsecureModes) {
			return true
		}
		if had || t.DestinationID <= 0 {
			continue
		}
		if d, err := s.app.Destinations.Get(r.Context(), t.DestinationID); err == nil && d.Kind != "" && d.Kind != engines.Local {
			return true
		}
	}
	return false
}
