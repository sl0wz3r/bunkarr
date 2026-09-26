package plex

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestFlexTypes(t *testing.T) {
	var v struct {
		S flexString   `json:"s"`
		I flexInt      `json:"i"`
		B flexBool     `json:"b"`
		L list[string] `json:"l"`
	}
	tests := []struct {
		body string
		s    string
		i    int
		b    bool
		l    []string
	}{
		{`{"s":"x","i":"12","b":"1","l":["a","b"]}`, "x", 12, true, []string{"a", "b"}},
		{`{"s":42,"i":7,"b":true,"l":"not a list"}`, "42", 7, true, nil},
		{`{"s":null,"i":null,"b":"yes","l":null}`, "", 0, true, nil},
		{`{"s":" y ","i":" 3 ","b":"off","l":[]}`, "y", 3, false, []string{}},
		{`{"b":{"x":1}}`, "", 0, false, nil},
	}
	for _, tt := range tests {
		v.S, v.I, v.B, v.L = "", 0, false, nil
		if err := json.Unmarshal([]byte(tt.body), &v); err != nil {
			t.Fatalf("%s: %v", tt.body, err)
		}
		if v.S.String() != tt.s || v.I.Int() != tt.i || bool(v.B) != tt.b || !reflect.DeepEqual([]string(v.L), tt.l) {
			t.Errorf("%s = %+v", tt.body, v)
		}
	}
	// flexString and flexInt refuse what is neither a string nor a number.
	for _, body := range []string{`{"s":true}`, `{"s":{}}`, `{"i":"abc"}`, `{"i":1.5}`} {
		if err := json.Unmarshal([]byte(body), &v); err == nil {
			t.Errorf("%s was accepted", body)
		}
	}
	// A single object is a one-element list.
	var objs list[struct{ A int }]
	if err := json.Unmarshal([]byte(`{"A":1}`), &objs); err != nil || len(objs) != 1 || objs[0].A != 1 {
		t.Errorf("single object list = %+v, %v", objs, err)
	}
}

func TestLenientParsers(t *testing.T) {
	for in, want := range map[string]int64{"12": 12, " 12.9 ": 12, "true": 1, "false": 0, "": 0, "x": 0, "1e30": 0} {
		if got := parseIntText(in); got != want {
			t.Errorf("parseIntText(%q) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[string]bool{"1": true, "TRUE": true, "yes": true, "0": false, "no": false, "": false, "2": true, "maybe": false} {
		if got := parseBoolText(in); got != want {
			t.Errorf("parseBoolText(%q) = %v, want %v", in, got, want)
		}
	}
	if got := firstNonEmpty("", " ", "a", "b"); got != "a" {
		t.Errorf("firstNonEmpty = %q", got)
	}
	if got := string(trimBody([]byte("\n\xef\xbb\xbf {} "))); got != "{}" {
		t.Errorf("trimBody = %q", got)
	}
	if !unixTime(0).IsZero() || !unixTime(1790000000).Equal(time.Unix(1790000000, 0)) || !unixTime(1790000000123).Equal(time.UnixMilli(1790000000123)) {
		t.Error("unixTime")
	}
	if !parsePlexTime("2026-09-22T12:30:00.123Z").Equal(time.Date(2026, 9, 22, 12, 30, 0, 123e6, time.UTC)) ||
		!parsePlexTime("1790000000").Equal(time.Unix(1790000000, 0)) || !parsePlexTime("tomorrow").IsZero() {
		t.Error("parsePlexTime")
	}
	for in, want := range map[string]bool{"server": true, "client,server": true, " Server , player": true, "client,player": false, "": false, "servers": false} {
		if got := providesServer(in); got != want {
			t.Errorf("providesServer(%q) = %v", in, got)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	o := Options{}.withDefaults()
	if o.PlexTVURL != PlexTVURL || o.ClientsPlexTVURL != ClientsPlexTVURL || o.Product != "Bunkarr" ||
		o.ClientIdentifier != DefaultClientIdentifier || o.Device == "" || o.DeviceName != "Bunkarr" || o.Timeout != DefaultTimeout {
		t.Errorf("defaults = %+v", o)
	}
	o = Options{PlexTVURL: " http://127.0.0.1:9/ ", Device: "Docker"}.withDefaults()
	if o.PlexTVURL != "http://127.0.0.1:9" || o.Device != "Docker" {
		t.Errorf("trimmed = %+v", o)
	}
}
