package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
	"github.com/sl0wz3r/bunkarr/internal/faultinject"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
	"github.com/sl0wz3r/bunkarr/internal/manifest"
)

// download sends an authenticated GET and returns the response with its body read.
func (e *env) download(t *testing.T, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Api-Key", e.auth.APIKey())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res, body
}

// requireManifestFile checks a manifest download's headers against its body.
func requireManifestFile(t *testing.T, res *http.Response, body []byte, contentType, namePrefix string) {
	t.Helper()
	sum := sha256.Sum256(body)
	if res.StatusCode != 200 || res.Header.Get(SHA256Header) != hex.EncodeToString(sum[:]) ||
		res.Header.Get("Content-Length") != strconv.Itoa(len(body)) || res.Header.Get("Content-Type") != contentType ||
		!strings.HasPrefix(res.Header.Get("Content-Disposition"), `attachment; filename=`+namePrefix) {
		t.Fatalf("status %d, headers %v, %d bytes", res.StatusCode, res.Header, len(body))
	}
}

// manifestSetup is arrSetup plus a destination linked to the source, with one manifest version.
func manifestSetup(t *testing.T) (*env, int64, manifest.Version, string) {
	t.Helper()
	e := newEnv(t, nil)
	fake, _, srcID := arrSetup(t, e)
	target := e.mkdir(t, "target")
	destID := e.createDestination(t, "UNAS", target, []int64{srcID}, nil)
	var job jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/manifest", destID), nil, &job)
	if job.Type != jobs.TypeManifestExport || job.Params.DestinationID != destID {
		t.Fatalf("job %+v", job)
	}
	if final := e.waitJob(t, job.ID); final.Status != jobs.StatusCompleted {
		t.Fatalf("export %+v", final)
	}
	var list []manifest.Version
	e.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/manifests", destID), nil, &list)
	if len(list) != 1 || list[0].DestinationID != destID || list[0].JobID != job.ID || list[0].ItemCount != 4 || list[0].FileCount != 3 ||
		list[0].Integrity != "ok" || !strings.HasPrefix(list[0].Checksum, "sha256:") {
		t.Fatalf("manifests %+v", list)
	}
	return e, destID, list[0], fake.URL
}

func TestManifestExportAndDownload(t *testing.T) {
	e, destID, v, fakeURL := manifestSetup(t)
	// The Manifest shape: no content hash.
	_, raw := e.raw(t, "GET", fmt.Sprintf("/destinations/%d/manifests", destID), nil)
	if bytes.Contains(raw, []byte("contentHash")) || !bytes.Contains(raw, []byte(`"itemCount":4`)) {
		t.Fatalf("list %s", raw)
	}

	res, body := e.download(t, fmt.Sprintf("/manifests/%d/download", v.ID))
	requireManifestFile(t, res, body, "application/json", "bunkarr-manifest-unas-")
	if manifest.Checksum(res.Header.Get(SHA256Header)) != v.Checksum {
		t.Fatalf("digest %s, recorded %s", res.Header.Get(SHA256Header), v.Checksum)
	}
	m, err := manifest.Parse(bytes.NewReader(body))
	if err != nil || m.Scope.DestinationID != destID || len(m.Items) != 4 {
		t.Fatalf("parse: %v, %+v", err, m)
	}
	for _, secret := range []string{arrTestKey, fakeURL, strings.TrimPrefix(fakeURL, "http://")} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("the manifest contains %q", secret)
		}
	}
	res, body = e.download(t, fmt.Sprintf("/manifests/%d/download?format=csv", v.ID))
	requireManifestFile(t, res, body, "text/csv; charset=utf-8", "bunkarr-manifest-unas-")
	if rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll(); err != nil || len(rows) != 1+4 {
		t.Fatalf("csv: %d rows, %v", len(rows), err)
	}

	// The on-the-spot export, in the export scope and in a destination's view.
	res, body = e.download(t, "/manifest/export")
	requireManifestFile(t, res, body, "application/json", "bunkarr-manifest-all-")
	if m, err := manifest.Parse(bytes.NewReader(body)); err != nil || m.Scope.Kind != manifest.ScopeExport || len(m.Items) != 4 {
		t.Fatalf("export: %v, %+v", err, m)
	}
	res, body = e.download(t, fmt.Sprintf("/manifest/export?format=csv&destinationId=%d", destID))
	requireManifestFile(t, res, body, "text/csv; charset=utf-8", "bunkarr-manifest-unas-")

	// Errors.
	for path, want := range map[string]int{
		"/manifests/999/download":                              404,
		fmt.Sprintf("/manifests/%d/download?format=xml", v.ID): 400,
		"/manifests/x/download":                                400,
		"/manifest/export?format=pdf":                          400,
		"/manifest/export?destinationId=999":                   404,
		"/manifest/export?destinationId=-1":                    400,
		"/destinations/999/manifests":                          404,
	} {
		if code, msg := e.status(t, "GET", path, nil); code != want {
			t.Errorf("GET %s: %d %s, want %d", path, code, msg, want)
		}
	}
	if code, _ := e.status(t, "POST", "/destinations/999/manifest", nil); code != 404 {
		t.Fatalf("export of an unknown destination: %d", code)
	}
	if code, _ := e.status(t, "POST", fmt.Sprintf("/destinations/%d/manifest", destID), `{"bogus":1}`); code != 400 {
		t.Fatalf("unknown field: %d", code)
	}
	// A dry run writes nothing.
	var job jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/manifest", destID), map[string]any{"dryRun": true}, &job)
	if final := e.waitJob(t, job.ID); !final.DryRun || final.Status != jobs.StatusCompleted || !strings.Contains(string(final.Stats), `"unchanged":true`) {
		t.Fatalf("dry run %+v", final)
	}
	if list, _ := e.app.Manifests.Store().List(t.Context(), destID); len(list) != 1 {
		t.Fatalf("after a dry run: %+v", list)
	}
}

