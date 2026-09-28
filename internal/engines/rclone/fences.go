package rclone

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// The fences of safety rules S2 and S23 (docs/design/phase4.md): every remote path is the
// destination root plus a relative path checked here, never a string a remote returned; Bunkarr
// writes, moves and deletes only inside a destFolder and .bunkarr/; delete and deletefile take
// only files under .bunkarr/retention/<run>/, purge only a version directory its injected
// validator accepts, and every check runs before the command is built.

// ErrFence means a path is outside what a command may touch (S2, S23): a programming error or
// a hostile value, and nothing ran.
var ErrFence = errors.New("path outside the destination's fences")

// runNameRe matches a job retention directory's name (filecopy.RetentionDir).
var runNameRe = regexp.MustCompile(`^\d{8}T\d{6}Z-job\d+$`)

// checkRel checks a clean relative path: not empty, no leading or trailing "/", no "", "." or
// ".." component, no NUL.
func checkRel(rel string) error {
	switch {
	case rel == "":
		return fmt.Errorf("%w: empty path", ErrFence)
	case strings.ContainsRune(rel, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", ErrFence, rel)
	case strings.HasPrefix(rel, "/") || path.Clean(rel) != rel || rel == "." || rel == ".." || strings.HasPrefix(rel, "../"):
		return fmt.Errorf("%w: %q is not a clean relative path", ErrFence, rel)
	}
	return nil
}

// CheckFileListName checks a relative path that goes into a --files-from-raw list: a clean
// relative path without a line break and not ending in "\r" (rclone reads the list line by line
// and drops a final "\r"). A name that fails cannot be transferred by rclone file lists; its item
// fails with the error.
func CheckFileListName(rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	if strings.ContainsRune(rel, '\n') || strings.HasSuffix(rel, "\r") {
		return fmt.Errorf("%w: %q contains a line break (not supported by rclone file lists)", ErrFence, rel)
	}
	return nil
}

// CheckArgName checks a relative path that is named on the command line (moveto, copyto,
// deletefile, cat): a clean relative path without control characters (the exec layer's
// allow-list refuses them in arguments).
func CheckArgName(rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	if strings.ContainsFunc(rel, unicode.IsControl) {
		return fmt.Errorf("%w: %q contains a control character (cannot be named on rclone's command line)", ErrFence, rel)
	}
	return nil
}

// CheckDestFolder checks a source's destination folder: a clean relative path outside .bunkarr/.
func CheckDestFolder(folder string) error {
	if err := CheckArgName(folder); err != nil {
		return err
	}
	if inMeta(folder) {
		return fmt.Errorf("%w: destination folder %q is inside %s", ErrFence, folder, filecopy.MetaDir)
	}
	return nil
}

// inMeta reports whether rel is .bunkarr or inside it.
func inMeta(rel string) bool {
	return rel == filecopy.MetaDir || strings.HasPrefix(rel, filecopy.MetaDir+"/")
}

// CheckRetentionDir checks a job retention directory, ".bunkarr/retention/<run>" as
// filecopy.RetentionDir makes it.
func CheckRetentionDir(dir string) error {
	run, ok := strings.CutPrefix(dir, filecopy.RetentionRoot+"/")
	if !ok || !runNameRe.MatchString(run) {
		return fmt.Errorf("%w: %q is not a job retention directory (%s/<time>-job<id>)", ErrFence, dir, filecopy.RetentionRoot)
	}
	return nil
}

// CheckRetentionFile checks a retained file: ".bunkarr/retention/<run>/<path>" with a job
// retention directory and a non-empty path. Only such files reach delete and deletefile (S23).
func CheckRetentionFile(rel string) error {
	if err := checkRel(rel); err != nil {
		return err
	}
	rest, ok := strings.CutPrefix(rel, filecopy.RetentionRoot+"/")
	run, file, found := strings.Cut(rest, "/")
	if !ok || !found || file == "" || !runNameRe.MatchString(run) {
		return fmt.Errorf("%w: %q is not a file under %s/<run>/", ErrFence, rel, filecopy.RetentionRoot)
	}
	return nil
}

// checkWritable checks a path Bunkarr may write to directly with rcat: inside .bunkarr/ but not
// in retention (the marker, links.tsv).
func checkWritable(rel string) error {
	if err := CheckArgName(rel); err != nil {
		return err
	}
	if !strings.HasPrefix(rel, filecopy.MetaDir+"/") || strings.HasPrefix(rel+"/", filecopy.RetentionRoot+"/") {
		return fmt.Errorf("%w: rcat writes only Bunkarr's own files in %s (not %q)", ErrFence, filecopy.MetaDir, rel)
	}
	return nil
}

// checkVersionDir checks a path purge may remove: a directory under .bunkarr/ outside retention
// that isVersionDir calls a version directory (snapshots.Layout.SplitVersionPath, wired by the
// caller). A nil validator refuses everything.
func checkVersionDir(rel string, isVersionDir func(string) bool) error {
	if err := CheckArgName(rel); err != nil {
		return err
	}
	if !strings.HasPrefix(rel, filecopy.MetaDir+"/") || strings.HasPrefix(rel+"/", filecopy.RetentionRoot+"/") {
		return fmt.Errorf("%w: %q is not a config version directory", ErrFence, rel)
	}
	if isVersionDir == nil || !isVersionDir(rel) {
		return fmt.Errorf("%w: %q is not a version directory of its kind", ErrFence, rel)
	}
	return nil
}

// fileList renders a --files-from-raw list after checking every name.
func fileList(rels []string) ([]byte, error) {
	if len(rels) == 0 {
		return nil, fmt.Errorf("%w: an empty file list", ErrFence)
	}
	var b strings.Builder
	for _, r := range rels {
		if err := CheckFileListName(r); err != nil {
			return nil, err
		}
		b.WriteString(r)
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}
