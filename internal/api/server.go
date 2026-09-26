// Package api is Bunkarr's HTTP layer: the REST API under /api/v1 (JSON, *arr-style API key or
// login session) and the embedded web UI, plus the wiring of the Phase 1 services it serves (App).
//
// Every Phase 1 route (docs/design/phase1.md §7) requires authentication. Request bodies are
// single JSON objects with unknown fields rejected. Errors are {"message": "..."}: 400 for invalid
// input, 404 for an unknown id, 409 for a conflict with the current state (including the safety
// refusals of S3 that need a confirmation or a mounted share), 502 when Plex fails, and 500 for
// everything else, with registered secret values redacted from every message. Secrets (Plex
// tokens, Apprise URLs) are write-only: responses say hasApiKey / hasUrls (S8). Job-starting
// endpoints answer 202 with the queued Job. Every route is documented in openapi.json.
package api

import (
	_ "embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/auth"
	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

//go:embed openapi.json
var openAPISpec []byte

// Options wires the server's dependencies.
type Options struct {
	Auth    *auth.Service
	DB      *db.DB
	Env     config.Env
	Log     *slog.Logger
	Web     fs.FS
	Started time.Time
	// App holds the Phase 1 services (sources, destinations, jobs, ...). Without it those
	// routes answer 503.
	App *App
}

// Server serves the API and the UI.
type Server struct {
	auth    *auth.Service
	db      *db.DB
	env     config.Env
	log     *slog.Logger
	web     fs.FS
	started time.Time
	app     *App
}

// New returns a Server.
func New(o Options) *Server {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Started.IsZero() {
		o.Started = time.Now()
	}
	return &Server{auth: o.Auth, db: o.DB, env: o.Env, log: o.Log, web: o.Web, started: o.Started, app: o.App}
}

// Handler returns the root handler.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(recoverer(s.log), securityHeaders, requestLog(s.log))
	r.Route("/api/v1", s.apiRoutes)
	if s.web != nil {
		r.Handle("/*", spaHandler(s.web))
	}
	return r
}

func (s *Server) apiRoutes(r chi.Router) {
	// Cross-site browser requests that change state are refused (CSRF). API clients such as
	// scripts and the *arr apps send no Origin / Sec-Fetch-Site headers and are unaffected.
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusForbidden, "cross-origin request refused")
	}))
	r.Use(cop.Handler, noStore)
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusNotFound, "not found")
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	})

	r.Get("/health", s.health)
	r.Get("/openapi.json", s.openAPI)
	r.Get("/auth/status", s.authStatus)
	r.Post("/auth/setup", s.authSetup)
	r.Post("/auth/login", s.authLogin)
	r.Post("/auth/logout", s.authLogout)
	// The *arrs' webhooks: the integration's webhook key only, never a session or the API key (D7).
	s.webhookRoutes(r)

	r.Group(func(r chi.Router) {
		r.Use(s.auth.Require)
		r.Get("/system/status", s.systemStatus)
		r.Get("/settings/general", s.getGeneralSettings)
		r.Put("/settings/general", s.putGeneralSettings)
		r.Post("/settings/general/apikey", s.regenerateAPIKey)
		r.Put("/auth/credentials", s.changeCredentials)

		r.Group(func(r chi.Router) {
			r.Use(s.requireApp)
			s.integrationRoutes(r)
			s.arrIndexRoutes(r)
			s.providerRoutes(r)
			s.webhookInfoRoutes(r)
			s.arrBackupRoutes(r)
			s.plexSignInRoutes(r)
			s.sourceRoutes(r)
			s.destinationRoutes(r)
			s.manifestRoutes(r)
			s.tierRoutes(r)
			s.jobRoutes(r)
			s.notificationRoutes(r)
		})
	})
}

