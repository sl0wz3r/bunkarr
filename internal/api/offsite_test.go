package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/jobs"
)

// checkS29 checks one request that S29 protects (phase4.md S29, acceptance 9): 403 "log in and
// confirm your password to send data off-site" for the API key and for the local bypass; 400
// "current password is incorrect" for a session with a wrong password, counted by the login limiter
// (five make the next login and the request itself a 429); no engine call before the password is
// right; then want with the right password. It returns the successful answer.
func (ee *engineEnv) checkS29(t *testing.T, c *http.Client, method, path string, body map[string]any, want int) []byte {
	t.Helper()
	calls := ee.engineCalls()
	if code, msg := ee.status(t, method, path, body); code != 403 || msg != msgOffsiteSession {
		t.Fatalf("%s %s with the API key: %d %q, want 403 %q", method, path, code, msg, msgOffsiteSession)
	}
	if code, msg := ee.localBypass(t, method, path, body); code != 403 || msg != msgOffsiteSession {
		t.Fatalf("%s %s through the local bypass: %d %q, want 403", method, path, code, msg)
	}
	if code, raw, _ := ee.sessionRaw(t, c, method, path, body); code != 400 || !strings.Contains(string(raw), msgWrongPassword) {
		t.Fatalf("%s %s from a session without a password: %d %s", method, path, code, raw)
	}
	for range 4 {
		if code, raw, _ := ee.sessionRaw(t, c, method, path, withPassword(body, "wrong horse")); code != 400 ||
			!strings.Contains(string(raw), msgWrongPassword) {
			t.Fatalf("%s %s with a wrong password: %d %s", method, path, code, raw)
		}
	}
	// Five failures: the login limiter blocks this client, for logins and for S29 alike.
	if code, _, _ := ee.do(t, nil, "POST", "/api/v1/auth/login", fmt.Sprintf(`{"username":%q,"password":%q}`, sessionUser, sessionPassword), nil); code != 429 {
		t.Fatalf("login after five wrong S29 passwords: %d, want 429 (not counted by the limiter?)", code)
	}
	if code, raw, _ := ee.sessionRaw(t, c, method, path, withPassword(body, sessionPassword)); code != 429 {
		t.Fatalf("%s %s while the client is blocked: %d %s", method, path, code, raw)
	}
	ee.auth.Limiter.Success(testLocalIP)
	if n := ee.engineCalls(); n != calls {
		t.Fatalf("%s %s: %d engine calls before the password was checked", method, path, n-calls)
	}
	return ee.sessionCall(t, c, want, method, path, withPassword(body, sessionPassword), nil)
}

