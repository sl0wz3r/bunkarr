package api

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/integrations"
	"github.com/sl0wz3r/bunkarr/internal/integrations/plex"
	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/testhooks"
)

// "Sign in with Plex" (design §5, D10, S19). The browser starts a sign-in (a plex.tv PIN), opens
// the returned authUrl on app.plex.tv, and polls until the PIN is approved. The account token and
// every server's access token then stay here, in a short-lived in-memory sign-in; the browser only
// ever holds the opaque sign-in id and a server id. Creating, updating or testing a Plex
// integration with plexSignIn {id, serverId, useAccountToken?} picks the server's token on the
// server side, checks that the URL answers /identity as that server (without the token), and
// then stores it bound to that URL like a pasted token.

// SettingPlexClientIdentifier is the setting that holds this install's X-Plex-Client-Identifier (a
// UUIDv4 created once), so plex.tv lists each install as its own device.
const SettingPlexClientIdentifier = "plex.clientIdentifier"

// Limits of the sign-in registry.
const (
	// signInMaxLive is how many sign-ins may be live at once (429 beyond).
	signInMaxLive = 8
	// signInPendingTTL caps a pending sign-in's life (the PIN's own expiry may be shorter).
	signInPendingTTL = 10 * time.Minute
	// signInClaimedTTL is how long an approved sign-in stays usable.
	signInClaimedTTL = 20 * time.Minute
	// signInCheckEvery is the shortest interval between two PIN checks at plex.tv.
	signInCheckEvery = 2 * time.Second
	// signInResourcesEvery is the shortest interval between two server list fetches.
	signInResourcesEvery = 10 * time.Second
	// signInForgetAfter is how long an expired sign-in still answers "expired" (with no secret
	// left in it) before it is forgotten and answers 404.
	signInForgetAfter = 10 * time.Minute
	// plexTVTimeout bounds each plex.tv call of a request.
	plexTVTimeout = 15 * time.Second
)

var (
	signInIDPattern     = regexp.MustCompile(`^[0-9a-f]{32}$`)
	plexServerIDPattern = regexp.MustCompile(`^[0-9A-Za-z-]{1,128}$`)
)

// Sign-in statuses (GET /plex/signin/{id}).
const (
	signInPending       = "pending"
	signInAuthenticated = "authenticated"
	signInExpired       = "expired"
)

// plexSignIn is one sign-in. Its fields are guarded by the registry's mutex; op serializes the
// plex.tv calls of one sign-in (a PIN check, a server list fetch) so two polls never claim twice.
type plexSignIn struct {
	id        string
	principal string
	pinID     int64
	pinCode   string
	expires   time.Time

	claimed      bool
	accountToken string
	username     string
	resources    []plex.Resource
	resourcesAt  time.Time
	warning      string
	lastCheck    time.Time

	// expired: the tokens are gone; forgetAt: when the entry is removed.
	expired  bool
	forgetAt time.Time
	// gone: removed from the registry (consumed, cancelled, forgotten or cleared).
	gone bool
	// inUse: a create or update is saving with this sign-in.
	inUse bool

	op sync.Mutex
}

// signInRegistry holds the live sign-ins in memory. Sign-ins do not survive a restart.
type signInRegistry struct {
	mu       sync.Mutex
	m        map[string]*plexSignIn
	reserved int
	now      func() time.Time
	log      *slog.Logger
}

func newSignInRegistry(log *slog.Logger) *signInRegistry {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &signInRegistry{m: map[string]*plexSignIn{}, now: time.Now, log: log}
}

// secretOwner names a sign-in's values in the redaction registry.
func (si *plexSignIn) secretOwner() string { return "plexsignin:" + si.id }

// short is the id prefix logs carry.
func (si *plexSignIn) short() string { return si.id[:8] }

// registerSecretsLocked holds the sign-in's id, PIN code and tokens in the redaction registry, so
// logs (the request log names the id in paths) carry only the id's prefix.
func (si *plexSignIn) registerSecretsLocked() {
	vals := []string{si.id, si.pinCode, si.accountToken}
	for _, r := range si.resources {
		vals = append(vals, r.AccessToken)
	}
	logging.SetSecrets(si.secretOwner(), vals...)
}

