package destinations

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/db"
	"github.com/sl0wz3r/bunkarr/internal/engines"
	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
	"github.com/sl0wz3r/bunkarr/internal/engines/restic"
	"github.com/sl0wz3r/bunkarr/internal/version"
)

// The recovery kit (phase4.md §5.2, §5.3, S21): everything needed to list and restore a restic or
// rclone destination without Bunkarr's database, generated from the sealed secret on each export
// (never stored, logged or uploaded). Every shell value is single-quoted by shellQuote, and the
// rclone.conf and known_hosts lines are rendered by the engines' builders from validated values
// (which refuse CR and LF).

// Kit is one export of a recovery kit: its attachment file name and its text.
type Kit struct {
	Filename string
	Content  string
}

// KitConfirmation confirms a destination's recovery kit custody (§5.2): the kit's check code, or,
// for a secret the user typed at create, that secret typed again. It never shows the secret.
type KitConfirmation struct {
	CheckCode string `json:"checkCode"`
	Secret    string `json:"secret"`
}

// String implements fmt.Stringer without the secret.
func (c KitConfirmation) String() string {
	return fmt.Sprintf("destinations.KitConfirmation{checkCode set: %t, secret set: %t}", c.CheckCode != "", c.Secret != "")
}

// GoString implements fmt.GoStringer without the secret.
func (c KitConfirmation) GoString() string { return c.String() }

// LogValue implements slog.LogValuer without the code or the secret.
func (c KitConfirmation) LogValue() slog.Value {
	return slog.GroupValue(slog.Bool("checkCodeSet", c.CheckCode != ""), slog.Bool("secretSet", c.Secret != ""))
}

// MarshalJSON implements json.Marshaler without the code or the secret.
func (c KitConfirmation) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]bool{"checkCodeSet": c.CheckCode != "", "secretSet": c.Secret != ""})
}

// Kit markers: the shell block to paste, and the JSON block at the end.
const (
	kitShellBegin = "----- BEGIN SHELL (paste into a POSIX shell, in an empty directory) -----"
	kitShellEnd   = "----- END SHELL -----"
	kitJSONBegin  = "-----BEGIN BUNKARR RECOVERY KIT JSON-----"
	kitJSONEnd    = "-----END BUNKARR RECOVERY KIT JSON-----"
	// kitHeredoc ends the heredocs that write rclone.conf, known_hosts and ca.pem; the quoted
	// delimiter turns off every expansion, and no rendered line equals it.
	kitHeredoc = "BUNKARR_EOF"
	// Files the shell block writes in the current directory.
	kitPasswordFile   = "./bunkarr_restic_password"
	kitKnownHostsFile = "./bunkarr_known_hosts"
	kitCACertFile     = "./bunkarr_ca.pem"
	kitRcloneConf     = "./rclone.conf"
	// kitReplace starts a placeholder for a storage credential the kit leaves out.
	kitReplace = "REPLACE_WITH_"
)

// shellQuote single-quotes s for a POSIX shell (§5.2): each single quote inside is closed,
// escaped with a backslash and reopened. It is the one quoting helper of every value in the
// kit's shell lines.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// CheckCode returns the check code of a secret for destination id (§5.2): the first 8 characters
// of base32(sha256("bunkarr-kit-v1\x00" + id + "\x00" + secret)), as XXXX-XXXX. It is bound to
// the row's id, which never changes (AUTOINCREMENT, never reused), and not to its marker_id: a kit
// exported while the create is pending ("pending:<uuid>") must still confirm once the create has
// finished and the marker_id became "restic:<id>" or the rclone marker's id.
func CheckCode(id int64, secret string) string {
	sum := sha256.Sum256([]byte("bunkarr-kit-v1\x00" + strconv.FormatInt(id, 10) + "\x00" + secret))
	code := base32.StdEncoding.EncodeToString(sum[:])[:8]
	return code[:4] + "-" + code[4:]
}

// normalizeCode drops dashes and spaces and upper-cases a typed check code.
func normalizeCode(c string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(c))
}

var slugRun = regexp.MustCompile(`[^a-z0-9]+`)

// kitFilename is bunkarr-recovery-<slug>-<yyyymmdd>.txt, the slug [a-z0-9-]{1,40} from the name.
func kitFilename(name string, id int64, at time.Time) string {
	slug := strings.Trim(slugRun.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" {
		slug = "destination-" + strconv.FormatInt(id, 10)
	}
	return "bunkarr-recovery-" + slug + "-" + at.UTC().Format("20060102") + ".txt"
}