// TestS29OffsiteRoutes checks every route S29 names, and that the API key keeps what S29 does not
// name (local destinations, renames, running jobs).
func TestS29OffsiteRoutes(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	src := ee.createSource(t, "Movies", ee.mkdir(t, "media/movies"))
	local := ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), []int64{src}, nil)

	// POST /destinations of an off-site kind.
	raw := ee.checkS29(t, c, "POST", "/destinations", s3Body("Offsite", "restic", "media", "main"), 201)
	s3ID := idOf(t, raw)
	// POST /destinations accepting no encryption.
	plain := s3Body("Plain", "rclone", "media", "plain")
	plain["encryption"] = map[string]any{"mode": "none", "acceptUnencrypted": true}
	plainID := idOf(t, ee.checkS29(t, c, "POST", "/destinations", plain, 201))

	// PUT /destinations/{id}: rotating the credentials (tested against the stored remote first).
	path := fmt.Sprintf("/destinations/%d", s3ID)
	var v destinationView
	raw = ee.checkS29(t, c, "PUT", path, map[string]any{"credentials": map[string]any{"secretAccessKey": testNewSecret}}, 200)
	if err := json.Unmarshal(raw, &v); err != nil || !v.HasCredentials["secretAccessKey"] || !v.HasCredentials["accessKeyId"] {
		t.Fatalf("after the rotation: %s", raw)
	}
	// Linking a source to an off-site destination.
	ee.checkS29(t, c, "PUT", path, map[string]any{"sourceIds": []int64{src}}, 200)
	// Pinning a CA certificate.
	remote := map[string]any{"provider": "Minio", "endpoint": "https://s3.example.com", "region": "us-east-1", "bucket": "media", "prefix": "main"}
	withCA := map[string]any{"caCert": caCertPEM(t)}
	for k, val := range remote {
		withCA[k] = val
	}
	ee.checkS29(t, c, "PUT", path, map[string]any{"remote": withCA}, 200)

	// SFTP: creating it and pinning new host keys.
	key1, _ := hostKeyPair(t)
	key2, _ := hostKeyPair(t)
	sftpRemote := func(keys ...engines.HostKey) map[string]any {
		return map[string]any{"host": "sftp.example.com", "port": 2222, "user": "backup", "path": "/srv/backup", "hostKeys": keys}
	}
	sftpBody := map[string]any{"name": "Box", "kind": "sftp", "engine": "rclone", "remote": sftpRemote(key1),
		"credentials": map[string]any{"privateKey": privateKeyPEM(t, "")}}
	sftpID := idOf(t, ee.checkS29(t, c, "POST", "/destinations", sftpBody, 201))
	ee.checkS29(t, c, "PUT", fmt.Sprintf("/destinations/%d", sftpID), map[string]any{"remote": sftpRemote(key1, key2)}, 200)

	// What S29 does not name stays open to the API key: the same host keys and sources again, a
	// rename, a local destination with its sources, a sync of it.
	ee.call(t, 200, "PUT", fmt.Sprintf("/destinations/%d", sftpID), map[string]any{"name": "Box 2", "remote": sftpRemote(key1, key2)}, nil)
	ee.call(t, 200, "PUT", path, map[string]any{"name": "Offsite 2", "sourceIds": []int64{src}, "remote": withCA}, nil)
	ee.call(t, 200, "PUT", path, map[string]any{"sourceIds": []int64{}}, nil)
	ee.createDestination(t, "NAS 2", ee.mkdir(t, "nas2"), []int64{src}, nil)
	var j jobs.Job
	ee.call(t, 202, "POST", fmt.Sprintf("/destinations/%d/sync", local), map[string]any{"dryRun": true}, &j)
	ee.waitJob(t, j.ID)

	// Integrations: a backup target added on an off-site destination, and acceptInsecureModes.
	plexBody := func(targets ...map[string]any) map[string]any {
		return map[string]any{"name": "Plex", "url": "http://127.0.0.1:9", "settings": map[string]any{"dataPath": "/plex",
			"backup": map[string]any{"targets": targets}}}
	}
	var created struct {
		ID int64 `json:"id"`
	}
	ee.call(t, 201, "POST", "/integrations", map[string]any{"type": "plex", "name": "Plex", "url": "http://127.0.0.1:9"}, &created)
	itPath := fmt.Sprintf("/integrations/%d", created.ID)
	localTarget := map[string]any{"destinationId": local, "cron": "0 6 * * *", "enabled": true}
	ee.call(t, 200, "PUT", itPath, plexBody(localTarget), nil)
	offsiteTarget := map[string]any{"destinationId": s3ID, "cron": "0 7 * * 0", "enabled": true}
	ee.checkS29(t, c, "PUT", itPath, plexBody(localTarget, offsiteTarget), 200)
	// Keeping the off-site target is not adding it.
	ee.call(t, 200, "PUT", itPath, plexBody(localTarget, offsiteTarget), nil)
	insecure := map[string]any{"destinationId": local, "cron": "0 6 * * *", "enabled": true, "acceptInsecureModes": true}
	ee.checkS29(t, c, "PUT", itPath, plexBody(insecure, offsiteTarget), 200)
	ee.call(t, 200, "PUT", itPath, plexBody(insecure, offsiteTarget), nil) // already set
	// POST /integrations with an off-site target; an unencrypted remote target is refused anyway.
	create := plexBody(offsiteTarget)
	create["type"] = "plex"
	create["name"] = "Plex 2"
	ee.checkS29(t, c, "POST", "/integrations", create, 201)
	code, msg := ee.status(t, "PUT", itPath, withPassword(plexBody(localTarget, map[string]any{"destinationId": plainID, "cron": "0 8 * * *", "enabled": true}), sessionPassword))
	if code != 403 {
		t.Fatalf("an unencrypted target with the API key: %d %q", code, msg)
	}
	code, raw, _ = ee.sessionRaw(t, c, "PUT", itPath, withPassword(plexBody(localTarget, map[string]any{"destinationId": plainID, "cron": "0 8 * * *",
		"enabled": true}), sessionPassword))
	if code != 400 || !strings.Contains(string(raw), "use an encrypted destination") {
		t.Fatalf("an unencrypted remote Plex target: %d %s", code, raw)
	}
}

