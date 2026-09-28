package rclone

import (
	"errors"
	"testing"

	"github.com/sl0wz3r/bunkarr/internal/engines/bwlimit"
)

// TestBWLimit: rclone's timetable string for each table case (§9.1, §14.1 bwlimit).
func TestBWLimit(t *testing.T) {
	weekdays := []string{"mon", "tue", "wed", "thu", "fri"}
	tests := []struct {
		name string
		cfg  bwlimit.Config
		want string
	}{
		{"no limit", bwlimit.Config{}, ""},
		{"base upload only", bwlimit.Config{UploadKiBps: 1024}, "1024k:off"},
		{"base both", bwlimit.Config{UploadKiBps: 512, DownloadKiBps: 2048}, "512k:2048k"},
		{"design example", bwlimit.Config{Timetable: []bwlimit.Entry{{Days: weekdays, From: "08:00", To: "23:00", UploadKiBps: 1024}}},
			"Mon-08:00,1024k:off Mon-23:00,off:off Tue-08:00,1024k:off Tue-23:00,off:off Wed-08:00,1024k:off Wed-23:00,off:off " +
				"Thu-08:00,1024k:off Thu-23:00,off:off Fri-08:00,1024k:off Fri-23:00,off:off"},
		{"across midnight with a base", bwlimit.Config{UploadKiBps: 100, Timetable: []bwlimit.Entry{{Days: []string{"mon"}, From: "23:00", To: "06:00", UploadKiBps: 4096, DownloadKiBps: 64}}},
			"Mon-23:00,4096k:64k Tue-06:00,100k:off"},
		{"across the week's end", bwlimit.Config{Timetable: []bwlimit.Entry{{Days: []string{"sun"}, From: "22:00", To: "02:00", UploadKiBps: 8}}},
			"Mon-02:00,off:off Sun-22:00,8k:off"},
		{"adjacent entries merge their change point", bwlimit.Config{Timetable: []bwlimit.Entry{
			{Days: []string{"mon"}, From: "08:00", To: "12:00", UploadKiBps: 1},
			{Days: []string{"mon"}, From: "12:00", To: "13:00", UploadKiBps: 2},
		}}, "Mon-08:00,1k:off Mon-12:00,2k:off Mon-13:00,off:off"},
		{"an entry equal to the base changes nothing", bwlimit.Config{UploadKiBps: 5, Timetable: []bwlimit.Entry{{Days: []string{"wed"}, From: "01:00", To: "02:00", UploadKiBps: 5}}}, "5k:off"},
		{"entries covering the whole week", bwlimit.Config{Timetable: []bwlimit.Entry{{Days: bwlimit.Days, From: "00:00", To: "12:00", UploadKiBps: 7}, {Days: bwlimit.Days, From: "12:00", To: "00:00", UploadKiBps: 7}}}, "7k:off"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := BWLimit(tc.cfg)
			if err != nil || got != tc.want {
				t.Errorf("BWLimit = %q, %v\nwant %q", got, err, tc.want)
			}
		})
	}
	var ve *bwlimit.ValidationError
	if _, err := BWLimit(bwlimit.Config{Timetable: []bwlimit.Entry{{Days: []string{"mon"}, From: "25:00", To: "01:00"}}}); !errors.As(err, &ve) {
		t.Errorf("an invalid timetable: %v", err)
	}
}
