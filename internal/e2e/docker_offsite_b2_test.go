//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestDockerOffsiteB2 is the manual pass against real Backblaze B2 (docs/design/phase4.md §14.6
// item 9, acceptance 1's B2 half; §17's B2 uncertainties). It runs only with BUNKARR_E2E_B2_KEY_ID,
// BUNKARR_E2E_B2_KEY and BUNKARR_E2E_B2_BUCKET set (and BUNKARR_E2E_OFFSITE=1), never in CI: the
// lifecycle through restic and through rclone crypt under a fresh prefix of the bucket (initial
// sync, the change set, verify, retention with prune and with backend cleanup); afterwards the
// prefix holds no hidden file version (hard deletes) and no unfinished large file; a key that is not
// restricted to the bucket gets the warning of POST /destinations/test (and a restricted one does
// not); and the rclone destination's backend cleanup leaves an unfinished large file under another
// prefix alone. The test removes everything it wrote under both prefixes.
func TestDockerOffsiteB2(t *testing.T) {
	keyID, key, bucket := os.Getenv("BUNKARR_E2E_B2_KEY_ID"), os.Getenv("BUNKARR_E2E_B2_KEY"), os.Getenv("BUNKARR_E2E_B2_BUCKET")
	if keyID == "" || key == "" || bucket == "" {
		t.Skip("BUNKARR_E2E_B2_KEY_ID, BUNKARR_E2E_B2_KEY and BUNKARR_E2E_B2_BUCKET are not set (the manual real-B2 pass; never in CI)")
	}
	o := newOffsite(t)
	o.secrets.add("B2 application key", key)
	o.secrets.add("B2 key id", keyID)
	api := b2Authorize(t, keyID, key)
	bucketID := api.bucketID(t, bucket)
	prefix := "bunkarr-e2e-" + randomHex(4)
	other := api.startLargeFile(t, bucketID, prefix+"-other/unfinished.bin")
	t.Cleanup(func() { api.cleanPrefix(t, bucketID, prefix) })

	b := o.startBunkarr("bunkarr", bunkarrOpts{})
	creds := map[string]any{"keyId": keyID, "applicationKey": key}
	// The unrestricted-key warning follows b2_authorize_account's allowed.bucketId.
	var tr struct {
		OK       bool     `json:"ok"`
		Warnings []string `json:"warnings"`
		Message  string   `json:"message"`
	}
	b.api.call(viaSession, 200, "POST", "/destinations/test", map[string]any{"kind": "b2", "engine": "rclone",
		"remote": map[string]any{"bucket": bucket, "prefix": prefix + "/probe"}, "credentials": creds,
		"encryption": map[string]any{"mode": "crypt"}}, &tr)
	warned := strings.Contains(strings.ToLower(strings.Join(tr.Warnings, " ")), "restricted")
	if restricted := api.AllowedBucketID != ""; restricted == warned {
		t.Fatalf("B2 test: key restricted to a bucket: %t, warning: %t (%q)", restricted, warned, tr.Warnings)
	}
	b2 := func(b *bunkarrC, engine, name, sub string, sources []int64, extra map[string]any) map[string]any {
		body := map[string]any{"name": name, "kind": "b2", "engine": engine, "remote": map[string]any{"bucket": bucket, "prefix": prefix + "/" + sub},
			"credentials": creds, "sourceIds": sources, "settings": map[string]any{"verify": map[string]any{"mode": "full", "samplePercent": 100}}}
		if engine == "rclone" {
			body["encryption"] = map[string]any{"mode": "crypt"}
		}
		for k, v := range extra {
			body[k] = v
		}
		return body
	}
	for _, engine := range []string{"restic", "rclone"} {
		t.Logf("== %s on B2", engine)
		dir := "b2-" + engine
		o.writeRandom(map[string]int64{dir + "/Movies/Changed.mkv": lcChanged, dir + "/Movies/Deleted.nfo": lcDeleted,
			dir + "/Movies/Renamed.mkv": lcRenamed})
		src := b.createSource("B2 "+engine, dir)
		d, _ := b.createOffsite(b2(b, engine, "B2 "+engine, engine, []int64{src.ID}, nil))
		requireStatus(t, o.untouchedTree(dir, func() apiJob { return b.sync(d.ID, nil) }), "completed")
		o.toolsSh(`cd "$1" && head -c 1052672 /dev/urandom > Movies/Changed.mkv && head -c 1572864 /dev/urandom > Movies/Added.mkv && rm Movies/Deleted.nfo && mv Movies/Renamed.mkv "Movies/Renamed (2003).mkv"`,
			offsiteMedia+"/"+dir)
		j := o.untouchedTree(dir, func() apiJob { return b.sync(d.ID, nil) })
		requireStatus(t, j, "completed")
		if st := decodeStats[engineSync](t, j); st.FilesCopied != 1 || st.FilesUpdated != 1 || st.FilesMoved != 1 || st.FilesRetained != 1 {
			t.Fatalf("%s on B2: the change set: %s", engine, j.Stats)
		}
		requireVerified(t, b.verify(d.ID))
		body := map[string]any{}
		if engine == "restic" {
			body["prune"] = true
		}
		r := b.retention(d.ID, body)
		if r.Status != "completed" && r.Status != "completed_with_warnings" {
			requireStatus(t, r, "completed")
		}
		if engine == "rclone" && !decodeStats[retentionStatsE2E](t, r).Pruned && !strings.Contains(string(r.Stats), `"cleanup":true`) {
			t.Fatalf("rclone on B2: the first retention ran no backend cleanup: %s", r.Stats)
		}
	}
	b.waitIdle(10 * time.Minute)
	if hidden := api.hiddenVersions(t, bucketID, prefix+"/"); len(hidden) > 0 {
		t.Fatalf("hidden file versions under %s/ (deletes must be hard deletes): %v", prefix, hidden)
	}
	if left := api.unfinishedLarge(t, bucketID, prefix+"/"); len(left) > 0 {
		t.Fatalf("unfinished large files under %s/: %v", prefix, left)
	}
	if left := api.unfinishedLarge(t, bucketID, prefix+"-other/"); len(left) != 1 || left[0] != other {
		t.Fatalf("the unfinished large file under another prefix was touched: %v (want %s)", left, other)
	}
	o.auditAll()
}

