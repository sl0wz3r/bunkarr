package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/config"
	"github.com/sl0wz3r/bunkarr/internal/tiers"
)

// TestDeleteArrIntegrationKeepsItsFoldersUnknown: DELETE /integrations/{id} of an *arr records
// its root folders for the tier facts before the cascade drops its index, so its files stay
// unknown (full) instead of turning unmanaged (S14) until the user confirms the removal.
func TestDeleteArrIntegrationKeepsItsFoldersUnknown(t *testing.T) {
	e := newEnv(t, nil)
	_, it, _ := arrSetup(t, e)
	list, err := e.app.Tiers.DeletedArrs(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("records before the delete %+v, %v", list, err)
	}
	e.call(t, 204, "DELETE", fmt.Sprintf("/integrations/%d", it.ID), nil, nil)
	list, err = e.app.Tiers.DeletedArrs(context.Background())
	if err != nil || len(list) != 1 || list[0].IntegrationID != it.ID || len(list[0].Folders) != 1 {
		t.Fatalf("records after the delete %+v, %v", list, err)
	}
}

// TestConfirmDeletedArrIntegration: after an *arr integration is deleted its files stay full
// (unknown-promoted) and the preview names it as an unknown source; GET /integrations/deleted
// lists it, and DELETE /integrations/deleted/{key} (the user's confirmation, logged) forgets its
// folders, so the files are unmanaged and the rules decide them again (manifest here: before the
// confirmation the unknown "Keep" rule, which is more protective, decided).
func TestConfirmDeletedArrIntegration(t *testing.T) {
	base := newEnv(t, nil)
	// The server under test logs to a buffer (the confirmation is the audit trail).
	logs := &lockedBuffer{}
	logged := New(Options{Auth: base.auth, DB: base.db, Env: config.Env{ConfigDir: base.config, Port: 8787}, App: base.app,
		Log: slog.New(slog.NewTextHandler(logs, nil))})
	srv := httptest.NewServer(logged.Handler())
	t.Cleanup(srv.Close)
	e := *base
	e.srv, e.api = srv, logged
	env := &e

	_, it, srcID := arrSetup(t, env)
	env.createDestination(t, "UNAS", env.mkdir(t, "target"), []int64{srcID}, nil)
	env.call(t, 200, "PUT", "/tiers/rules", map[string]any{"revision": 0, "rules": []map[string]any{
		{"name": "Keep", "action": "full", "conditions": []map[string]any{{"field": "arr.tag", "op": "has", "value": "keep-forever"}}},
		{"name": "Everything else", "action": "manifest"},
	}}, nil)
	var fileID int64
	if err := env.db.Reader().QueryRow(`SELECT id FROM catalog_files WHERE source_id = ? ORDER BY rel_path LIMIT 1`, srcID).Scan(&fileID); err != nil {
		t.Fatal(err)
	}
	detail := func(when string) FileDetail {
		t.Helper()
		var fd FileDetail
		env.call(t, 200, "GET", fmt.Sprintf("/catalog/files/%d", fileID), nil, &fd)
		if len(fd.Tiers) != 1 {
			t.Fatalf("%s: tiers %+v", when, fd.Tiers)
		}
		return fd
	}
	preview := func() tiers.TierPreview {
		t.Helper()
		var p tiers.TierPreview
		env.call(t, 200, "POST", "/tiers/preview", nil, &p)
		if len(p.Destinations) != 1 {
			t.Fatalf("preview %+v", p)
		}
		return p
	}
	// Managed by Radarr, not tagged: manifest.
	if fd := detail("before the delete"); fd.Tiers[0].Tier != tiers.Manifest || fd.Tiers[0].UnknownPromoted || fd.Facts.Arr.State != tiers.ArrItem {
		t.Fatalf("before the delete: %+v %+v", fd.Tiers[0], fd.Facts.Arr)
	}
	var deleted []tiers.DeletedArr
	env.call(t, 200, "GET", "/integrations/deleted", nil, &deleted)
	if len(deleted) != 0 {
		t.Fatalf("deleted before the delete %+v", deleted)
	}

	env.call(t, 204, "DELETE", fmt.Sprintf("/integrations/%d", it.ID), nil, nil)
	env.call(t, 200, "GET", "/integrations/deleted", nil, &deleted)
	if len(deleted) != 1 || deleted[0].Key == "" || deleted[0].IntegrationID != it.ID || deleted[0].Name != "Radarr" || deleted[0].App != "Radarr" ||
		len(deleted[0].Folders) != 1 || deleted[0].DeletedAt.IsZero() {
		t.Fatalf("deleted after the delete %+v", deleted)
	}
	// Deleted, not confirmed: unknown, so full (unknown-promoted), and the reason says where to confirm.
	fd := detail("after the delete")
	if fd.Tiers[0].Tier != tiers.Full || !fd.Tiers[0].UnknownPromoted || fd.Tiers[0].RuleName != "Keep" || fd.Facts.Arr.State != tiers.ArrUnknown ||
		!strings.Contains(fd.Facts.Arr.Why, "was deleted") || !strings.Contains(fd.Facts.Arr.Why, "Settings → Tiers") {
		t.Fatalf("after the delete: %+v %+v", fd.Tiers[0], fd.Facts.Arr)
	}
	p := preview()
	if len(p.UnknownSources) != 1 || p.UnknownSources[0].IntegrationID != it.ID || !strings.Contains(p.UnknownSources[0].Reason, "confirm its removal") ||
		p.Destinations[0].UnknownPromoted.Files == 0 || p.Destinations[0].Manifest.Files != 0 {
		t.Fatalf("preview after the delete %+v %+v", p.UnknownSources, p.Destinations[0])
	}

	// Refusals: an unknown key is a 404 and changes nothing.
	for _, key := range []string{"0000000000000000", "nope"} {
		if code, msg := env.status(t, "DELETE", "/integrations/deleted/"+key, nil); code != 404 || !strings.Contains(msg, "not found") {
			t.Errorf("DELETE /integrations/deleted/%s: %d %q", key, code, msg)
		}
	}
	env.call(t, 200, "GET", "/integrations/deleted", nil, &deleted)
	if len(deleted) != 1 {
		t.Fatalf("an unknown key removed a record: %+v", deleted)
	}
	if strings.Contains(logs.String(), "removal confirmed") {
		t.Fatalf("a refused confirmation was logged: %s", logs.String())
	}

	// The confirmation: logged, the record is gone, the files are unmanaged and the rule decides.
	key := deleted[0].Key
	env.call(t, 204, "DELETE", "/integrations/deleted/"+key, nil, nil)
	out := logs.String()
	if !strings.Contains(out, "Deleted integration removal confirmed") || !strings.Contains(out, "key="+key) ||
		!strings.Contains(out, fmt.Sprintf("integrationId=%d", it.ID)) || !strings.Contains(out, "folders=1") {
		t.Errorf("confirmation log: %s", out)
	}
	env.call(t, 200, "GET", "/integrations/deleted", nil, &deleted)
	if len(deleted) != 0 {
		t.Fatalf("deleted after confirming %+v", deleted)
	}
	if code, _ := env.status(t, "DELETE", "/integrations/deleted/"+key, nil); code != 404 {
		t.Errorf("second confirmation: %d, want 404", code)
	}
	fd = detail("after confirming")
	if fd.Tiers[0].Tier != tiers.Manifest || fd.Tiers[0].UnknownPromoted || fd.Tiers[0].RuleName != "Everything else" || fd.Facts.Arr.State != tiers.ArrUnmanaged {
		t.Fatalf("after confirming: %+v %+v", fd.Tiers[0], fd.Facts.Arr)
	}
	p = preview()
	if len(p.UnknownSources) != 0 || p.Destinations[0].UnknownPromoted.Files != 0 || p.Destinations[0].Manifest.Files == 0 {
		t.Fatalf("preview after confirming %+v %+v", p.UnknownSources, p.Destinations[0])
	}
}

