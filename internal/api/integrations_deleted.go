package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Deleted *arr integrations (phase2-3.md §8.3, S14). Deleting an *arr integration cascades its
// index, so the tier facts keep a record of its folders (tiers.RememberDeletedArr): a file there
// that no live *arr claims stays unknown, so full, until the user confirms the removal here. The
// confirmation forgets the record; those files are then decided by the live integrations alone
// (unmanaged when every *arr cache is fresh and mapped), so the rules may lower their tier. What
// the destinations hold for them is kept (S15) until a confirmed release.

func (s *Server) deletedIntegrationRoutes(r chi.Router) {
	r.Get("/integrations/deleted", s.listDeletedIntegrations)
	r.Delete("/integrations/deleted/{key}", s.confirmDeletedIntegration)
}

func (s *Server) listDeletedIntegrations(w http.ResponseWriter, r *http.Request) {
	list, err := s.app.Tiers.DeletedArrs(r.Context())
	if err != nil {
		s.fail(w, r, "list deleted integrations", err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// confirmDeletedIntegration forgets a deleted *arr integration's folders (the user confirmed the
// removal). It is logged with what it forgot, as the audit trail of a change that can lower tiers.
func (s *Server) confirmDeletedIntegration(w http.ResponseWriter, r *http.Request) {
	key := chi.URLParam(r, "key")
	gone, err := s.app.Tiers.ConfirmDeletedArr(r.Context(), key)
	if err != nil {
		s.fail(w, r, "confirm the removal of a deleted integration", tierError(err))
		return
	}
	s.log.Info("Deleted integration removal confirmed: its folders are decided by the live integrations from the next sync",
		"key", gone.Key, "integrationId", gone.IntegrationID, "name", gone.Name, "app", gone.App,
		"folders", len(gone.Folders), "unmapped", len(gone.Unmapped), "deletedAt", gone.DeletedAt)
	w.WriteHeader(http.StatusNoContent)
}
