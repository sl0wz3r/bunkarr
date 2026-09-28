package restic

import (
	"maps"
	"path"
	"slices"
	"strings"
)

// CompressIncludes returns a batch's include list (docs/design/phase4.md §6.2 step 4): the
// absolute paths restic backs up with --files-from-raw. live holds the relative path
// (slash-separated, as the catalog records it) of every live catalog file of the source, and
// include says which of them are in the include set I_k. A directory D is listed whole when every
// live catalog file under D is included; otherwise its included files and its whole
// subdirectories are listed, recursively. When every live file is included (no tier rules, no
// held or failed item) the list is the source root alone; when none is, it is empty. Paths are
// sourceRoot joined with the relative path, byte for byte (names with newlines, "*", "[" or "\"
// are kept as they are: the list is NUL-separated), in a stable order.
func CompressIncludes(sourceRoot string, live []string, include func(rel string) bool) []string {
	root := &includeDir{}
	for _, rel := range live {
		if rel == "" {
			continue
		}
		d := root
		parts := strings.Split(rel, "/")
		for _, p := range parts[:len(parts)-1] {
			d = d.dir(p)
		}
		in := include(rel)
		d.files = append(d.files, includeFile{name: parts[len(parts)-1], in: in})
	}
	root.count()
	if root.included == 0 {
		return nil
	}
	var out []string
	clean := path.Clean(sourceRoot)
	prefix := clean
	if clean == "/" {
		prefix = ""
	}
	root.emit(prefix, clean, &out)
	return out
}

type includeFile struct {
	name string
	in   bool
}

type includeDir struct {
	files []includeFile
	dirs  map[string]*includeDir
	// total and included count the live files under the directory.
	total, included int
}

func (d *includeDir) dir(name string) *includeDir {
	if d.dirs == nil {
		d.dirs = map[string]*includeDir{}
	}
	sub := d.dirs[name]
	if sub == nil {
		sub = &includeDir{}
		d.dirs[name] = sub
	}
	return sub
}

func (d *includeDir) count() {
	for _, f := range d.files {
		d.total++
		if f.in {
			d.included++
		}
	}
	for _, sub := range d.dirs {
		sub.count()
		d.total += sub.total
		d.included += sub.included
	}
}

// emit appends the list for d, whose path is prefix ("" for "/" as the source root) and whose
// own path as listed is self.
func (d *includeDir) emit(prefix, self string, out *[]string) {
	if d.included == d.total {
		*out = append(*out, self)
		return
	}
	join := func(name string) string {
		if prefix == "" {
			return "/" + name
		}
		return prefix + "/" + name
	}
	files := slices.Clone(d.files)
	slices.SortFunc(files, func(a, b includeFile) int { return strings.Compare(a.name, b.name) })
	for _, f := range files {
		if f.in {
			*out = append(*out, join(f.name))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.dirs)) {
		sub := d.dirs[name]
		if sub.included > 0 {
			p := join(name)
			sub.emit(p, p, out)
		}
	}
}