func TestManifestDownloadRefusesDamagedAndUnmounted(t *testing.T) {
	e, destID, v, _ := manifestSetup(t)
	d, err := e.app.Destinations.Get(t.Context(), destID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(d.Target, filepath.FromSlash(v.Path))
	if err := os.WriteFile(filepath.Join(dir, manifest.CSVName), []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, msg := e.status(t, "GET", fmt.Sprintf("/manifests/%d/download?format=csv", v.ID), nil)
	if code != 409 || msg != "manifest damaged: checksum mismatch" {
		t.Fatalf("damaged: %d %q", code, msg)
	}
	var list []manifest.Version
	e.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/manifests", destID), nil, &list)
	if list[0].Integrity != "damaged" {
		t.Fatalf("not marked damaged: %+v", list[0])
	}
	// The next export writes a new version (the damaged one is not "unchanged").
	var job jobs.Job
	e.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/manifest", destID), nil, &job)
	e.waitJob(t, job.ID)
	e.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/manifests", destID), nil, &list)
	if len(list) != 2 || list[0].Integrity != "ok" {
		t.Fatalf("after the damage: %+v", list)
	}
	// Not mounted: 409, and nothing marked.
	if err := os.Remove(filepath.Join(d.Target, filepath.FromSlash(filecopy.MarkerRel))); err != nil {
		t.Fatal(err)
	}
	if code, msg := e.status(t, "GET", fmt.Sprintf("/manifests/%d/download", list[0].ID), nil); code != 409 || !strings.Contains(msg, "not mounted") {
		t.Fatalf("unmounted: %d %q", code, msg)
	}
	e.call(t, 200, "GET", fmt.Sprintf("/destinations/%d/manifests", destID), nil, &list)
	if list[0].Integrity != "ok" {
		t.Fatalf("an unmounted destination marked a version: %+v", list[0])
	}
	// A disabled destination takes no export job.
	off := false
	e.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", destID), map[string]any{"enabled": &off}, nil)
	if code, _ := e.status(t, "POST", fmt.Sprintf("/destinations/%d/manifest", destID), nil); code != 409 {
		t.Fatalf("disabled: %d", code)
	}
}

func TestManifestExportLimitAndFailedBuild(t *testing.T) {
	e := newEnv(t, nil)
	arrSetup(t, e)
	// Two exports being served: the third answers 429.
	var held []*manifest.StagedFile
	for range manifest.DefaultMaxExports {
		f, err := e.app.Manifests.Export(t.Context(), 0, manifest.FormatJSON)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, f)
	}
	res, body := e.download(t, "/manifest/export")
	if res.StatusCode != 429 || res.Header.Get("Retry-After") == "" || res.Header.Get(SHA256Header) != "" {
		t.Fatalf("busy: %d %v %s", res.StatusCode, res.Header, body)
	}
	for _, f := range held {
		_ = f.Close()
	}
	// A build that fails half-way answers 500 before any byte of the manifest.
	faultinject.SetHook(faultinject.CrashAt(manifest.PointBuildItem, 2))
	res, body = e.download(t, "/manifest/export")
	faultinject.SetHook(nil)
	if res.StatusCode != 500 || res.Header.Get(SHA256Header) != "" || res.Header.Get("Content-Disposition") != "" ||
		bytes.Contains(body, []byte(manifest.FormatName)) {
		t.Fatalf("failed build: %d %v %s", res.StatusCode, res.Header, body)
	}
	// And frees its slot.
	for range manifest.DefaultMaxExports {
		if res, _ := e.download(t, "/manifest/export"); res.StatusCode != 200 {
			t.Fatalf("after the failed build: %d", res.StatusCode)
		}
	}
}