// dropSecretsLocked forgets every secret of si and releases them from the redaction registry.
func (si *plexSignIn) dropSecretsLocked() {
	si.accountToken, si.pinCode, si.resources = "", "", nil
	logging.SetSecrets(si.secretOwner())
}

// sweepLocked expires sign-ins past their expiry and forgets expired ones past forgetAt.
func (g *signInRegistry) sweepLocked(now time.Time) {
	for id, si := range g.m {
		switch {
		case !si.expired && !now.Before(si.expires):
			g.expireLocked(si, now)
		case si.expired && !now.Before(si.forgetAt):
			si.gone = true
			delete(g.m, id)
		}
	}
}

func (g *signInRegistry) expireLocked(si *plexSignIn, now time.Time) {
	if si.expired {
		return
	}
	si.expired, si.forgetAt = true, now.Add(signInForgetAfter)
	si.dropSecretsLocked()
	g.log.Info("Plex sign-in expired", "signIn", si.short())
}

// liveLocked counts the sign-ins that are not expired.
func (g *signInRegistry) liveLocked() int {
	n := 0
	for _, si := range g.m {
		if !si.expired {
			n++
		}
	}
	return n
}

// reserve takes a slot for a sign-in being created; false when signInMaxLive are live.
func (g *signInRegistry) reserve() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(g.now())
	if g.liveLocked()+g.reserved >= signInMaxLive {
		return false
	}
	g.reserved++
	return true
}

// unreserve gives back a slot whose sign-in was not created.
func (g *signInRegistry) unreserve() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserved--
}

// add turns a reservation into a sign-in for pin.
func (g *signInRegistry) add(principal string, pin plex.Pin) (*plexSignIn, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		g.unreserve()
		return nil, fmt.Errorf("create a sign-in id: %w", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reserved--
	now := g.now()
	expires := now.Add(signInPendingTTL)
	if !pin.ExpiresAt.IsZero() && pin.ExpiresAt.Before(expires) {
		expires = pin.ExpiresAt
	}
	si := &plexSignIn{id: hex.EncodeToString(b[:]), principal: principal, pinID: pin.ID, pinCode: pin.Code, expires: expires}
	si.registerSecretsLocked()
	g.m[si.id] = si
	return si, nil
}

// get returns the sign-in id of principal, or nil (unknown, forgotten, or another principal's).
func (g *signInRegistry) get(id, principal string) *plexSignIn {
	if !signInIDPattern.MatchString(id) {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweepLocked(g.now())
	si := g.m[id]
	if si == nil || si.principal != principal {
		return nil
	}
	return si
}

// removeLocked forgets si at once and releases its secrets.
func (g *signInRegistry) removeLocked(si *plexSignIn) {
	si.gone = true
	si.dropSecretsLocked()
	delete(g.m, si.id)
}

// remove forgets si (consumed or cancelled).
func (g *signInRegistry) remove(si *plexSignIn) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.removeLocked(si)
}

// clear forgets every sign-in (App.Stop).
func (g *signInRegistry) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, si := range g.m {
		g.removeLocked(si)
	}
}

// signInStatus is GET /plex/signin/{id}.
type signInStatus struct {
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expiresAt"`
	Username  string    `json:"username,omitempty"`
	Warning   string    `json:"warning,omitempty"`
}

func (g *signInRegistry) statusLocked(si *plexSignIn) signInStatus {
	st := signInStatus{Status: signInPending, ExpiresAt: si.expires.UTC(), Warning: si.warning}
	switch {
	case si.expired:
		st = signInStatus{Status: signInExpired, ExpiresAt: si.expires.UTC()}
	case si.claimed:
		st.Status, st.Username = signInAuthenticated, si.username
	}
	return st
}

// signInPrincipal names who made r: a hash of the session token, "apikey" or "local". Another
// principal cannot see or use a sign-in.
func signInPrincipal(r *http.Request) string {
	p, _ := auth.PrincipalFrom(r.Context())
	switch p.Kind {
	case auth.KindSession:
		sum := sha256.Sum256([]byte(p.Token))
		return "session:" + hex.EncodeToString(sum[:])
	case auth.KindAPIKey, auth.KindLocal:
		return p.Kind
	}
	return "anonymous"
}