// TestEngineDestinationsNeverShowSecrets: no response of the destination routes, the new routes
// or the status pages contains a storage credential, an encryption secret (clear, obscured or
// JSON-escaped) or a private key; only the recovery kit carries the encryption secret.
func TestEngineDestinationsNeverShowSecrets(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	src := ee.createSource(t, "Movies", ee.mkdir(t, "media/movies"))
	var responses []struct {
		what string
		raw  []byte
	}
	keep := func(what string, raw []byte) {
		responses = append(responses, struct {
			what string
			raw  []byte
		}{what, raw})
	}
	create := func(body map[string]any) int64 {
		raw := ee.sessionCall(t, c, 201, "POST", "/destinations", withPassword(body, sessionPassword), nil)
		keep("POST /destinations "+body["name"].(string), raw)
		return idOf(t, raw)
	}
	restic := s3Body("Restic S3", "restic", "media", "restic")
	restic["sourceIds"] = []int64{src}
	resticID := create(restic)
	crypt := s3Body("Crypt S3", "rclone", "media", "crypt")
	crypt["encryption"] = map[string]any{"mode": "crypt", "generate": false, "secret": testUserSecret}
	cryptID := create(crypt)
	key, _ := hostKeyPair(t)
	pemKey := privateKeyPEM(t, "passphrase-for-tests-1")
	sftpID := create(map[string]any{"name": "SFTP", "kind": "sftp", "engine": "restic",
		"remote":      map[string]any{"host": "sftp.example.com", "user": "backup", "path": "backups", "hostKeys": []engines.HostKey{key}},
		"credentials": map[string]any{"password": testSFTPPass}})
	pemID := create(map[string]any{"name": "SFTP key", "kind": "sftp", "engine": "rclone",
		"remote":      map[string]any{"host": "sftp2.example.com", "user": "backup", "path": "/b", "hostKeys": []engines.HostKey{key}},
		"credentials": map[string]any{"privateKey": pemKey, "privateKeyPassphrase": "passphrase-for-tests-1"}})
	ee.createDestination(t, "NAS", ee.mkdir(t, "nas"), nil, nil)

	var values []string
	for _, id := range []int64{resticID, cryptID, sftpID, pemID} {
		values = append(values, ee.secretValues(t, id)...)
	}
	values = append(values, testAccessKey, testSecretKey, testUserSecret, testSFTPPass, "passphrase-for-tests-1", testNewSecret)
	for _, line := range strings.Split(pemKey, "\n") {
		if len(line) >= 12 && !strings.HasPrefix(line, "-----") {
			values = append(values, line)
		}
	}

	get := func(method, path string, body any) {
		t.Helper()
		code, raw, _ := ee.sessionRaw(t, c, method, path, body)
		if code >= 300 {
			t.Fatalf("%s %s: %d %s", method, path, code, raw)
		}
		keep(method+" "+path, raw)
	}
	get("GET", "/destinations", nil)
	for _, id := range []int64{resticID, cryptID, sftpID, pemID} {
		p := fmt.Sprintf("/destinations/%d", id)
		get("GET", p, nil)
		get("PUT", p, map[string]any{"name": fmt.Sprintf("renamed %d", id)})
		get("POST", p+"/test", nil)
		get("GET", p+"/snapshots", nil)
		get("GET", p+"/manifests", nil)
	}
	get("PUT", fmt.Sprintf("/destinations/%d", resticID), withPassword(map[string]any{"credentials": map[string]any{"secretAccessKey": testNewSecret}}, sessionPassword))
	test := s3Body("new", "restic", "other", "x")
	delete(test, "name")
	get("POST", "/destinations/test", test)
	get("GET", "/schedules", nil)
	get("GET", "/system/status", nil)
	get("GET", "/settings/engines", nil)
	get("GET", "/jobs", nil)
	for what, r := range responses {
		noSecrets(t, fmt.Sprintf("%d %s", what, r.what), r.raw, values)
	}

	// The recovery kit is the one response with the encryption secret.
	code, raw, _ := ee.sessionRaw(t, c, "POST", fmt.Sprintf("/destinations/%d/recovery-kit", cryptID),
		map[string]any{"currentPassword": sessionPassword})
	if code != 200 || !strings.Contains(string(raw), testUserSecret) {
		t.Fatalf("recovery kit: %d, contains the secret: %v", code, strings.Contains(string(raw), testUserSecret))
	}
	if strings.Contains(string(raw), testSecretKey) || strings.Contains(string(raw), testNewSecret) {
		t.Fatal("the kit holds the storage credentials although includeStorageCredentials was not set")
	}
	var d destinationView
	ee.call(t, 200, "GET", fmt.Sprintf("/destinations/%d", resticID), nil, &d)
	if d.Kind != engines.S3 || d.Engine != destinations.EngineRestic || d.Encryption.Mode != engines.EncryptionRestic ||
		d.Encryption.Origin != destinations.OriginGenerated || d.EngineVersion != "0.18.1" || d.EngineState == nil ||
		d.RetentionSchedule == nil || d.RetentionSchedule.Cron != DefaultEngineRetentionCron || !d.RetentionSchedule.Enabled ||
		d.BlockedReason != destinations.BlockedKit || d.WaitingUntil != nil || !d.HasCredentials["accessKeyId"] {
		t.Fatalf("restic destination view: %+v", d)
	}
}

