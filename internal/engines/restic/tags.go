package restic

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/engines"
)

// Snapshot tags and groups (docs/design/phase4.md §6.1, D32, S24). Every snapshot Bunkarr makes
// has --host bunkarr and the tags bunkarr, bunkarr-dest:<engine_tag>, bunkarr-kind:<kind> and
// bunkarr-job:<jobId>; media snapshots add bunkarr-source:<sourceId> and bunkarr-batch:<n>,
// Plex DB and *arr versions bunkarr-integration:<id> and bunkarr-version:<name>, manifests
// bunkarr-version:<name>. A group is (engine_tag, media, source), (engine_tag, plexdb or arr,
// integration) or (engine_tag, manifest). A snapshot whose Bunkarr tags do not parse into
// exactly one group of this destination row belongs to none and is never forgotten.

// Host is --host of every snapshot Bunkarr makes.
const Host = "bunkarr"

// Tag prefixes.
const (
	TagBunkarr        = "bunkarr"
	TagDestPrefix     = "bunkarr-dest:"
	TagKindPrefix     = "bunkarr-kind:"
	TagJobPrefix      = "bunkarr-job:"
	TagSourcePrefix   = "bunkarr-source:"
	TagBatchPrefix    = "bunkarr-batch:"
	TagIntegrationPfx = "bunkarr-integration:"
	TagVersionPrefix  = "bunkarr-version:"
)

var (
	engineTagRe   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	versionNameRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)
)

// DestTag is the tag every snapshot of the destination row with engineTag carries, and that
// every tag filter includes.
func DestTag(engineTag string) string { return TagDestPrefix + engineTag }

// KindTag is the tag of a snapshot kind (media, plexdb, arr, manifest).
func KindTag(kind string) string { return TagKindPrefix + kind }

// JobTag is the tag of the job that made a snapshot.
func JobTag(jobID int64) string { return TagJobPrefix + strconv.FormatInt(jobID, 10) }

// BatchTag is the tag of a media snapshot's batch.
func BatchTag(batch int) string { return TagBatchPrefix + strconv.Itoa(batch) }

// SourceTag is the tag of a media snapshot's source.
func SourceTag(sourceID int64) string { return TagSourcePrefix + strconv.FormatInt(sourceID, 10) }

// IntegrationTag is the tag of a Plex DB or *arr version's integration.
func IntegrationTag(id int64) string { return TagIntegrationPfx + strconv.FormatInt(id, 10) }

// VersionTag is the tag of a config version's name.
func VersionTag(name string) string { return TagVersionPrefix + name }

// ValidEngineTag reports whether tag is an engine_tag (16 random bytes in hex, D32).
func ValidEngineTag(tag string) bool { return engineTagRe.MatchString(tag) }

// validKind reports a snapshot kind.
func validKind(k string) bool {
	switch k {
	case engines.VersionMedia, engines.VersionPlexDB, engines.VersionArr, engines.VersionManifest:
		return true
	}
	return false
}

// TagInput is what a snapshot is tagged with.
type TagInput struct {
	EngineTag string
	// Kind is media, plexdb, arr or manifest.
	Kind  string
	JobID int64
	// SourceID and Batch: media.
	SourceID int64
	Batch    int
	// IntegrationID: plexdb and arr. Version: plexdb, arr and manifest (the version's name).
	IntegrationID int64
	Version       string
}

// Tags returns the tags of a snapshot (§6.1), in a fixed order.
func Tags(in TagInput) ([]string, error) {
	if !ValidEngineTag(in.EngineTag) {
		return nil, fmt.Errorf("restic tags: %q is not an engine tag", in.EngineTag)
	}
	if !validKind(in.Kind) {
		return nil, fmt.Errorf("restic tags: unknown kind %q", in.Kind)
	}
	if in.JobID <= 0 {
		return nil, fmt.Errorf("restic tags: job id %d", in.JobID)
	}
	tags := []string{TagBunkarr, DestTag(in.EngineTag), KindTag(in.Kind), JobTag(in.JobID)}
	switch in.Kind {
	case engines.VersionMedia:
		if in.SourceID <= 0 || in.Batch <= 0 {
			return nil, fmt.Errorf("restic tags: a media snapshot needs a source (%d) and a batch (%d)", in.SourceID, in.Batch)
		}
		return append(tags, SourceTag(in.SourceID), BatchTag(in.Batch)), nil
	case engines.VersionPlexDB, engines.VersionArr:
		if in.IntegrationID <= 0 {
			return nil, fmt.Errorf("restic tags: a %s version needs an integration", in.Kind)
		}
		tags = append(tags, IntegrationTag(in.IntegrationID))
	}
	if !versionNameRe.MatchString(in.Version) {
		return nil, fmt.Errorf("restic tags: version name %q", in.Version)
	}
	return append(tags, VersionTag(in.Version)), nil
}

