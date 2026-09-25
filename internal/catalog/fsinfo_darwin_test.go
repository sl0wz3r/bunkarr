//go:build darwin

package catalog

import "testing"

func TestAnonymousDevDarwin(t *testing.T) {
	for _, tc := range []struct {
		fsType string
		want   bool
	}{
		{"apfs", false},
		{"hfs", false},
		{"msdos", false},
		{"nfs", true},
		{"smbfs", true},
		{"afpfs", true},
		{"webdav", true},
		{"macfuse", true},
		{"osxfuse", true},
	} {
		if got := anonymousDev(tc.fsType, 0x1000010); got != tc.want {
			t.Errorf("anonymousDev(%s) = %v, want %v", tc.fsType, got, tc.want)
		}
	}
}