func (s *Server) plexSignInRoutes(r chi.Router) {
	r.Post("/plex/signin", s.createPlexSignIn)
	r.Get("/plex/signin/{id}", s.getPlexSignIn)
	r.Delete("/plex/signin/{id}", s.deletePlexSignIn)
	r.Get("/plex/signin/{id}/servers", s.plexSignInServers)
	r.Post("/plex/signin/{id}/servers/{serverId}/test", s.testPlexSignInServer)
}

// plexSignInCreated is POST /plex/signin's answer. The PIN code appears only inside AuthURL; the
// PIN id never leaves the server.
type plexSignInCreated struct {
	ID        string    `json:"id"`
	AuthURL   string    `json:"authUrl"`
	ExpiresAt time.Time `json:"expiresAt"`
}

func (s *Server) createPlexSignIn(w http.ResponseWriter, r *http.Request) {
	if err := decodeOptionalBody(w, r, &struct{}{}); err != nil {
		s.fail(w, r, "start plex sign-in", err)
		return
	}
	reg := s.app.signIns
	if !reg.reserve() {
		s.fail(w, r, "start plex sign-in", errorf(http.StatusTooManyRequests,
			"%d Plex sign-ins are already in progress; finish or cancel one, or wait until they expire", signInMaxLive))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), plexTVTimeout)
	defer cancel()
	pin, err := plex.CreatePin(ctx, s.app.plexOpts)
	if err != nil {
		reg.unreserve()
		s.log.Warn("Could not start a Plex sign-in", "error", err)
		s.fail(w, r, "start plex sign-in", errorf(http.StatusBadGateway, "plex.tv did not create a sign-in PIN: %v", plexTVCause(err)))
		return
	}
	si, err := reg.add(signInPrincipal(r), pin)
	if err != nil {
		s.fail(w, r, "start plex sign-in", err)
		return
	}
	s.log.Info("Plex sign-in started", "signIn", si.short())
	writeJSON(w, http.StatusCreated, plexSignInCreated{ID: si.id, AuthURL: plex.AuthURL(s.app.plexOpts, pin.Code), ExpiresAt: si.expires.UTC()})
}

// plexTVCause is the cause of a plex.tv helper error, without the "plex.tv <method> <path>" prefix.
func plexTVCause(err error) error {
	var perr *plex.Error
	if errors.As(err, &perr) && perr.Err != nil {
		return perr.Err
	}
	return err
}

// signIn loads the {id} of the request's principal (404 otherwise).
func (s *Server) signIn(r *http.Request) (*plexSignIn, error) {
	si := s.app.signIns.get(chi.URLParam(r, "id"), signInPrincipal(r))
	if si == nil {
		return nil, errorf(http.StatusNotFound, "no such Plex sign-in (it may have expired, or Bunkarr restarted); sign in with Plex again")
	}
	return si, nil
}

func (s *Server) getPlexSignIn(w http.ResponseWriter, r *http.Request) {
	si, err := s.signIn(r)
	if err != nil {
		s.fail(w, r, "check plex sign-in", err)
		return
	}
	st := s.checkPlexSignIn(r.Context(), si)
	writeJSON(w, http.StatusOK, st)
}