// Group is a snapshot group of one destination row (§6.1): the kind and the source (media) or
// integration (plexdb, arr); a manifest group has neither.
type Group struct {
	Kind          string
	SourceID      int64
	IntegrationID int64
}

// TagInfo is everything a snapshot's Bunkarr tags say.
type TagInfo struct {
	Group
	JobID   int64
	Batch   int
	Version string
}

// ParseTags parses a snapshot's tags for the destination row with engineTag. ok is true only
// when the Bunkarr tags form exactly one group of this row: the bunkarr tag, one
// bunkarr-dest:<engineTag>, one known kind, one job, and exactly the tags of that kind, each
// once and well-formed. Any other bunkarr-* tag, a duplicate, a missing tag or another
// engine_tag gives false, and such a snapshot is never forgotten (S24). Tags without the
// bunkarr prefix (a user's own) are ignored.
func ParseTags(tags []string, engineTag string) (TagInfo, bool) {
	var info TagInfo
	if !ValidEngineTag(engineTag) {
		return info, false
	}
	seen := map[string]int{}
	values := map[string]string{}
	for _, t := range tags {
		if t != TagBunkarr && !strings.HasPrefix(t, TagBunkarr+"-") {
			continue
		}
		prefix := t
		value := ""
		if i := strings.IndexByte(t, ':'); i >= 0 {
			prefix, value = t[:i+1], t[i+1:]
		}
		switch prefix {
		case TagBunkarr, TagDestPrefix, TagKindPrefix, TagJobPrefix, TagSourcePrefix, TagBatchPrefix, TagIntegrationPfx, TagVersionPrefix:
		default:
			return info, false
		}
		seen[prefix]++
		values[prefix] = value
	}
	for _, n := range seen {
		if n != 1 {
			return info, false
		}
	}
	if seen[TagBunkarr] != 1 || values[TagDestPrefix] != engineTag || seen[TagKindPrefix] != 1 || seen[TagJobPrefix] != 1 {
		return info, false
	}
	info.Kind = values[TagKindPrefix]
	if !validKind(info.Kind) {
		return info, false
	}
	var ok bool
	if info.JobID, ok = positive(values[TagJobPrefix]); !ok {
		return info, false
	}
	want := map[string]bool{}
	switch info.Kind {
	case engines.VersionMedia:
		want = map[string]bool{TagSourcePrefix: true, TagBatchPrefix: true}
	case engines.VersionPlexDB, engines.VersionArr:
		want = map[string]bool{TagIntegrationPfx: true, TagVersionPrefix: true}
	case engines.VersionManifest:
		want = map[string]bool{TagVersionPrefix: true}
	}
	for _, p := range []string{TagSourcePrefix, TagBatchPrefix, TagIntegrationPfx, TagVersionPrefix} {
		if (seen[p] == 1) != want[p] {
			return info, false
		}
	}
	if want[TagSourcePrefix] {
		if info.SourceID, ok = positive(values[TagSourcePrefix]); !ok {
			return info, false
		}
		b, ok := positive(values[TagBatchPrefix])
		if !ok || b > 1<<30 {
			return info, false
		}
		info.Batch = int(b)
	}
	if want[TagIntegrationPfx] {
		if info.IntegrationID, ok = positive(values[TagIntegrationPfx]); !ok {
			return info, false
		}
	}
	if want[TagVersionPrefix] {
		info.Version = values[TagVersionPrefix]
		if !versionNameRe.MatchString(info.Version) {
			return info, false
		}
	}
	return info, true
}

// ParseGroup returns the group of a snapshot's tags for the destination row with engineTag
// (ParseTags).
func ParseGroup(tags []string, engineTag string) (Group, bool) {
	info, ok := ParseTags(tags, engineTag)
	return info.Group, ok
}

// positive parses a decimal id > 0 without a sign or leading zeros.
func positive(s string) (int64, bool) {
	if s == "" || s[0] == '0' || s[0] == '+' || s[0] == '-' {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n > 0
}
