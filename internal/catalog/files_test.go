package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFilesPagingSearchFilter(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	sc := NewScanner(st, ScannerOptions{})
	root := tempDir(t)
	files := map[string]string{
		"100% real.mkv":  "x",
		"a_b.mkv":        "x",
		"axb.mkv":        "x",
		`back\slash.mkv`: "x",
		"Upper/CASE.mkv": "x",
		"gone.mkv":       "x",
	}
	for i := range 10 {
		files[fmt.Sprintf("series/e%02d.mkv", i)] = "e"
	}
	writeFiles(t, root, files)
	if err := os.Link(filepath.Join(root, "axb.mkv"), filepath.Join(root, "axb-link.mkv")); err != nil {
		t.Fatal(err)
	}
	src := createSource(t, st, "S", root)
	mustScan(t, sc, src.ID)
	if err := os.Remove(filepath.Join(root, "gone.mkv")); err != nil {
		t.Fatal(err)
	}
	mustScan(t, sc, src.ID)

	paths := func(p FilePage) []string {
		out := []string{}
		for _, f := range p.Records {
			out = append(out, f.RelPath)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		q     Query
		total int64
		want  []string
	}{
		{"literal underscore", Query{Search: "_"}, 1, []string{"a_b.mkv"}},
		{"literal percent", Query{Search: "%"}, 1, []string{"100% real.mkv"}},
		{"literal backslash", Query{Search: `\`}, 1, []string{`back\slash.mkv`}},
		{"case-insensitive", Query{Search: "case"}, 1, []string{"Upper/CASE.mkv"}},
		{"hardlinked", Query{Filter: FilterHardlinked}, 2, []string{"axb-link.mkv", "axb.mkv"}},
		{"deleted", Query{Filter: FilterDeleted}, 1, []string{"gone.mkv"}},
		{"first page", Query{PageSize: 4}, 16, []string{"100% real.mkv", "Upper/CASE.mkv", "a_b.mkv", "axb-link.mkv"}},
		{"last page", Query{Page: 4, PageSize: 5}, 16, []string{"series/e09.mkv"}},
		{"past the end", Query{Page: 9, PageSize: 5}, 16, []string{}},
		{"search and page", Query{Search: "series/", Page: 2, PageSize: 3}, 10, []string{"series/e03.mkv", "series/e04.mkv", "series/e05.mkv"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := st.Files(ctx, src.ID, tc.q)
			if err != nil {
				t.Fatal(err)
			}
			if p.TotalRecords != tc.total || !slices.Equal(paths(p), tc.want) {
				t.Fatalf("got %d %v, want %d %v", p.TotalRecords, paths(p), tc.total, tc.want)
			}
		})
	}
	p, err := st.Files(ctx, src.ID, Query{PageSize: 100000, Page: -1})
	if err != nil || p.PageSize != MaxPageSize || p.Page != 1 {
		t.Fatalf("clamped page = %+v, %v", p, err)
	}
	if p, _ := st.Files(ctx, src.ID, Query{}); p.PageSize != DefaultPageSize {
		t.Fatalf("default page size = %d", p.PageSize)
	}
	var ve *ValidationError
	if _, err := st.Files(ctx, src.ID, Query{Filter: "bogus"}); !errors.As(err, &ve) {
		t.Fatalf("bad filter = %v", err)
	}
	if _, err := st.Files(ctx, 999, Query{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing source = %v", err)
	}
}

func TestLiveIteratesInPathOrder(t *testing.T) {
	ctx := context.Background()
	st := newStore(t, StoreOptions{})
	root := tempDir(t)
	files := map[string]string{"a.txt": "x", "a/b.txt": "x", "a-b.txt": "x"}
	for i := range iterBatch + 7 {
		files[fmt.Sprintf("bulk/%04d.mkv", i)] = "x"
	}
	writeFiles(t, root, files)
	src := createSource(t, st, "S", root)
	mustScan(t, NewScanner(st, ScannerOptions{}), src.ID)

	var got []string
	if err := st.Live(ctx, src.ID, func(f File) error { got = append(got, f.RelPath); return nil }); err != nil {
		t.Fatal(err)
	}
	want := keys(files)
	slices.Sort(want) // byte order: "a-b.txt" < "a.txt" < "a/b.txt"
	if !slices.Equal(got, want) {
		t.Fatalf("Live returned %d paths, first %v; want %d, first %v", len(got), got[:3], len(want), want[:3])
	}
	stop := errors.New("stop")
	n := 0
	if err := st.Live(ctx, src.ID, func(File) error { n++; return stop }); !errors.Is(err, stop) || n != 1 {
		t.Fatalf("Live stop = %v after %d", err, n)
	}
}

func TestFileJSON(t *testing.T) {
	f := File{ID: 3, RelPath: "a/b.mkv", Size: 10, MtimeNs: 1_700_000_000_000_000_000, Nlink: 2, HardlinkGroup: "1.2:3", Dev: 9, Inode: 8}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":3,"relPath":"a/b.mkv","size":10,"mtime":"2023-11-14T22:13:20Z","hardlinkGroup":"1.2:3","nlink":2,"deleted":false}`
	if string(b) != want {
		t.Fatalf("JSON = %s\nwant   %s", b, want)
	}
	f.HardlinkGroup = ""
	b, _ = json.Marshal(FilePage{Records: []File{f}})
	if !strings.Contains(string(b), `"hardlinkGroup":null`) || !strings.Contains(string(b), `"totalRecords":0`) {
		t.Fatalf("page JSON = %s", b)
	}
}
