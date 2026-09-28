package notify

import (
	"context"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/logging"
)

// TestWarnHonoursOnWarning checks the warnings that belong to no job (phase4.md §5.2, §11.5): they
// reach every enabled target with onWarning, not the others, with the title prefixed and every
// registered secret redacted.
func TestWarnHonoursOnWarning(t *testing.T) {
	ctx := context.Background()
	s, _, _ := openStore(t)
	f := newFakeApprise(t, fakeOpts{})
	for _, in := range []Input{
		{Name: "warnings", APIURL: f.URL(), ConfigKey: "warnings"},
		{Name: "failures only", APIURL: f.URL(), ConfigKey: "failures", OnWarning: new(false)},
		{Name: "disabled", APIURL: f.URL(), ConfigKey: "disabled", Enabled: new(false)},
		{Name: "everything", APIURL: f.URL(), ConfigKey: "everything", OnSuccess: new(true)},
	} {
		if _, err := s.Create(ctx, in); err != nil {
			t.Fatal(err)
		}
	}
	const secret = "warn-secret-value-5a5a5a"
	logging.SetSecrets("test:warn", secret)
	t.Cleanup(func() { logging.SetSecrets("test:warn") })

	d := New(s, nil, Options{Client: testClient()})
	d.Warn("Recovery kit for UNAS exported", "exported at 12:00 from 10.0.0.2 with "+secret)
	d.Warn("  ", "")
	d.pending.Wait()
	if err := d.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reqs := f.requests()
	var paths []string
	for _, r := range reqs {
		paths = append(paths, r.Path)
		title, _ := r.Body["title"].(string)
		body, _ := r.Body["body"].(string)
		if r.Body["type"] != string(TypeWarning) {
			t.Errorf("%s: type %v, want warning", r.Path, r.Body["type"])
		}
		if !strings.HasPrefix(title, "Bunkarr: ") || body == "" {
			t.Errorf("%s: title %q body %q", r.Path, title, body)
		}
		if strings.Contains(title+body, secret) {
			t.Errorf("%s: a registered secret was sent: %q %q", r.Path, title, body)
		}
	}
	if len(reqs) != 4 {
		t.Fatalf("sent %d requests (%v), want 2 warnings to 2 targets", len(reqs), paths)
	}
	for _, p := range paths {
		if !strings.HasSuffix(p, "/warnings") && !strings.HasSuffix(p, "/everything") {
			t.Errorf("warning sent to %s, which did not subscribe to warnings", p)
		}
	}
	// After Close a warning is dropped, without a panic.
	d.Warn("late", "late")
}
