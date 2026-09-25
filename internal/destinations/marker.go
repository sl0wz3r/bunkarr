package destinations

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
	"time"

	"github.com/sl0wz3r/bunkarr/internal/engines/filecopy"
)

// maxMarkerBytes bounds how much of a marker file is read.
const maxMarkerBytes = 64 << 10

// Marker is the content of <target>/.bunkarr/destination.json (safety rule S3).
type Marker struct {
	// ID is a random UUID, stored as destinations.marker_id.
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
}

// errMarkerInvalid means a marker file exists but is not a valid marker.
var errMarkerInvalid = errors.New("marker file is not a valid Bunkarr marker")

// readMarker reads the marker through root. present is false when there is no marker file;
// an unreadable or invalid one returns present true and an error.
func readMarker(root *os.Root) (m Marker, present bool, err error) {
	fi, err := root.Lstat(filecopy.MarkerRel)
	if errors.Is(err, fs.ErrNotExist) {
		return Marker{}, false, nil
	}
	if errors.Is(err, syscall.ENOTDIR) {
		return Marker{}, true, fmt.Errorf("%w: %s is not a directory", errMarkerInvalid, filecopy.MetaDir)
	}
	if err != nil {
		return Marker{}, false, fmt.Errorf("read %s: %w", filecopy.MarkerRel, err)
	}
	if !fi.Mode().IsRegular() {
		return Marker{}, true, fmt.Errorf("%w: %s is not a regular file", errMarkerInvalid, filecopy.MarkerRel)
	}
	f, err := root.Open(filecopy.MarkerRel)
	if err != nil {
		return Marker{}, true, fmt.Errorf("read %s: %w", filecopy.MarkerRel, err)
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes))
	if err != nil {
		return Marker{}, true, fmt.Errorf("read %s: %w", filecopy.MarkerRel, err)
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.ID == "" {
		return Marker{}, true, fmt.Errorf("%w (%s)", errMarkerInvalid, filecopy.MarkerRel)
	}
	return m, true, nil
}

// writeMarker writes the marker atomically; it never replaces an existing marker.
func writeMarker(root *os.Root, m Marker) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode marker: %w", err)
	}
	data = append(data, '\n')
	if err := filecopy.WriteFileAtomic(root, filecopy.MarkerRel, data, 0o644, true); err != nil {
		if errors.Is(err, filecopy.ErrExists) {
			return fmt.Errorf("%w (a marker appeared while creating)", ErrMarkerExists)
		}
		return fmt.Errorf("write marker: %w", err)
	}
	return nil
}

// removeMarkerIfOurs removes the marker when it still carries id (undoing a create whose row
// could not be inserted).
func removeMarkerIfOurs(root *os.Root, id string) error {
	m, present, err := readMarker(root)
	if err != nil || !present || m.ID != id {
		return err
	}
	return root.Remove(filecopy.MarkerRel)
}

// newUUID returns a random (version 4) UUID.
func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (it aborts the program instead)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
