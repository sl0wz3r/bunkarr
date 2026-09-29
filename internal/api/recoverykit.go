package api

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/destinations"
)

// The recovery kit (docs/design/phase4.md §5.2, S21): the only response that carries a
// destination's encryption secret, and the confirmation of its custody that lets the
// destination's jobs run. Both need a UI session (the API key and the local bypass get 403); the
// export also needs the user's password, checked like a login and counted by the login limiter,
// and confirmations are rate limited like logins. Every export sends a warning notification and
// writes a process-log line without any of the kit's content.

// Texts of the kit endpoints.
const (
	msgKitSession     = "log in and confirm your password to export the recovery kit"
	msgConfirmSession = "log in to confirm the recovery kit"
)

// kitFallbackName is the attachment name when the kit's own name cannot be formatted.
const kitFallbackName = "bunkarr-recovery-kit.txt"

// exportRecoveryKit is POST /destinations/{id}/recovery-kit {currentPassword,
// includeStorageCredentials} → 200 text/plain attachment.
func (s *Server) exportRecoveryKit(w http.ResponseWriter, r *http.Request) {
	const action = "export a recovery kit"
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	var body struct {
		CurrentPassword           string `json:"currentPassword"`
		IncludeStorageCredentials bool   `json:"includeStorageCredentials"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if _, ok := s.sessionOnly(w, r, msgKitSession); !ok {
		s.log.Warn("Recovery kit export refused without a login session", "destinationId", id, "remote", auth.ClientIP(r).String())
		return
	}
	ctx := r.Context()
	d, err := s.app.Destinations.Get(ctx, id)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	if !d.HasSecret() {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "destination %q has no encryption secret, so it has no recovery kit", d.Name))
		return
	}
	if !s.freshPassword(w, r, body.CurrentPassword, msgKitSession, action) {
		return
	}
	kit, err := s.app.Destinations.RecoveryKit(ctx, id, body.IncludeStorageCredentials)
	if errors.Is(err, destinations.ErrNotEngine) {
		err = errorf(http.StatusBadRequest, "destination %q has no recovery kit", d.Name)
	}
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	client := auth.ClientIP(r).String()
	at := time.Now().In(s.app.loc).Format("2006-01-02 15:04 MST")
	// Content-free: the kit itself is never logged (S22). The App's log is the process log.
	s.app.log.Warn("Recovery kit exported", "destinationId", d.ID, "destination", d.Name, "remote", client,
		"includeStorageCredentials", body.IncludeStorageCredentials)
	s.app.Notifier.Warn(fmt.Sprintf("Recovery kit for %s exported", d.Name), fmt.Sprintf("Recovery kit for %s exported at %s from %s. "+
		"Whoever holds the kit (and access to the storage) can read this backup; if this was not you, change your Bunkarr password.",
		d.Name, at, client))

	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": kit.Filename})
	if disposition == "" {
		disposition = mime.FormatMediaType("attachment", map[string]string{"filename": kitFallbackName})
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Content-Length", strconv.Itoa(len(kit.Content)))
	h.Set("Content-Disposition", disposition)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, kit.Content)
}

// confirmRecoveryKit is POST /destinations/{id}/recovery-kit/confirm {checkCode} or {secret} →
// 204: the kit's check code (case, spaces and dashes ignored), or, for an encryption secret the
// user typed at create, that secret typed again. A wrong one is 400 and counted by the login
// limiter.
func (s *Server) confirmRecoveryKit(w http.ResponseWriter, r *http.Request) {
	const action = "confirm a recovery kit"
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	var body struct {
		CheckCode string `json:"checkCode"`
		Secret    string `json:"secret"`
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, action, err)
		return
	}
	if _, ok := s.sessionOnly(w, r, msgConfirmSession); !ok {
		s.log.Warn("Recovery kit confirmation refused without a login session", "destinationId", id, "remote", auth.ClientIP(r).String())
		return
	}
	// Counted before the comparison, so guesses sent together cannot all be compared before the
	// first is counted; only a wrong answer keeps the count (a right one leaves it as it was).
	undo, ok := s.attempt(w, r)
	if !ok {
		return
	}
	wrong := false
	defer func() {
		if !wrong {
			undo()
		}
	}()
	hasCode, hasSecret := strings.TrimSpace(body.CheckCode) != "", body.Secret != ""
	if hasCode == hasSecret {
		s.fail(w, r, action, errorf(http.StatusBadRequest, "send either the kit's check code (checkCode) or your encryption password (secret)"))
		return
	}
	ctx := r.Context()
	d, err := s.app.Destinations.Get(ctx, id)
	if err != nil {
		s.fail(w, r, action, err)
		return
	}
	client := auth.ClientIP(r).String()
	err = s.app.Destinations.ConfirmKit(ctx, id, destinations.KitConfirmation{CheckCode: body.CheckCode, Secret: body.Secret})
	switch {
	case errors.Is(err, destinations.ErrWrongCheckCode), errors.Is(err, destinations.ErrWrongSecret):
		wrong = true
		s.app.log.Warn("Wrong recovery kit confirmation", "destinationId", d.ID, "destination", d.Name, "remote", client)
		msg := "wrong check code"
		if errors.Is(err, destinations.ErrWrongSecret) {
			msg = "the password does not match the destination's encryption password"
		}
		writeError(w, http.StatusBadRequest, msg)
		return
	case errors.Is(err, destinations.ErrNotEngine):
		s.fail(w, r, action, errorf(http.StatusBadRequest, "destination %q has no recovery kit", d.Name))
		return
	case err != nil:
		s.fail(w, r, action, err)
		return
	}
	s.app.log.Info("Recovery kit custody confirmed", "destinationId", d.ID, "destination", d.Name, "remote", client, "by", confirmedBy(hasCode))
	w.WriteHeader(http.StatusNoContent)
}

// confirmedBy names how a kit was confirmed in the process log.
func confirmedBy(code bool) string {
	if code {
		return "check code"
	}
	return "encryption password"
}