// checkPlexSignIn asks plex.tv whether the PIN was approved (at most every signInCheckEvery) and,
// when it was, reads the account name and its servers.
func (s *Server) checkPlexSignIn(ctx context.Context, si *plexSignIn) signInStatus {
	reg := s.app.signIns
	si.op.Lock()
	defer si.op.Unlock()

	reg.mu.Lock()
	now := reg.now()
	reg.sweepLocked(now)
	if si.gone || si.expired || si.claimed || now.Sub(si.lastCheck) < signInCheckEvery {
		st := reg.statusLocked(si)
		reg.mu.Unlock()
		return st
	}
	si.lastCheck = now
	pinID, code := si.pinID, si.pinCode
	reg.mu.Unlock()

	cctx, cancel := context.WithTimeout(ctx, plexTVTimeout)
	defer cancel()
	pin, err := plex.CheckPin(cctx, s.app.plexOpts, pinID, code)
	var (
		account plex.Account
		res     []plex.Resource
		userErr error
		resErr  error
	)
	if err == nil && pin.AuthToken != "" {
		account, userErr = plex.User(cctx, s.app.plexOpts, pin.AuthToken)
		res, resErr = plex.Resources(cctx, s.app.plexOpts, pin.AuthToken)
	}

	reg.mu.Lock()
	defer reg.mu.Unlock()
	switch {
	case si.gone || si.expired:
	case errors.Is(err, plex.ErrNotFound):
		reg.expireLocked(si, reg.now())
	case err != nil:
		si.warning = "plex.tv did not answer the last check; still waiting."
		s.log.Warn("Could not check a Plex sign-in", "signIn", si.short(), "error", err)
	case pin.AuthToken != "":
		si.claimed, si.accountToken, si.warning = true, pin.AuthToken, ""
		si.expires = reg.now().Add(signInClaimedTTL)
		si.username = account.Username
		if userErr != nil {
			si.warning = "Signed in, but plex.tv did not say which account; check the servers listed."
		}
		if resErr == nil {
			si.resources, si.resourcesAt = res, reg.now()
		}
		si.registerSecretsLocked()
		s.log.Info("Plex sign-in approved", "signIn", si.short(), "username", si.username, "servers", len(res))
	default:
		si.warning = ""
	}
	return reg.statusLocked(si)
}

func (s *Server) deletePlexSignIn(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !signInIDPattern.MatchString(id) {
		s.fail(w, r, "cancel plex sign-in", errorf(http.StatusNotFound, "no such Plex sign-in"))
		return
	}
	if si := s.app.signIns.get(id, signInPrincipal(r)); si != nil {
		s.app.signIns.remove(si)
		s.log.Info("Plex sign-in cancelled", "signIn", si.short())
	}
	w.WriteHeader(http.StatusNoContent)
}

// plexServerChoice is one server of GET /plex/signin/{id}/servers. It carries no token:
// hasAccessToken says whether plex.tv listed one.
type plexServerChoice struct {
	ID             string                 `json:"id"`
	Name           string                 `json:"name"`
	Owned          bool                   `json:"owned"`
	ProductVersion string                 `json:"productVersion"`
	Platform       string                 `json:"platform"`
	HasAccessToken bool                   `json:"hasAccessToken"`
	Connections    []plexConnectionChoice `json:"connections"`
}

