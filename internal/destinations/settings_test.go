package destinations

import (
	"encoding/json"
	"errors"
	"strings"
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
	want := DefaultRetention()
	want.DeletedDays, want.PlexDBWeekly = 1, 520
	if got != want {
		t.Errorf("partial retention = %+v", got)
	}
}

func TestRetentionManifestVersions(t *testing.T) {
	// manifestDays and manifestWeeks (phase2-3.md §11.2 step 6) are kept, so internal/manifest
	// finds them in the stored JSON. manifestWeeks 0 keeps no weekly versions: only a missing
	// manifestWeeks takes the default, whether it comes from the API or from the stored JSON.
	var in Input
	if err := json.Unmarshal([]byte(`{"retention":{"manifestDays":7,"manifestWeeks":0}}`), &in); err != nil {
		t.Fatal(err)
	}
	r, err := in.Retention.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if r.ManifestDays != 7 || r.ManifestWeeks != 0 {
		t.Fatalf("normalized %+v; want manifestDays 7, manifestWeeks 0", r)
	}
	if again, err := r.Normalize(); err != nil || again != r {
		t.Fatalf("normalizing again = %+v, %v", again, err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"manifestDays":7,"manifestWeeks":0`) {
		t.Fatalf("stored retention %s", raw)
	}
	back, err := ParseRetention(string(raw))
	if err != nil || back != r {
		t.Fatalf("ParseRetention(%s) = %+v, %v; want %+v", raw, back, err, r)
	}
	for raw, want := range map[string][2]int{
		`{}`: {DefaultManifestDays, DefaultManifestWeeks},
		`{"manifestDays":0,"manifestWeeks":null}`:   {DefaultManifestDays, DefaultManifestWeeks},
		`{"manifestWeeks":0}`:                       {DefaultManifestDays, 0},
		`{"manifestDays":3650,"manifestWeeks":520}`: {3650, 520},
		`{"manifestDays":1,"unknown":true}`:         {1, DefaultManifestWeeks},
	} {
		got, err := ParseRetention(raw)
		if err != nil || got.ManifestDays != want[0] || got.ManifestWeeks != want[1] {
			t.Errorf("ParseRetention(%s) = %+v, %v; want %v", raw, got, err, want)
		}
	}
	for _, raw := range []string{`{"manifestDays":3651}`, `{"manifestDays":-1}`, `{"manifestWeeks":-1}`, `{"manifestWeeks":521}`} {
		var ve ValidationError
		if _, err := ParseRetention(raw); !errors.As(err, &ve) {
			t.Errorf("ParseRetention(%s) = %v, want a ValidationError", raw, err)
		}
	}
	// A Go literal's zero is missing, as for the other periods.
	if r, _ := (Retention{DeletedDays: 3}).Normalize(); r.ManifestWeeks != DefaultManifestWeeks {
		t.Errorf("literal zero manifestWeeks = %d", r.ManifestWeeks)
	}
	// The API decodes strictly: an unknown retention key is still refused, and so is trailing data.
	for _, body := range []string{`{"retention":{"manifestDayz":7}}`} {
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&Input{}); err == nil {
			t.Errorf("%s accepted", body)
		}
	}
	if _, err := ParseRetention(`{"manifestDays":7} x`); err == nil {
		t.Error("trailing data accepted")
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
		{ArrDaily: -1},
		{ArrDaily: 366},
		{ArrWeekly: -1},
		{ArrWeekly: 521},
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
