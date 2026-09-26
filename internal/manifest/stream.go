package manifest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"reflect"
	"strings"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// A manifest of a large library is hundreds of megabytes of JSON. It is written the way
// encoding/json would write it, byte for byte, but its top-level lists one element at a time, so
// neither manifest.json nor the canonical form ContentHash hashes is ever held in memory whole.

// jsonStyle is how streamJSON encodes: like WriteJSON (indented by two spaces, HTML characters
// as they are) or like json.Marshal (compact, HTML characters escaped).
type jsonStyle struct {
	indent     bool
	escapeHTML bool
}

var (
	styleFile  = jsonStyle{indent: true}
	styleCanon = jsonStyle{escapeHTML: true}
)

// streamJSON writes m as encoding/json encodes it in style st (without a trailing newline). Only
// the top level is written by hand: each field's value, and each element of a top-level list, is
// encoded by encoding/json.
func streamJSON(w io.Writer, m *Manifest, st jsonStyle) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(st.escapeHTML)
	// encode returns v's encoding at nesting depth depth (valid until the next call).
	encode := func(v any, depth int) ([]byte, error) {
		buf.Reset()
		if st.indent {
			enc.SetIndent(strings.Repeat("  ", depth), "  ")
		}
		if err := enc.Encode(v); err != nil {
			return nil, err
		}
		return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
	}
	put := func(parts ...[]byte) error {
		for _, p := range parts {
			if _, err := w.Write(p); err != nil {
				return err
			}
		}
		return nil
	}
	nl1, nl2, colon := []byte{}, []byte{}, []byte(":")
	if st.indent {
		nl1, nl2, colon = []byte("\n  "), []byte("\n    "), []byte(": ")
	}
	rv := reflect.ValueOf(m).Elem()
	rt := rv.Type()
	if err := put([]byte("{")); err != nil {
		return err
	}
	n := 0
	for i := range rt.NumField() {
		sf := rt.Field(i)
		if !sf.IsExported() || sf.Anonymous {
			return fmt.Errorf("manifest: cannot stream field %s", sf.Name)
		}
		name, opts, _ := strings.Cut(sf.Tag.Get("json"), ",")
		if name == "" || name == "-" || (opts != "" && opts != "omitempty") {
			return fmt.Errorf("manifest: cannot stream field %s with json tag %q", sf.Name, sf.Tag.Get("json"))
		}
		fv := rv.Field(i)
		if opts == "omitempty" && isEmptyJSON(fv) {
			continue
		}
		key, err := encode(name, 1)
		if err != nil {
			return err
		}
		sep := []byte(",")
		if n == 0 {
			sep = nil
		}
		n++
		if err := put(sep, nl1, key, colon); err != nil {
			return err
		}
		// Values are encoded through pointers (encoding/json writes a pointer as what it points
		// to), so no element is copied into an interface.
		if fv.Kind() != reflect.Slice || fv.IsNil() || fv.Len() == 0 {
			b, err := encode(fv.Addr().Interface(), 1)
			if err != nil {
				return err
			}
			if err := put(b); err != nil {
				return err
			}
			continue
		}
		if err := put([]byte("[")); err != nil {
			return err
		}
		for j := range fv.Len() {
			b, err := encode(fv.Index(j).Addr().Interface(), 2)
			if err != nil {
				return err
			}
			sep := []byte(",")
			if j == 0 {
				sep = nil
			}
			if err := put(sep, nl2, b); err != nil {
				return err
			}
		}
		if err := put(nl1, []byte("]")); err != nil {
			return err
		}
	}
	end := []byte("}")
	if st.indent && n > 0 {
		end = []byte("\n}")
	}
	return put(end)
}

// isEmptyJSON is encoding/json's omitempty test.
func isEmptyJSON(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool:
		return !v.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return v.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return v.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return v.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return v.IsNil()
	}
	return false
}

// encodedSize returns the number of bytes fill writes (nothing is kept).
func encodedSize(fill func(io.Writer) error) (int64, error) {
	cw := &countWriter{w: io.Discard}
	bw := bufio.NewWriterSize(cw, 64<<10)
	if err := fill(bw); err != nil {
		return 0, err
	}
	if err := bw.Flush(); err != nil {
		return 0, err
	}
	return cw.n, nil
}

// writeFileStream writes the file rel in root as filecopy.WriteFileAtomic does, with the data
// streamed from fill instead of held in memory: parent directories are created, a new temp file
// next to rel is written and fsynced, then renamed to rel (never replacing it) and the directory
// is fsynced. It returns the lowercase hex sha256 of what it wrote. On an error the temp file is
// removed.
func writeFileStream(root *os.Root, rel string, perm fs.FileMode, fill func(io.Writer) error) (sum string, err error) {
	dir, base := path.Split(rel)
	dir = strings.TrimSuffix(dir, "/")
	if dir == "" || base == "" {
		return "", fmt.Errorf("write %s: not a file in a directory", rel)
	}
	if err := root.MkdirAll(dir, filecopy.DefaultDirPerm); err != nil {
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	var rnd [6]byte
	_, _ = rand.Read(rnd[:]) // crypto/rand.Read never fails (it aborts the program instead)
	tempRel := dir + "/" + filecopy.TempPrefix + base + "-" + hex.EncodeToString(rnd[:])
	f, err := root.OpenFile(tempRel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = f.Close()
		}
		if err != nil {
			_ = root.Remove(tempRel)
		}
	}()
	h := sha256.New()
	bw := bufio.NewWriterSize(io.MultiWriter(f, h), 64<<10)
	if err = fill(bw); err != nil {
		return "", err
	}
	if err = bw.Flush(); err != nil {
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	if err = f.Sync(); err != nil {
		return "", fmt.Errorf("write %s: fsync: %w", rel, err)
	}
	closed = true
	if err = f.Close(); err != nil {
		return "", fmt.Errorf("write %s: %w", rel, err)
	}
	if err = filecopy.Commit(root, tempRel, rel, true); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
