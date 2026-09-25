package destinations

import (
	"errors"
	"testing"
)

func TestParseSettingsDefaults(t *testing.T) {
	// The schema's column default must parse to the same defaults.
	const schemaDefault = `{"verify":{"mode":"sample","samplePercent":5},"hardlinks":"recreate","adoptExisting":"size+mtime","mtimeWindowSec":0,"maxChangePercent":10,"maxChangeFiles":1000}`
	for _, raw := range []string{"", "{}", schemaDefault, `{"verify":{}}`, `{"verify":{"mode":"","samplePercent":0},"maxChangePercent":0,"maxChangeFiles":0}`} {
		got, err := ParseSettings(raw)
		if err != nil {
			t.Fatalf("ParseSettings(%q): %v", raw, err)
		}
		if got != DefaultSettings() {
			t.Errorf("ParseSettings(%q) = %+v, want the defaults %+v", raw, got, DefaultSettings())
		}
	}
	got, err := ParseSettings(`{"verify":{"mode":"full"},"hardlinks":"copy","mtimeWindowSec":2,"maxChangeFiles":50}`)
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultSettings()
	want.Verify.Mode = VerifyFull
	want.Hardlinks = HardlinksCopy
	want.MtimeWindowSec = 2
	want.MaxChangeFiles = 50
	if got != want {
		t.Errorf("partial settings = %+v, want %+v", got, want)
	}
}

func TestParseRetentionDefaults(t *testing.T) {
	const schemaDefault = `{"deletedDays":30,"plexDbDaily":14,"plexDbWeekly":8}`
	for _, raw := range []string{"", "{}", schemaDefault, `{"deletedDays":0}`, `{"deletedDays":0,"plexDbDaily":0,"plexDbWeekly":0}`} {
		got, err := ParseRetention(raw)
		if err != nil {
			t.Fatalf("ParseRetention(%q): %v", raw, err)
		}
		if got != DefaultRetention() {
			t.Errorf("ParseRetention(%q) = %+v, want the defaults", raw, got)
		}
	}
	if got, _ := ParseRetention(`{"deletedDays":0}`); got.DeletedDays != 30 {
		t.Errorf("zero deletedDays = %d, want 30 (never zero retention)", got.DeletedDays)
	}
	got, err := ParseRetention(`{"deletedDays":1,"plexDbWeekly":520}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Retention{DeletedDays: 1, PlexDBDaily: 14, PlexDBWeekly: 520}) {
		t.Errorf("partial retention = %+v", got)
	}
}

func TestSettingsValidation(t *testing.T) {
	bad := []Settings{
		{Verify: Verify{Mode: "sometimes"}},
		{Verify: Verify{SamplePercent: 101}},
		{Verify: Verify{SamplePercent: -1}},
		{Hardlinks: "symlink"},
		{AdoptExisting: "always"},
		{MtimeWindowSec: -1},
		{MtimeWindowSec: 3601},
		{MaxChangePercent: 101},
		{MaxChangePercent: -5},
		{MaxChangeFiles: -1},
	}
	for _, s := range bad {
		var ve ValidationError
		if _, err := s.Normalize(); !errors.As(err, &ve) {
			t.Errorf("Normalize(%+v) = %v, want a ValidationError", s, err)
		}
	}
	ok := []Settings{
		{Verify: Verify{Mode: VerifyOff, SamplePercent: 100}},
		{MtimeWindowSec: 3600, MaxChangePercent: 100, MaxChangeFiles: 1},
		{AdoptExisting: AdoptOff},
		{AdoptExisting: AdoptSizeHash},
	}
	for _, s := range ok {
		if _, err := s.Normalize(); err != nil {
			t.Errorf("Normalize(%+v) = %v", s, err)
		}
	}
}

func TestRetentionValidation(t *testing.T) {
	bad := []Retention{
		{DeletedDays: -1},
		{DeletedDays: 3651},
		{PlexDBDaily: -1},
		{PlexDBDaily: 366},
		{PlexDBWeekly: -1},
		{PlexDBWeekly: 521},
	}
	for _, r := range bad {
		var ve ValidationError
		if _, err := r.Normalize(); !errors.As(err, &ve) {
			t.Errorf("Normalize(%+v) = %v, want a ValidationError", r, err)
		}
	}
	if _, err := ParseRetention(`{"deletedDays":"x"}`); err == nil {
		t.Errorf("invalid JSON accepted")
	}
}