// kitSource is a source linked to the destination, for the kit's layout.
type kitSource struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Path       string `json:"path"`
	DestFolder string `json:"destFolder"`
}

// RecoveryKit renders destination id's recovery kit and records the export (kit_exported_at).
// The storage credentials are included only when includeStorageCredentials is set; otherwise the
// kit says to create a new key, and its shell lines hold REPLACE_WITH_ placeholders. It is the
// only output that contains the encryption secret; the API gates it (session plus password,
// notification) and never logs it.
func (s *Store) RecoveryKit(ctx context.Context, id int64, includeStorageCredentials bool) (Kit, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return Kit{}, err
	}
	if !d.IsEngine() {
		return Kit{}, ErrNotEngine
	}
	ed, sec, err := s.SecretsFor(ctx, id)
	if err != nil {
		return Kit{}, err
	}
	sources, err := s.kitSources(ctx, id)
	if err != nil {
		return Kit{}, err
	}
	now := s.now()
	content, err := renderKit(kitData{d: d, ed: ed, sec: sec, sources: sources, includeCreds: includeStorageCredentials, now: now})
	if err != nil {
		return Kit{}, err
	}
	err = s.db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE destinations SET kit_exported_at = ? WHERE id = ?`, db.FormatTime(now), id)
		return err
	})
	if err != nil {
		return Kit{}, fmt.Errorf("record the kit export of destination %d: %w", id, err)
	}
	return Kit{Filename: kitFilename(d.Name, d.ID, now), Content: content}, nil
}

// kitSources returns the sources linked to destination id.
func (s *Store) kitSources(ctx context.Context, id int64) ([]kitSource, error) {
	rows, err := s.db.Reader().QueryContext(ctx, `SELECT s.id, s.name, s.path, s.dest_folder FROM sources s
		JOIN destination_sources ds ON ds.source_id = s.id WHERE ds.destination_id = ? ORDER BY s.id`, id)
	if err != nil {
		return nil, fmt.Errorf("read the sources of destination %d: %w", id, err)
	}
	defer rows.Close()
	out := []kitSource{}
	for rows.Next() {
		var k kitSource
		if err := rows.Scan(&k.ID, &k.Name, &k.Path, &k.DestFolder); err != nil {
			return nil, fmt.Errorf("read the sources of destination %d: %w", id, err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ConfirmKit confirms destination id's recovery kit custody (§5.2) and sets kit_confirmed_at: by
// the kit's check code (case, spaces and dashes ignored; ErrWrongCheckCode), or, for a secret the
// user typed at create (secret_origin user) without a crypt password2, by that secret typed again
// (ErrWrongSecret). Both compare in constant time. The password check, the rate limit and the
// session belong to the API.
func (s *Store) ConfirmKit(ctx context.Context, id int64, c KitConfirmation) error {
	d, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if !d.IsEngine() || !d.HasSecret() {
		return ValidationError("this destination has no encryption secret to confirm")
	}
	_, sec, err := s.SecretsFor(ctx, id)
	if err != nil {
		return err
	}
	switch {
	case c.Secret != "" && c.CheckCode != "":
		return ValidationError("confirm with the check code or with the secret, not both")
	case c.Secret != "":
		if d.Encryption.Origin != OriginUser {
			return ValidationError("a generated secret is confirmed with the check code of its recovery kit")
		}
		if sec.Encryption.CryptPassword2 != "" {
			// Two crypt passwords were typed (encryption.secret2): re-entering the first would not
			// show that the second was kept, and a restore needs both. The kit holds both.
			return ValidationError("this crypt remote has a password2 as well: confirm with the check code of its recovery kit, which holds both")
		}
		want := sec.Encryption.ResticPassword
		if want == "" {
			want = sec.Encryption.CryptPassword
		}
		if subtle.ConstantTimeCompare([]byte(c.Secret), []byte(want)) != 1 {
			return ErrWrongSecret
		}
	case c.CheckCode != "":
		want := normalizeCode(CheckCode(d.ID, secretText(sec.Encryption)))
		if subtle.ConstantTimeCompare([]byte(normalizeCode(c.CheckCode)), []byte(want)) != 1 {
			return ErrWrongCheckCode
		}
	default:
		return ValidationError("a checkCode (from the recovery kit) or the secret is required")
	}
	now := db.FormatTime(s.now())
	return s.db.Write(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE destinations SET kit_confirmed_at = ?, updated_at = ? WHERE id = ?`, now, now, id)
		if err != nil {
			return fmt.Errorf("confirm the recovery kit of destination %d: %w", id, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// kitData is what renderKit needs.
type kitData struct {
	d            Destination
	ed           engines.Destination
	sec          engines.Secrets
	sources      []kitSource
	includeCreds bool
	now          time.Time
}

// kitJSON is the kit's JSON block: the same data as the text.
type kitJSON struct {
	Format        string            `json:"format"`
	Name          string            `json:"name"`
	ID            int64             `json:"id"`
	Engine        string            `json:"engine"`
	Kind          engines.DestKind  `json:"kind"`
	Target        string            `json:"target"`
	Remote        engines.Remote    `json:"remote"`
	MarkerID      string            `json:"markerId"`
	EngineTag     string            `json:"engineTag,omitempty"`
	Bunkarr       string            `json:"bunkarrVersion"`
	CreatedAt     time.Time         `json:"createdAt"`
	ExportedAt    time.Time         `json:"exportedAt"`
	CheckCode     string            `json:"checkCode,omitempty"`
	Encryption    kitEncryption     `json:"encryption"`
	Storage       map[string]string `json:"storageCredentials"`
	KnownHosts    string            `json:"knownHosts,omitempty"`
	Sources       []kitSource       `json:"sources"`
	RcloneConf    string            `json:"rcloneConf,omitempty"`
	ResticEnv     map[string]string `json:"resticEnvironment,omitempty"`
	LinksManifest string            `json:"linksManifest,omitempty"`
}

type kitEncryption struct {
	Mode           engines.EncryptionMode `json:"mode"`
	Origin         string                 `json:"origin,omitempty"`
	ResticPassword string                 `json:"resticPassword,omitempty"`
	CryptPassword  string                 `json:"cryptPassword,omitempty"`
	CryptPassword2 string                 `json:"cryptPassword2,omitempty"`
	// CryptObscured are the obscured forms rclone.conf carries.
	CryptObscured map[string]string `json:"cryptObscured,omitempty"`
}

// kitSecrets returns the secrets the kit's shell lines and rclone.conf use: the stored ones, or,
// without includeCreds, placeholders for the storage credentials of the kind the destination
// uses (so the lines stay complete and say what to replace).
func kitSecrets(k kitData) engines.Secrets {
	sec := engines.Secrets{Encryption: k.sec.Encryption, Obscured: map[string]string{}}
	for _, f := range []string{engines.FieldCryptPassword, engines.FieldCryptPassword2} {
		if v := k.sec.Obscured[f]; v != "" {
			sec.Obscured[f] = v
		}
	}
	if k.includeCreds {
		sec.Credentials = k.sec.Credentials
		maps.Copy(sec.Obscured, k.sec.Obscured)
		return sec
	}
	c := k.sec.Credentials
	switch k.d.Kind {
	case engines.S3:
		sec.Credentials = engines.Credentials{AccessKeyID: kitReplace + "ACCESS_KEY_ID", SecretAccessKey: kitReplace + "SECRET_ACCESS_KEY"}
	case engines.B2:
		sec.Credentials = engines.Credentials{KeyID: kitReplace + "KEY_ID", ApplicationKey: kitReplace + "APPLICATION_KEY"}
	case engines.SFTP:
		if c.PrivateKey != "" {
			sec.Credentials.PrivateKey = kitReplace + "PRIVATE_KEY_PEM"
			if c.PrivateKeyPassphrase != "" {
				sec.Credentials.PrivateKeyPassphrase = kitReplace + "PASSPHRASE"
				sec.Obscured[engines.FieldPrivateKeyPassphrase] = kitReplace + "OBSCURED_PASSPHRASE"
			}
		} else {
			sec.Credentials.Password = kitReplace + "PASSWORD"
			sec.Obscured[engines.FieldPassword] = kitReplace + "OBSCURED_PASSWORD"
		}
	}
	return sec
}

// renderKit renders the kit's text (§5.2): what the destination is, the secret and its check
// code, the storage credentials or how to replace them, the layout, the restore steps without
// Bunkarr (restic export lines, or a complete rclone.conf; the pinned known_hosts for SFTP), the
// hardlinks, a warning, and the same data as JSON at the end.
func renderKit(k kitData) (string, error) {
	d := k.d
	kitSec := kitSecrets(k)
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("BUNKARR RECOVERY KIT")
	line("====================")
	line("")
	line("WARNING: whoever holds this kit (and access to the storage) can read the whole backup.")
	line("Keep it offline and private, for example printed and stored with your other important papers.")
	line("")
	line("Destination:     %s (id %d)", d.Name, d.ID)
	line("Engine and kind: %s on %s", d.Engine, d.Kind)
	line("Location:        %s", d.Target)
	line("Created:         %s", d.CreatedAt.UTC().Format(time.RFC3339))
	line("Exported:        %s", k.now.UTC().Format(time.RFC3339))
	line("Bunkarr version: %s", version.Version)
	line("")
	data := kitJSON{Format: "bunkarr-recovery-kit-1", Name: d.Name, ID: d.ID, Engine: d.Engine, Kind: d.Kind, Target: d.Target, Remote: d.Remote,
		MarkerID: d.MarkerID, EngineTag: d.EngineTag, Bunkarr: version.Version, CreatedAt: d.CreatedAt.UTC(), ExportedAt: k.now.UTC(),
		Encryption: kitEncryption{Mode: d.Encryption.Mode, Origin: d.Encryption.Origin}, Sources: k.sources}

	// The check code.
	if d.HasSecret() {
		data.CheckCode = CheckCode(d.ID, secretText(k.sec.Encryption))
		line("CHECK CODE: %s", data.CheckCode)
		line("Type this code in Bunkarr (destination card, \"Confirm recovery kit\") to confirm that you stored this kit.")
		line("Until then, Bunkarr runs no backup to this destination.")
		line("")
	}

	// The location.
	line("LOCATION")
	switch {
	case d.Remote.SFTP != nil:
		r := d.Remote.SFTP
		line("  SFTP host: %s", r.Host)
		line("  port:      %d", r.Port)
		line("  user:      %s", r.User)
		line("  path:      %s", r.Path)
		for _, hk := range r.HostKeys {
			fp, err := rclone.Fingerprint(hk)
			if err != nil {
				return "", fmt.Errorf("recovery kit: %w", err)
			}
			line("  pinned host key: %s %s", hk.Type, fp)
		}
	case d.Remote.S3 != nil:
		r := d.Remote.S3
		ep := r.Endpoint
		if ep == "" {
			ep = "https://" + awsHost(r.Region) + " (AWS)"
		}
		line("  S3 provider:   %s", r.Provider)
		line("  endpoint:      %s", ep)
		line("  region:        %s", r.Region)
		line("  bucket:        %s", r.Bucket)
		line("  prefix:        %s", r.Prefix)
		if r.StorageClass != "" {
			line("  storage class: %s", r.StorageClass)
		}
		if r.CACert != "" {
			line("  the endpoint uses a self-signed certificate: the shell block writes it to %s", kitCACertFile)
		}
	case d.Remote.B2 != nil:
		line("  Backblaze B2 bucket: %s", d.Remote.B2.Bucket)
		line("  prefix:              %s", d.Remote.B2.Prefix)
	default:
		line("  directory: %s", d.Target)
	}
	line("")

	// The secret.
	line("ENCRYPTION SECRET")
	switch d.Encryption.Mode {
	case engines.EncryptionRestic:
		data.Encryption.ResticPassword = k.sec.Encryption.ResticPassword
		line("  restic repository password: %s", k.sec.Encryption.ResticPassword)
	case engines.EncryptionCrypt:
		data.Encryption.CryptPassword = k.sec.Encryption.CryptPassword
		data.Encryption.CryptPassword2 = k.sec.Encryption.CryptPassword2
		data.Encryption.CryptObscured = map[string]string{}
		line("  rclone crypt password:  %s", k.sec.Encryption.CryptPassword)
		line("    obscured (rclone.conf): %s", k.sec.Obscured[engines.FieldCryptPassword])
		data.Encryption.CryptObscured[engines.FieldCryptPassword] = k.sec.Obscured[engines.FieldCryptPassword]
		if k.sec.Encryption.CryptPassword2 != "" {
			line("  rclone crypt password2: %s", k.sec.Encryption.CryptPassword2)
			line("    obscured (rclone.conf): %s", k.sec.Obscured[engines.FieldCryptPassword2])
			data.Encryption.CryptObscured[engines.FieldCryptPassword2] = k.sec.Obscured[engines.FieldCryptPassword2]
		} else {
			line("  rclone crypt password2: (none: rclone's default salt)")
		}
	default:
		line("  none: the files are stored unencrypted")
	}
	line("")

	// The storage credentials.
	line("STORAGE CREDENTIALS")
	data.Storage = map[string]string{}
	if k.includeCreds {
		c := k.sec.Credentials
		for _, f := range [][2]string{{engines.FieldAccessKeyID, c.AccessKeyID}, {engines.FieldSecretAccessKey, c.SecretAccessKey},
			{engines.FieldKeyID, c.KeyID}, {engines.FieldApplicationKey, c.ApplicationKey}, {engines.FieldPassword, c.Password},
			{engines.FieldPrivateKeyPassphrase, c.PrivateKeyPassphrase}, {engines.FieldPrivateKey, c.PrivateKey}} {
			if f[1] == "" {
				continue
			}
			data.Storage[f[0]] = f[1]
			if f[0] == engines.FieldPrivateKey {
				line("  %s:", f[0])
				for _, l := range strings.Split(strings.TrimSpace(f[1]), "\n") {
					line("    %s", strings.TrimRight(l, "\r"))
				}
				continue
			}
			line("  %s: %s", f[0], f[1])
		}
	} else {
		data.Storage = nil
		switch d.Kind {
		case engines.S3, engines.B2:
			bucket := ""
			if d.Remote.S3 != nil {
				bucket = d.Remote.S3.Bucket
			} else if d.Remote.B2 != nil {
				bucket = d.Remote.B2.Bucket
			}
			line("  Not included. Create a new application key for bucket %s in your provider's console,", bucket)
			line("  and put it where the lines below say %s….", kitReplace)
		case engines.SFTP:
			line("  Not included. Use your SSH key or password for %s@%s, and put it where the lines below say %s….",
				d.Remote.SFTP.User, d.Remote.SFTP.Host, kitReplace)
			line("  An rclone password is given obscured: rclone obscure '<password>' prints that form.")
		default:
			line("  none (a local directory)")
		}
	}
	line("")

	// The layout.
	line("LAYOUT")
	if len(k.sources) > 0 {
		line("  Sources backed up here:")
		for _, src := range k.sources {
			line("    source %d %q: %s (destination folder %s)", src.ID, src.Name, src.Path, src.DestFolder)
		}
	}
	if d.Engine == EngineRestic {
		line("  One restic repository. Every snapshot Bunkarr makes has --host bunkarr and the tags:")
		line("    bunkarr, bunkarr-dest:%s, bunkarr-kind:<media|plexdb|arr|manifest>, bunkarr-job:<job id>;", d.EngineTag)
		line("    media: bunkarr-source:<source id> and bunkarr-batch:<n>; each holds the source's files under its path;")
		line("    Plex DB and *arr backups: bunkarr-integration:<id> and bunkarr-version:<version>;")
		line("    manifests: bunkarr-version:<version>.")
		line("  Snapshots of other bunkarr-dest tags belong to other Bunkarr destinations (or earlier installs).")
		line("  restic restores hardlinks within one restore by itself.")
	} else {
		line("  Inside the destination root (the crypt remote %s when encrypted):", rclone.ConfCrypt)
		line("    <destination folder>/<path>  the live copy of each source")
		line("    .bunkarr/destination.json    the marker of this destination")
		line("    .bunkarr/links.tsv           the hardlinks: each line is <name> TAB <primary> TAB <state>;")
		line("                                 recreate a name with ln <primary> <name> (or cp)")
		line("    .bunkarr/retention/<run>/    deleted and replaced versions, kept for the retention period")
		line("    .bunkarr/plex/, .bunkarr/arr/, .bunkarr/manifests/  Plex DB, *arr and manifest versions")
		data.LinksManifest = ".bunkarr/links.tsv"
	}
	line("")

	// The restore steps.
	shell, err := kitShell(k, kitSec, &data)
	if err != nil {
		return "", err
	}
	line("RESTORE WITHOUT BUNKARR")
	line("  In an empty directory, with restic and rclone installed, paste this block into a POSIX shell:")
	line("")
	line(kitShellBegin)
	b.WriteString(shell)
	line(kitShellEnd)
	line("")
	if d.Engine == EngineRestic {
		line("  Then list the backups and restore:")
		line("    restic snapshots --tag %s", shellQuote("bunkarr-dest:"+d.EngineTag))
		line("    restic restore <snapshot>:<source path> --target <directory>")
		line("    restic dump <snapshot> <file path> > <file>")
	} else {
		root := rclone.ConfDest + ":"
		if d.Encryption.Mode == engines.EncryptionCrypt {
			root = rclone.ConfCrypt + ":"
		} else if d.Remote.S3 != nil || d.Remote.B2 != nil || d.Remote.SFTP != nil {
			root = strings.Replace(rclone.StorageRoot(k.ed), "BKDEST", rclone.ConfDest, 1)
		}
		line("  Then list the backups and restore:")
		line("    rclone lsd %s", shellQuote(root))
		folder := "<destination folder>"
		if len(k.sources) == 1 {
			folder = k.sources[0].DestFolder
		}
		src := strings.TrimSuffix(root, "/") + "/" + folder
		if strings.HasSuffix(root, ":") {
			src = root + folder
		}
		line("    rclone copy %s <directory>", shellQuote(src))
		if d.Remote.S3 != nil && d.Remote.S3.CACert != "" {
			line("  (add --ca-cert %s to every rclone command)", kitCACertFile)
		}
	}
	line("")

	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return "", fmt.Errorf("recovery kit: %w", err)
	}
	line(kitJSONBegin)
	b.Write(raw)
	line("")
	line(kitJSONEnd)
	return b.String(), nil
}

// kitShell renders the shell block: the secret files (with umask 077), the known_hosts and CA
// heredocs, and restic's export lines or rclone's rclone.conf. Every value is shellQuote'd.
func kitShell(k kitData, sec engines.Secrets, data *kitJSON) (string, error) {
	var b strings.Builder
	d := k.d
	b.WriteString("umask 077\n")
	if r := d.Remote.SFTP; r != nil {
		kh, err := rclone.KnownHosts(r.Host, r.Port, r.HostKeys)
		if err != nil {
			return "", fmt.Errorf("recovery kit: %w", err)
		}
		data.KnownHosts = kh
		fmt.Fprintf(&b, "cat > %s <<'%s'\n%s%s\n", shellQuote(kitKnownHostsFile), kitHeredoc, kh, kitHeredoc)
	}
	caPath := ""
	if r := d.Remote.S3; r != nil && r.CACert != "" {
		caPath = kitCACertFile
		fmt.Fprintf(&b, "cat > %s <<'%s'\n%s%s\n", shellQuote(kitCACertFile), kitHeredoc, r.CACert, kitHeredoc)
	}
	if d.Engine == EngineRestic {
		env, err := restic.Env(k.ed, sec, "/bunkarr-kit/password", "/bunkarr-kit/cache", "", kitKnownHostsFile, caPath)
		if err != nil {
			return "", fmt.Errorf("recovery kit: %w", err)
		}
		delete(env, "RESTIC_CACHE_DIR")
		delete(env, "RESTIC_PROGRESS_FPS")
		env["RESTIC_PASSWORD_FILE"] = kitPasswordFile
		data.ResticEnv = env
		fmt.Fprintf(&b, "printf '%%s' %s > %s\n", shellQuote(k.sec.Encryption.ResticPassword), shellQuote(kitPasswordFile))
		keys := slices.Sorted(maps.Keys(env))
		slices.SortStableFunc(keys, func(a, c string) int {
			// RESTIC_REPOSITORY first, then the rest by name.
			switch {
			case a == "RESTIC_REPOSITORY":
				return -1
			case c == "RESTIC_REPOSITORY":
				return 1
			}
			return 0
		})
		for _, key := range keys {
			if strings.ContainsAny(env[key], "\r\n\x00") {
				return "", fmt.Errorf("recovery kit: %s contains a line break", key)
			}
			fmt.Fprintf(&b, "export %s=%s\n", key, shellQuote(env[key]))
		}
		return b.String(), nil
	}
	conf, err := rclone.ConfFile(k.ed, sec, kitKnownHostsFile)
	if err != nil {
		return "", fmt.Errorf("recovery kit: %w", err)
	}
	for _, l := range strings.Split(conf, "\n") {
		if l == kitHeredoc {
			return "", errors.New("recovery kit: a value ends a heredoc")
		}
	}
	data.RcloneConf = conf
	fmt.Fprintf(&b, "cat > %s <<'%s'\n%s%s\n", shellQuote(kitRcloneConf), kitHeredoc, conf, kitHeredoc)
	fmt.Fprintf(&b, "export RCLONE_CONFIG=%s\n", shellQuote(kitRcloneConf))
	return b.String(), nil
}
