package logging

import (
	"bytes"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactsSecretsInBothOutputs(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	log, closer, err := New(Options{Dir: dir, Level: "info", StdoutFormat: "text", Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("hello", "apiKey", "abc123secret", "Password", "hunter2", "user", "alice")
	_ = closer.Close()

	file, err := os.ReadFile(filepath.Join(dir, "bunkarr.log"))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{"stdout": out.String(), "file": string(file)} {
		if strings.Contains(got, "abc123secret") || strings.Contains(got, "hunter2") {
			t.Errorf("%s leaked a secret: %s", name, got)
		}
		if !strings.Contains(got, "alice") {
			t.Errorf("%s lost a normal attribute: %s", name, got)
		}
	}
	if !strings.HasPrefix(string(file), "{") {
		t.Errorf("file log is not JSON: %s", file)
	}
}

func TestRedactURL(t *testing.T) {
	u, _ := url.Parse("http://user:pw@host/api/v1/x?apikey=SECRET&page=2&X-Plex-Token=T0K")
	got := RedactURL(u)
	if strings.Contains(got, "SECRET") || strings.Contains(got, "T0K") || strings.Contains(got, ":pw@") {
		t.Fatalf("RedactURL leaked: %s", got)
	}
	if !strings.Contains(got, "page=2") {
		t.Fatalf("RedactURL dropped a normal parameter: %s", got)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRotatingWriter(dir, "t", 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	line := bytes.Repeat([]byte("x"), 60)
	for range 5 {
		if _, err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close()
	for _, name := range []string{"t.log", "t.1.log", "t.2.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "t.3.log")); err == nil {
		t.Error("kept more files than configured")
	}
	if _, err := w.Write(line); err == nil {
		t.Error("write after Close succeeded")
	}
}
