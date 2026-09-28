package enginerun

import (
	"encoding/json"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines/rclone"
)

// TestUploadedBytes pins bytesUploaded to what rclone 1.74 reports for a copy with --backup-dir
// (phase4.md §20.4). The values are the lifecycle change set of TestDockerOffsiteLifecycle: a
// 1 MiB + 4 KiB update and a 1.5 MiB addition (2 625 536 bytes) plus 720 bytes of small objects,
// whose update displaced a 1 MiB old version into the retention directory.
func TestUploadedBytes(t *testing.T) {
	const uploads = 2625536 + 720
	tests := []struct {
		name  string
		stats string
		want  int64
	}{
		{
			// S3 cannot rename: the displacement is a server-side copy, which rclone counts in
			// bytes and in serverSideCopyBytes.
			name:  "s3 server-side copy into the backup dir",
			stats: `{"bytes":3674832,"serverSideCopies":1,"serverSideCopyBytes":1048576,"serverSideMoves":0,"serverSideMoveBytes":0}`,
			want:  uploads,
		},
		{
			// SFTP renames: the displacement is a server-side move accounted as a checking
			// transfer, in serverSideMoveBytes only (the regression: it was subtracted from bytes,
			// which reported 1 577 680 bytes).
			name:  "sftp server-side move into the backup dir",
			stats: `{"bytes":2626256,"serverSideCopies":0,"serverSideCopyBytes":0,"serverSideMoves":1,"serverSideMoveBytes":1048576}`,
			want:  uploads,
		},
		{
			name:  "uploads only",
			stats: `{"bytes":2626256,"serverSideCopyBytes":0,"serverSideMoveBytes":0}`,
			want:  uploads,
		},
		{
			name:  "nothing sent",
			stats: `{"bytes":0,"serverSideCopyBytes":0,"serverSideMoveBytes":0}`,
			want:  0,
		},
		{
			// Never negative, whatever a stats line says.
			name:  "inconsistent stats",
			stats: `{"bytes":10,"serverSideCopyBytes":20}`,
			want:  0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st rclone.Stats
			if err := json.Unmarshal([]byte(tt.stats), &st); err != nil {
				t.Fatal(err)
			}
			if got := uploadedBytes(st); got != tt.want {
				t.Errorf("uploadedBytes(%s) = %d, want %d", tt.stats, got, tt.want)
			}
		})
	}
}