// plexConnectionChoice is one address plex.tv lists for a server.
type plexConnectionChoice struct {
	URI      string `json:"uri"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Local    bool   `json:"local"`
	Relay    bool   `json:"relay"`
	IPv6     bool   `json:"ipv6"`
}

// claimedResources returns the servers of a claimed sign-in, fetched again from plex.tv when the
// list is older than signInResourcesEvery (or was never read). A failed refetch keeps the list it
// had; with none, it fails with 502.
func (s *Server) claimedResources(ctx context.Context, si *plexSignIn) ([]plex.Resource, error) {
	reg := s.app.signIns
	si.op.Lock()
	defer si.op.Unlock()

	reg.mu.Lock()
	reg.sweepLocked(reg.now())
	switch {
	case si.gone || si.expired:
		reg.mu.Unlock()
		return nil, errorf(http.StatusConflict, "the Plex sign-in expired; sign in with Plex again")
	case !si.claimed:
		reg.mu.Unlock()
		return nil, errorf(http.StatusConflict, "the Plex sign-in is not complete yet: approve it in the Plex window first")
	}
	cached, fresh := si.resources, !si.resourcesAt.IsZero() && reg.now().Sub(si.resourcesAt) < signInResourcesEvery
	token := si.accountToken
	reg.mu.Unlock()
	if fresh {
		return cached, nil
	}

	cctx, cancel := context.WithTimeout(ctx, plexTVTimeout)
	defer cancel()
	res, err := plex.Resources(cctx, s.app.plexOpts, token)

	reg.mu.Lock()
	defer reg.mu.Unlock()
	if si.gone || si.expired {
		return nil, errorf(http.StatusConflict, "the Plex sign-in expired; sign in with Plex again")
	}
	if err != nil {
		s.log.Warn("Could not list the servers of a Plex sign-in", "signIn", si.short(), "error", err)
		if si.resourcesAt.IsZero() {
			return nil, errorf(http.StatusBadGateway, "plex.tv did not list the account's servers: %v", plexTVCause(err))
		}
		return si.resources, nil
	}
	si.resources, si.resourcesAt = res, reg.now()
	si.registerSecretsLocked()
	return res, nil
}

func (s *Server) plexSignInServers(w http.ResponseWriter, r *http.Request) {
	si, err := s.signIn(r)
	if err != nil {
		s.fail(w, r, "list plex sign-in servers", err)
		return
	}
	res, err := s.claimedResources(r.Context(), si)
	if err != nil {
		s.fail(w, r, "list plex sign-in servers", err)
		return
	}
	out := make([]plexServerChoice, 0, len(res))
	for _, rs := range res {
		if !plexServerIDPattern.MatchString(rs.ClientIdentifier) {
			continue
		}
		c := plexServerChoice{ID: rs.ClientIdentifier, Name: rs.Name, Owned: rs.Owned, ProductVersion: rs.ProductVersion,
			Platform: rs.Platform, HasAccessToken: rs.AccessToken != "", Connections: make([]plexConnectionChoice, 0, len(rs.Connections))}
		for _, cn := range rs.Connections {
			c.Connections = append(c.Connections, plexConnectionChoice{URI: cn.URI, Protocol: cn.Protocol, Address: cn.Address,
				Port: cn.Port, Local: cn.Local, Relay: cn.Relay, IPv6: cn.IPv6})
		}
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b plexServerChoice) int {
		if a.Owned != b.Owned {
			if a.Owned {
				return -1
			}
			return 1
		}
		return cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	writeJSON(w, http.StatusOK, out)
}

// plexProbeAnswer is POST /plex/signin/{id}/servers/{serverId}/test's answer.
type plexProbeAnswer struct {
	// Recommended is the URI to preselect: a working https, non-relay connection; null when none.
	Recommended *string            `json:"recommended"`
	Results     []plex.ProbeResult `json:"results"`
}

// findServer returns the server serverID of a sign-in's list.
func findServer(res []plex.Resource, serverID string) (plex.Resource, bool) {
	for _, r := range res {
		if r.ClientIdentifier == serverID {
			return r, true
		}
	}
	return plex.Resource{}, false
}

func (s *Server) testPlexSignInServer(w http.ResponseWriter, r *http.Request) {
	si, err := s.signIn(r)
	if err != nil {
		s.fail(w, r, "test plex server connections", err)
		return
	}
	var body struct {
		UseAccountToken bool `json:"useAccountToken"`
	}
	if err := decodeOptionalBody(w, r, &body); err != nil {
		s.fail(w, r, "test plex server connections", err)
		return
	}
	serverID := chi.URLParam(r, "serverId")
	if !plexServerIDPattern.MatchString(serverID) {
		s.fail(w, r, "test plex server connections", errorf(http.StatusNotFound, "no such server in this Plex sign-in"))
		return
	}
	res, err := s.claimedResources(r.Context(), si)
	if err != nil {
		s.fail(w, r, "test plex server connections", err)
		return
	}
	server, ok := findServer(res, serverID)
	if !ok {
		s.fail(w, r, "test plex server connections", errorf(http.StatusNotFound, "no such server in this Plex sign-in"))
		return
	}
	s.app.signIns.mu.Lock()
	account := si.accountToken
	s.app.signIns.mu.Unlock()
	// The token goes only where /identity answers as this server (ProbeConnections). Without a
	// server token, only an owned server may be probed with the account token, and only when asked.
	token := server.AccessToken
	if token == "" && server.Owned && body.UseAccountToken {
		token = account
	}
	results := plex.ProbeConnections(r.Context(), s.app.plexOpts, server.ClientIdentifier, token, plex.Candidates(server))
	ranked, rec := plex.RankResults(results)
	for i := range ranked {
		ranked[i].Message = logging.RedactValues(ranked[i].Message, token, account)
	}
	ans := plexProbeAnswer{Results: ranked}
	if rec != "" {
		ans.Recommended = &rec
	}
	writeJSON(w, http.StatusOK, ans)
}

// plexSignInRef is the plexSignIn field of POST/PUT /integrations and POST /integrations/test: the
// sign-in and the server whose token to use (in place of apiKey).
type plexSignInRef struct {
	ID       string `json:"id"`
	ServerID string `json:"serverId"`
	// UseAccountToken allows the plex.tv account token for an owned server that has no access
	// token of its own.
	UseAccountToken bool `json:"useAccountToken"`
}

// integrationBody is the body of POST/PUT /integrations: integrations.Input plus an optional
// plexSignIn (unknown fields are still refused).
type integrationBody struct {
	integrations.Input
	PlexSignIn *plexSignInRef `json:"plexSignIn"`
}

// signInUse is a sign-in reserved by a create or update. Call done when the request ends; call
// consume first when the integration was saved.
type signInUse struct {
	reg      *signInRegistry
	si       *plexSignIn
	consumed bool
}

// consume forgets the sign-in: its token now lives, sealed, in the integration.
func (u *signInUse) consume() {
	if u == nil {
		return
	}
	u.consumed = true
	u.reg.remove(u.si)
}

// done releases a sign-in that was not consumed, so it can be used again.
func (u *signInUse) done() {
	if u == nil || u.consumed {
		return
	}
	u.reg.mu.Lock()
	defer u.reg.mu.Unlock()
	u.si.inUse = false
}

// useSignIn resolves a plexSignIn reference of a create or update into in.APIKey (see
// resolveSignInToken) and reserves the sign-in; ref == nil does nothing.
func (s *Server) useSignIn(r *http.Request, ref *plexSignInRef, typ integrations.Type, in *integrations.Input) (*signInUse, error) {
	if ref == nil {
		return nil, nil
	}
	token, si, err := s.resolveSignInToken(r, ref, typ, in.URL, in.APIKey != "" || in.ClearAPIKey, true)
	if err != nil {
		return nil, err
	}
	in.APIKey = token
	return &signInUse{reg: s.app.signIns, si: si}, nil
}

// resolveSignInToken returns the token a sign-in gives for its server ref.ServerID at rawURL:
//  1. the integration must be Plex, and the request must not also send apiKey or clearApiKey;
//  2. the sign-in must exist (for this principal) and be approved;
//  3. the server must be one of its servers;
//  4. the token is the server's access token; without one, an owned server needs
//     useAccountToken (the account token), and a shared server needs a token entered by hand;
//  5. the URL must answer /identity (sent without the token) as that server.
//
// reserve marks the sign-in in use (a create or update), so a second concurrent use is refused.
func (s *Server) resolveSignInToken(r *http.Request, ref *plexSignInRef, typ integrations.Type, rawURL string, hasKey, reserve bool) (string, *plexSignIn, error) {
	switch {
	case typ != integrations.TypePlex:
		return "", nil, errorf(http.StatusBadRequest, "plexSignIn is only for Plex integrations")
	case hasKey:
		return "", nil, errorf(http.StatusBadRequest, "send either a token (apiKey or clearApiKey) or plexSignIn, not both")
	case !plexServerIDPattern.MatchString(ref.ServerID):
		return "", nil, errorf(http.StatusBadRequest, "plexSignIn.serverId is not a Plex server id")
	}
	signInAgain := errorf(http.StatusBadRequest, "the Plex sign-in has expired, was not completed or was already used; sign in with Plex again")
	reg := s.app.signIns
	si := reg.get(ref.ID, signInPrincipal(r))
	if si == nil {
		return "", nil, signInAgain
	}
	reg.mu.Lock()
	if !si.claimed || si.expired || si.gone || (reserve && si.inUse) {
		reg.mu.Unlock()
		return "", nil, signInAgain
	}
	server, ok := findServer(si.resources, ref.ServerID)
	account := si.accountToken
	reg.mu.Unlock()
	if !ok {
		return "", nil, errorf(http.StatusBadRequest, "the chosen server is not among this Plex account's servers; sign in with Plex again")
	}
	token := server.AccessToken
	switch {
	case token != "":
	case !server.Owned:
		return "", nil, errorf(http.StatusBadRequest,
			"plex.tv lists no access token for the shared server %q; enter a token for it manually (Bunkarr never sends your account token to a server you do not own)", server.Name)
	case !ref.UseAccountToken:
		return "", nil, errorf(http.StatusBadRequest,
			"plex.tv lists no access token for %q; allow your Plex account token for it (useAccountToken) or enter a token manually", server.Name)
	default:
		token = account
	}
	u, err := integrations.NormalizeURL(rawURL)
	if err != nil {
		return "", nil, err
	}
	// S19: the token goes only to a URL that answers /identity as the chosen server.
	c, err := plex.New(u, "", s.app.plexOpts)
	if err != nil {
		return "", nil, errorf(http.StatusBadRequest, "%v", err)
	}
	id, err := c.Identity(r.Context())
	if err != nil {
		return "", nil, errorf(http.StatusBadGateway, "could not check that the URL is %q (the token was not sent): %v", server.Name, err)
	}
	if !strings.EqualFold(strings.TrimSpace(id.MachineIdentifier), server.ClientIdentifier) {
		return "", nil, errorf(http.StatusBadRequest,
			"the server at this URL is not %q: it answered as another Plex server, so the token was not sent", server.Name)
	}
	if reserve {
		reg.mu.Lock()
		defer reg.mu.Unlock()
		if !si.claimed || si.expired || si.gone || si.inUse {
			return "", nil, signInAgain
		}
		si.inUse = true
	}
	return token, si, nil
}

// signInTestToken resolves the plexSignIn of POST /integrations/test into the token to test with
// (the sign-in is not reserved or consumed).
func (s *Server) signInTestToken(r *http.Request, ref *plexSignInRef, typ integrations.Type, rawURL, apiKey string) (string, error) {
	token, _, err := s.resolveSignInToken(r, ref, typ, rawURL, strings.TrimSpace(apiKey) != "", false)
	return token, err
}

// plexClientIdentifier returns this install's X-Plex-Client-Identifier, creating it (a UUIDv4)
// and storing it as SettingPlexClientIdentifier the first time. Without a settings store (some
// tests) it returns a new identifier that is not stored.
func plexClientIdentifier(ctx context.Context, settings *config.Settings) (string, error) {
	if settings != nil {
		v, ok, err := settings.Get(ctx, SettingPlexClientIdentifier)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", SettingPlexClientIdentifier, err)
		}
		if v = strings.TrimSpace(v); ok && v != "" {
			return v, nil
		}
	}
	id, err := newUUIDv4()
	if err != nil {
		return "", err
	}
	if settings != nil {
		if err := settings.Set(ctx, SettingPlexClientIdentifier, id); err != nil {
			return "", fmt.Errorf("store %s: %w", SettingPlexClientIdentifier, err)
		}
	}
	return id, nil
}

// newUUIDv4 returns a random (version 4) UUID.
func newUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("create a client identifier: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}

// PlexOptions returns the Plex options of the bunkarr binary (AppOptions.Plex): X-Plex-Device is
// "Docker" inside the official image, else the OS name, so plex.tv → Authorized Devices shows
// where Bunkarr runs.
func PlexOptions(docker bool) plex.Options {
	if docker {
		return plex.Options{Device: "Docker"}
	}
	return plex.Options{}
}

// appPlexOptions completes the Plex options of the app: the install's client identifier (unless
// the caller set one), and the plex.tv URLs, which only an e2e build lets the environment
// replace (testhooks).
func appPlexOptions(ctx context.Context, settings *config.Settings, o plex.Options) (plex.Options, error) {
	if strings.TrimSpace(o.ClientIdentifier) == "" {
		id, err := plexClientIdentifier(ctx, settings)
		if err != nil {
			return plex.Options{}, err
		}
		o.ClientIdentifier = id
	}
	if o.PlexTVURL == "" {
		o.PlexTVURL = testhooks.PlexTVURL(plex.PlexTVURL)
	}
	if o.ClientsPlexTVURL == "" {
		o.ClientsPlexTVURL = testhooks.ClientsPlexTVURL(plex.ClientsPlexTVURL)
	}
	return o, nil
}