// requireApp answers 503 when the server was built without the Phase 1 services.
func (s *Server) requireApp(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.app == nil {
			writeError(w, http.StatusServiceUnavailable, "this server has no backup services configured")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ping(r.Context()); err != nil {
		s.log.Error("Health check: database unavailable", "error", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "error", "message": "database unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) openAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(openAPISpec)
}

// SystemStatus is GET /api/v1/system/status.
type SystemStatus struct {
	AppName       string    `json:"appName"`
	Version       string    `json:"version"`
	Commit        string    `json:"commit"`
	BuildDate     string    `json:"buildDate"`
	StartTime     time.Time `json:"startTime"`
	UptimeSeconds int64     `json:"uptimeSeconds"`
	DatabasePath  string    `json:"databasePath"`
	SchemaVersion int       `json:"schemaVersion"`
	ConfigDir     string    `json:"configDir"`
	GoVersion     string    `json:"goVersion"`
	OS            string    `json:"os"`
	Arch          string    `json:"arch"`
	IsDocker      bool      `json:"isDocker"`
	AuthMethod    string    `json:"authenticationMethod"`
	AuthRequired  auth.Mode `json:"authenticationRequired"`
}

func (s *Server) systemStatus(w http.ResponseWriter, r *http.Request) {
	schema, err := s.db.SchemaVersion(r.Context())
	if err != nil {
		s.log.Error("Read schema version", "error", err)
	}
	writeJSON(w, http.StatusOK, SystemStatus{
		AppName:       "Bunkarr",
		Version:       version.Version,
		Commit:        version.Commit,
		BuildDate:     version.BuildDate,
		StartTime:     s.started.UTC(),
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		DatabasePath:  s.db.Path,
		SchemaVersion: schema,
		ConfigDir:     s.env.ConfigDir,
		GoVersion:     runtime.Version(),
		OS:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		IsDocker:      s.env.Docker,
		AuthMethod:    "forms",
		AuthRequired:  s.auth.Mode(),
	})
}

// AuthStatus is GET /api/v1/auth/status: what the UI needs before it can render.
type AuthStatus struct {
	SetupRequired bool      `json:"setupRequired"`
	Authenticated bool      `json:"authenticated"`
	Via           string    `json:"via,omitempty"`
	User          *string   `json:"username"`
	AuthRequired  auth.Mode `json:"authenticationRequired"`
}

func (s *Server) authStatus(w http.ResponseWriter, r *http.Request) {
	setup, err := s.auth.SetupRequired(r.Context())
	if err != nil {
		s.internalError(w, "check setup state", err)
		return
	}
	st := AuthStatus{SetupRequired: setup, AuthRequired: s.auth.Mode()}
	if !setup {
		p, ok, err := s.auth.Identify(r)
		if err != nil && !errors.Is(err, auth.ErrBadAPIKey) {
			s.internalError(w, "identify request", err)
			return
		}
		st.Authenticated, st.Via = ok, p.Kind
		if p.User != nil {
			st.User = &p.User.Username
		}
	}
	writeJSON(w, http.StatusOK, st)
}

type credentialsBody struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) authSetup(w http.ResponseWriter, r *http.Request) {
	var body credentialsBody
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err := s.auth.Setup(r.Context(), body.Username, body.Password)
	var verr auth.ValidationError
	switch {
	case errors.Is(err, auth.ErrSetupDone):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.As(err, &verr):
		writeError(w, http.StatusBadRequest, verr.Error())
		return
	case err != nil:
		s.internalError(w, "create user", err)
		return
	}
	s.log.Info("First-run setup completed", "remote", auth.ClientIP(r).String())
	s.startSession(w, r, body.Username, body.Password, http.StatusCreated)
}

func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	client := auth.ClientIP(r).String()
	if blocked, wait := s.auth.Limiter.Blocked(client); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many failed logins; try again later")
		return
	}
	var body credentialsBody
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.startSession(w, r, body.Username, body.Password, http.StatusOK)
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, username, password string, status int) {
	client := auth.ClientIP(r).String()
	sess, err := s.auth.Login(r.Context(), username, password, auth.SessionMeta{RemoteAddr: client, UserAgent: r.UserAgent()})
	if errors.Is(err, auth.ErrInvalidCredentials) {
		s.auth.Limiter.Fail(client)
		s.log.Warn("Failed login", "remote", client)
		writeError(w, http.StatusUnauthorized, err.Error())
		return
	}
	if err != nil {
		s.internalError(w, "log in", err)
		return
	}
	s.auth.Limiter.Success(client)
	auth.SetSessionCookie(w, r, sess.Token, sess.Expires)
	writeJSON(w, status, map[string]string{"username": sess.User.Username})
}

func (s *Server) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(auth.SessionCookie); err == nil {
		if err := s.auth.Logout(r.Context(), c.Value); err != nil {
			s.internalError(w, "log out", err)
			return
		}
	}
	auth.ClearSessionCookie(w, r)
	w.WriteHeader(http.StatusNoContent)
}

// GeneralSettings is Settings > General.
type GeneralSettings struct {
	APIKey       string    `json:"apiKey"`
	AuthRequired auth.Mode `json:"authenticationRequired"`
	AuthMethod   string    `json:"authenticationMethod"`
	BindAddress  string    `json:"bindAddress"`
	Port         int       `json:"port"`
}

func (s *Server) generalSettings() GeneralSettings {
	bind := s.env.Bind
	if bind == "" {
		bind = "*"
	}
	return GeneralSettings{
		APIKey:       s.auth.APIKey(),
		AuthRequired: s.auth.Mode(),
		AuthMethod:   "forms",
		BindAddress:  bind,
		Port:         s.env.Port,
	}
}

func (s *Server) getGeneralSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.generalSettings())
}

func (s *Server) putGeneralSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AuthRequired auth.Mode `json:"authenticationRequired"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	var verr auth.ValidationError
	if err := s.auth.SetMode(r.Context(), body.AuthRequired); errors.As(err, &verr) {
		writeError(w, http.StatusBadRequest, verr.Error())
		return
	} else if err != nil {
		s.internalError(w, "save settings", err)
		return
	}
	s.log.Info("Authentication requirement changed", "authenticationRequired", body.AuthRequired)
	writeJSON(w, http.StatusOK, s.generalSettings())
}

func (s *Server) regenerateAPIKey(w http.ResponseWriter, r *http.Request) {
	if _, err := s.auth.RegenerateAPIKey(r.Context()); err != nil {
		s.internalError(w, "regenerate API key", err)
		return
	}
	s.log.Info("API key regenerated")
	writeJSON(w, http.StatusOK, s.generalSettings())
}

func (s *Server) changeCredentials(w http.ResponseWriter, r *http.Request) {
	p, _ := auth.PrincipalFrom(r.Context())
	if p.Kind != auth.KindSession || p.User == nil {
		writeError(w, http.StatusForbidden, "log in with your username and password to change them")
		return
	}
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		Username        string `json:"username"`
		NewPassword     string `json:"newPassword"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	u, err := s.auth.ChangeCredentials(r.Context(), p.User.ID, body.CurrentPassword, body.Username, body.NewPassword, p.Token)
	var verr auth.ValidationError
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		writeError(w, http.StatusBadRequest, "current password is incorrect")
	case errors.As(err, &verr):
		writeError(w, http.StatusBadRequest, verr.Error())
	case err != nil:
		s.internalError(w, "change credentials", err)
	default:
		writeJSON(w, http.StatusOK, map[string]string{"username": u.Username})
	}
}

func (s *Server) internalError(w http.ResponseWriter, what string, err error) {
	s.log.Error("Request failed", "action", what, "error", err)
	writeError(w, http.StatusInternalServerError, "internal server error")
}
