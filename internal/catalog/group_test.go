package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanGroups(t *testing.T) {
	m := Meta{Size: 10, MtimeNs: 100, CtimeNs: 200, Dev: 1, Inode: 5, Nlink: 2}
	with := func(f func(*Meta)) Meta { c := m; f(&c); return c }
	for _, tc := range []struct {
		name     string
		cands    map[devIno][]linkCand
		groups   [][]string
		warnings []string // substrings, in order
	}{
		{
			name:   "two names of one inode",
			cands:  map[devIno][]linkCand{{1, 5}: {{"b", m}, {"a", m}}},
			groups: [][]string{{"a", "b"}},
		},
		{
			name:  "one name inside the source",
			cands: map[devIno][]linkCand{{1, 5}: {{"a", m}}},
		},
		{
			name: "members disagree on ctime",
			cands: map[devIno][]linkCand{{1, 5}: {
				{"a", m}, {"b", with(func(x *Meta) { x.CtimeNs++ })},
			}},
			warnings: []string{"changed during the scan"},
		},
		{
			name: "members disagree on size",
			cands: map[devIno][]linkCand{{1, 5}: {
				{"a", m}, {"b", with(func(x *Meta) { x.Size = 11 })},
			}},
			warnings: []string{"changed during the scan"},
		},
		{
			name: "more names than links (inode reuse)",
			cands: map[devIno][]linkCand{{1, 5}: {
				{"a", m}, {"b", m}, {"c", m},
			}},
			warnings: []string{"link count is 2"},
		},
		{
			name: "several groups ordered by first path",
			cands: map[devIno][]linkCand{
				{1, 7}: {{"z/2", with(func(x *Meta) { x.Inode = 7 })}, {"c/1", with(func(x *Meta) { x.Inode = 7 })}},
				{1, 5}: {{"d", m}, {"b", m}},
				{2, 5}: {{"x", with(func(x *Meta) { x.Dev = 2; x.Nlink = 3 })}, {"y", with(func(x *Meta) { x.Dev = 2; x.Nlink = 3 })}},
			},
			groups: [][]string{{"b", "d"}, {"c/1", "z/2"}, {"x", "y"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			groups, warnings := planGroups(tc.cands)
			if len(groups) != len(tc.groups) {
				t.Fatalf("groups = %v, want %v", groups, tc.groups)
			}
			for i, g := range groups {
				var rels []string
				for _, c := range g {
					rels = append(rels, c.rel)
				}
				if strings.Join(rels, ",") != strings.Join(tc.groups[i], ",") {
					t.Fatalf("group %d = %v, want %v", i, rels, tc.groups[i])
				}
			}
			if len(warnings) != len(tc.warnings) {
				t.Fatalf("warnings = %v, want %v", warnings, tc.warnings)
			}
			for i, w := range tc.warnings {
				if !strings.Contains(warnings[i], w) {
					t.Fatalf("warning %q does not mention %q", warnings[i], w)
				}
			}
		})
	}
}

func TestHeadTailHash(t *testing.T) {
	dir := tempDir(t)
	big := make([]byte, 3<<20+123)
	for i := range big {
		big[i] = byte(i * 7)
	}
	if err := os.WriteFile(filepath.Join(dir, "big"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFiles(t, dir, map[string]string{"small": "hello", "empty": ""})
	if err := os.Symlink("small", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	meta := func(name string) Meta {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		m, _ := MetaOf(fi)
		return m
	}
	sum := func(parts ...[]byte) string {
		h := sha256.New()
		for _, p := range parts {
			h.Write(p)
		}
		return hex.EncodeToString(h.Sum(nil))
	}
	for _, tc := range []struct{ name, want string }{
		{"big", sum(big[:1<<20], big[len(big)-1<<20:])},
		{"small", sum([]byte("hello"))},
		{"empty", sum()},
	} {
		got, err := headTailHash(root, tc.name, meta(tc.name))
		if err != nil || got != tc.want {
			t.Fatalf("%s: %s, %v; want %s", tc.name, got, err, tc.want)
		}
	}
	// The file must still be what the walk saw: a symlink resolves to another inode.
	if _, err := headTailHash(root, "link", meta("link")); !errors.Is(err, errChangedWhileHashing) {
		t.Fatalf("hash through a symlink = %v", err)
	}
	stale := meta("small")
	stale.MtimeNs--
	if _, err := headTailHash(root, "small", stale); !errors.Is(err, errChangedWhileHashing) {
		t.Fatalf("hash of a changed file = %v", err)
	}
	if _, err := headTailHash(root, "../outside", meta("small")); err == nil {
		t.Fatal("hash escaped the root")
	}
}