// TestS29RemovingThePinnedCA: the store replaces an S3 remote with the one sent, so a PUT whose
// remote leaves caCert out (or sets it to null) removes the pinned CA certificate. That changes
// what the engines trust (S29): the API key gets 403 and the CA stays; a session with the
// password may remove it.
func TestS29RemovingThePinnedCA(t *testing.T) {
	ee := newEngineEnv(t, engineEnvOptions{})
	c := ee.session(t)
	body := s3Body("Offsite", "restic", "media", "ca")
	ca := caCertPEM(t)
	remote := body["remote"].(map[string]any)
	withCA := map[string]any{"caCert": ca}
	for k, v := range remote {
		withCA[k] = v
	}
	body["remote"] = withCA
	id := ee.createEngineDest(t, c, body)
	path := fmt.Sprintf("/destinations/%d", id)
	storedCA := func() string {
		d, err := ee.app.Destinations.Get(t.Context(), id)
		if err != nil || d.Remote.S3 == nil {
			t.Fatalf("destination %d: %+v %v", id, d, err)
		}
		return d.Remote.S3.CACert
	}
	if storedCA() == "" {
		t.Fatal("the CA certificate was not stored")
	}
	withNull := map[string]any{"caCert": nil}
	for k, v := range remote {
		withNull[k] = v
	}
	for name, r := range map[string]map[string]any{"without caCert": remote, "with caCert null": withNull} {
		if code, msg := ee.status(t, "PUT", path, map[string]any{"remote": r}); code != 403 || msg != msgOffsiteSession {
			t.Fatalf("PUT %s with the API key: %d %q, want 403 %q", name, code, msg, msgOffsiteSession)
		}
		if code, msg := ee.localBypass(t, "PUT", path, map[string]any{"remote": r}); code != 403 {
			t.Fatalf("PUT %s through the local bypass: %d %q, want 403", name, code, msg)
		}
		if storedCA() == "" {
			t.Fatalf("PUT %s with the API key removed the pinned CA", name)
		}
	}
	// The same remote, CA included, is no change of trust.
	ee.call(t, 200, "PUT", path, map[string]any{"name": "Offsite 2", "remote": withCA}, nil)
	// A session with the password may remove it.
	ee.sessionCall(t, c, 200, "PUT", path, withPassword(map[string]any{"remote": remote}, sessionPassword), nil)
	if got := storedCA(); got != "" {
		t.Fatalf("the CA after its removal with the password: %q", got)
	}
}