// b2API is the native B2 API as the test uses it (the key only in the Authorization header).
type b2API struct {
	AccountID       string
	Token           string
	APIURL          string
	AllowedBucketID string
}

// b2Authorize calls b2_authorize_account (v2).
func b2Authorize(t *testing.T, keyID, key string) *b2API {
	t.Helper()
	req, err := http.NewRequest("GET", "https://api.backblazeb2.com/b2api/v2/b2_authorize_account", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.SetBasicAuth(keyID, key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var a struct {
		AccountID          string `json:"accountId"`
		AuthorizationToken string `json:"authorizationToken"`
		APIURL             string `json:"apiUrl"`
		Allowed            struct {
			BucketID *string `json:"bucketId"`
		} `json:"allowed"`
	}
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&a) != nil {
		t.Fatalf("b2_authorize_account: HTTP %d", res.StatusCode)
	}
	out := &b2API{AccountID: a.AccountID, Token: a.AuthorizationToken, APIURL: a.APIURL}
	if a.Allowed.BucketID != nil {
		out.AllowedBucketID = *a.Allowed.BucketID
	}
	return out
}

// call posts body to a B2 API operation and decodes the answer into out.
func (a *b2API) call(t *testing.T, op string, body, out any) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", a.APIURL+"/b2api/v2/"+op, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", a.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != 200 {
		t.Fatalf("%s: HTTP %d: %s", op, res.StatusCode, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}
}

// bucketID returns the id of bucket name.
func (a *b2API) bucketID(t *testing.T, name string) string {
	t.Helper()
	if a.AllowedBucketID != "" {
		return a.AllowedBucketID
	}
	var out struct {
		Buckets []struct {
			BucketID string `json:"bucketId"`
		} `json:"buckets"`
	}
	a.call(t, "b2_list_buckets", map[string]any{"accountId": a.AccountID, "bucketName": name}, &out)
	if len(out.Buckets) != 1 {
		t.Fatalf("bucket %s not found", name)
	}
	return out.Buckets[0].BucketID
}

// startLargeFile starts a large file that is never finished and returns its id.
func (a *b2API) startLargeFile(t *testing.T, bucketID, name string) string {
	t.Helper()
	var out struct {
		FileID string `json:"fileId"`
	}
	a.call(t, "b2_start_large_file", map[string]any{"bucketId": bucketID, "fileName": name, "contentType": "b2/x-auto"}, &out)
	return out.FileID
}

// b2Version is one file version of b2_list_file_versions.
type b2Version struct {
	FileName string `json:"fileName"`
	FileID   string `json:"fileId"`
	Action   string `json:"action"`
}

// versions lists every file version under prefix.
func (a *b2API) versions(t *testing.T, bucketID, prefix string) []b2Version {
	t.Helper()
	var all []b2Version
	body := map[string]any{"bucketId": bucketID, "prefix": prefix, "maxFileCount": 1000}
	for {
		var out struct {
			Files        []b2Version `json:"files"`
			NextFileName *string     `json:"nextFileName"`
			NextFileID   *string     `json:"nextFileId"`
		}
		a.call(t, "b2_list_file_versions", body, &out)
		all = append(all, out.Files...)
		if out.NextFileName == nil {
			return all
		}
		body["startFileName"], body["startFileId"] = *out.NextFileName, out.NextFileID
	}
}

// hiddenVersions returns the hide markers under prefix.
func (a *b2API) hiddenVersions(t *testing.T, bucketID, prefix string) []string {
	var out []string
	for _, v := range a.versions(t, bucketID, prefix) {
		if v.Action == "hide" {
			out = append(out, v.FileName)
		}
	}
	return out
}

// unfinishedLarge returns the ids of the unfinished large files under prefix.
func (a *b2API) unfinishedLarge(t *testing.T, bucketID, prefix string) []string {
	t.Helper()
	var out struct {
		Files []struct {
			FileID string `json:"fileId"`
		} `json:"files"`
	}
	a.call(t, "b2_list_unfinished_large_files", map[string]any{"bucketId": bucketID, "namePrefix": prefix, "maxFileCount": 100}, &out)
	var ids []string
	for _, f := range out.Files {
		ids = append(ids, f.FileID)
	}
	return ids
}

// cleanPrefix removes every file version and unfinished large file the test left under prefix and
// prefix-other.
func (a *b2API) cleanPrefix(t *testing.T, bucketID, prefix string) {
	for _, p := range []string{prefix + "/", prefix + "-other/"} {
		for _, id := range a.unfinishedLarge(t, bucketID, p) {
			a.call(t, "b2_cancel_large_file", map[string]any{"fileId": id}, nil)
		}
		for _, v := range a.versions(t, bucketID, p) {
			a.call(t, "b2_delete_file_version", map[string]any{"fileName": v.FileName, "fileId": v.FileID}, nil)
		}
	}
	t.Logf("removed the test's files under %s/ and %s-other/ (%s)", prefix, prefix, fmt.Sprint(time.Now().Format(time.TimeOnly)))
}
