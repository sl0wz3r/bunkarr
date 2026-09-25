package api

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/sl0wz3r/bunkarr/internal/logging"
	"github.com/sl0wz3r/bunkarr/internal/notify"
)

// Notifications (design §7, Settings → Connect). The Apprise URLs are write-only: responses carry
// hasUrls.

func (s *Server) notificationRoutes(r chi.Router) {
	r.Get("/notifications", s.listNotifications)
	r.Post("/notifications", s.createNotification)
	r.Post("/notifications/test", s.testNotification)
	r.Put("/notifications/{id}", s.updateNotification)
	r.Delete("/notifications/{id}", s.deleteNotification)
}

func (s *Server) listNotifications(w http.ResponseWriter, r *http.Request) {
	list, err := s.app.Notifications.List(r.Context())
	if err != nil {
		s.fail(w, r, "list notifications", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) createNotification(w http.ResponseWriter, r *http.Request) {
	var in notify.Input
	if err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, "create notification", err)
		return
	}
	n, err := s.app.Notifications.Create(r.Context(), in)
	if err != nil {
		s.fail(w, r, "create notification", err)
		return
	}
	s.log.Info("Notification created", "id", n.ID, "name", n.Name)
	writeJSON(w, http.StatusCreated, n)
}

// updateNotification replaces a notification (see notify.Input). The stored Apprise URLs are bound
// to the stored Apprise API URL: a request that changes apiUrl of a stateless notification must
// send the URLs again. Such a request is refused with a 400 that asks for them, rather than
// silently clearing them, so they are never posted to a host chosen by a caller who does not know
// them (S8).
func (s *Server) updateNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "update notification", err)
		return
	}
	var in notify.Input
	if err := decodeBody(w, r, &in); err != nil {
		s.fail(w, r, "update notification", err)
		return
	}
	n, err := s.app.Notifications.Update(r.Context(), id, in)
	if err != nil {
		s.fail(w, r, "update notification", err)
		return
	}
	s.log.Info("Notification updated", "id", n.ID, "name", n.Name)
	writeJSON(w, http.StatusOK, n)
}

func (s *Server) deleteNotification(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, r, "delete notification", err)
		return
	}
	if err := s.app.Notifications.Delete(r.Context(), id); err != nil {
		s.fail(w, r, "delete notification", err)
		return
	}
	s.log.Info("Notification deleted", "id", id)
	w.WriteHeader(http.StatusNoContent)
}

// testResult is POST /notifications/test's answer.
type testResult struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
}

// testNotification sends a test message. The body is a notification form (notify.Input) plus an
// optional id: with an id, an empty urls field uses the stored URLs, but only when apiUrl is the
// stored one (another apiUrl is a 400 asking for the URLs); an id alone (no apiUrl) tests the
// saved notification as it is. Invalid settings are a 400; a delivery failure is
// {ok: false, message}.
func (s *Server) testNotification(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
		notify.Input
	}
	if err := decodeBody(w, r, &body); err != nil {
		s.fail(w, r, "test notification", err)
		return
	}
	if body.ID < 0 {
		s.fail(w, r, "test notification", errorf(http.StatusBadRequest, "id must be a notification id"))
		return
	}
	in := &body.Input
	if body.ID > 0 && body.APIURL == "" {
		in = nil
	}
	err := s.app.Notifier.Test(r.Context(), body.ID, in)
	var se *notify.SendError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, testResult{OK: true, Message: "The test notification was sent."})
	case errors.As(err, &se):
		writeJSON(w, http.StatusOK, testResult{Message: logging.RedactSecrets(se.Error())})
	default:
		s.fail(w, r, "test notification", err)
	}
}