// TestFailedDeleteForgetsItsRecord: ForgetDeletedArr (the undo of a failed delete) removes only
// the record it names and is not an error for a key that names none, unlike the confirmation.
func TestFailedDeleteForgetsItsRecord(t *testing.T) {
	e := newEnv(t, nil)
	_, it, _ := arrSetup(t, e)
	ctx := context.Background()
	gone, err := e.app.Tiers.RememberDeletedArr(ctx, it)
	if err != nil || gone.Key == "" {
		t.Fatalf("remember %+v, %v", gone, err)
	}
	if err := e.app.Tiers.ForgetDeletedArr(ctx, "0000000000000000"); err != nil {
		t.Fatalf("forget an unknown key: %v", err)
	}
	if _, err := e.app.Tiers.ConfirmDeletedArr(ctx, "0000000000000000"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("confirm an unknown key: %v", err)
	}
	if list, _ := e.app.Tiers.DeletedArrs(ctx); len(list) != 1 {
		t.Fatalf("records %+v", list)
	}
	if err := e.app.Tiers.ForgetDeletedArr(ctx, gone.Key); err != nil {
		t.Fatal(err)
	}
	if list, _ := e.app.Tiers.DeletedArrs(ctx); len(list) != 0 {
		t.Fatalf("records after forget %+v", list)
	}
}
