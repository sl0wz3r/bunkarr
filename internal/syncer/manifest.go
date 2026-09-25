package syncer

import (
	"bytes"
	"context"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/destinations"
	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// LinkManifestRel is the hardlink manifest of a destination (relative to its target): every
// hardlinked name Bunkarr recorded there and the name that holds its content. A name recorded
// only (link_recorded: the destination keeps one copy) has no file at the destination, and the
// relationship is otherwise only in Bunkarr's database; the manifest lets a restore without that
// database recreate such names (ln or cp) and the hardlinks.
const LinkManifestRel = filecopy.MetaDir + "/links.tsv"

// linkManifestHeader explains the format to whoever restores without Bunkarr.
const linkManifestHeader = `# Bunkarr hardlink manifest (format 1), rewritten after every sync.
# Each line: <name> TAB <primary> TAB <state>; paths are relative to this destination's target.
# link_recorded: <name> has no file here; it has the content of <primary> (recreate it with
#   ln <primary> <name>, or cp where hardlinks are not possible).
# linked: <name> is a hardlink of <primary>.
# In paths, a backslash, tab, newline or carriage return is written as \\, \t, \n or \r, and a
#   # that starts a path as \#: only comment lines start with #.
`

var manifestEscaper = strings.NewReplacer(`\`, `\\`, "\t", `\t`, "\n", `\n`, "\r", `\r`)

// manifestPath escapes a path for the manifest (a leading '#' too: a destination folder may start
// with one, and the line would read as a comment).
func manifestPath(p string) string {
	p = manifestEscaper.Replace(p)
	if strings.HasPrefix(p, "#") {
		p = `\` + p
	}
	return p
}

// writeLinkManifest writes LinkManifestRel at the end of a sync (not a dry run) through the job's
// root: a temp file renamed over the old manifest. An unchanged manifest is not rewritten.
func writeLinkManifest(ctx context.Context, store *Store, h *destinations.Handle) error {
	links, err := store.links(context.WithoutCancel(ctx), h.Destination.ID)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	b.WriteString(linkManifestHeader)
	for _, l := range links {
		b.WriteString(manifestPath(l.name))
		b.WriteByte('\t')
		b.WriteString(manifestPath(l.primary))
		b.WriteByte('\t')
		b.WriteString(string(l.state))
		b.WriteByte('\n')
	}
	if old, err := h.Root.ReadFile(LinkManifestRel); err == nil && bytes.Equal(old, b.Bytes()) {
		return nil
	}
	return filecopy.WriteFileAtomic(h.Root, LinkManifestRel, b.Bytes(), 0, false)
}
