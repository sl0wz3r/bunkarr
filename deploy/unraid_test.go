// Package deploy holds no code: its tests tie the files Bunkarr ships for deployment together (the
// compose file, the Unraid template, the release workflow and their documentation), so an edit to
// one of them cannot silently leave another behind. unraid/ca/validate-template.sh checks the
// template's Community Applications format.
package deploy

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// readRepoFile returns a file's content by its path from the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// composeItem is one list item of a compose block: its text after "- ", whether it is commented
// out, and its line number.
type composeItem struct {
	text      string
	commented bool
	line      int
}

// composeItems returns every list item of the service's KEY: block (volumes, environment or
// ports) of deploy/docker-compose.yml, commented-out items included. Comment lines that are not
// items are skipped; any other line in the block (a map entry, a flow list) fails the test,
// because the checks below compare list items only and would not see it.
func composeItems(t *testing.T, compose, key string) []composeItem {
	t.Helper()
	lines := strings.Split(compose, "\n")
	start, indent := -1, 0
	for i, l := range lines {
		trimmed := strings.TrimLeft(l, " \t")
		// Indented: a key of the service, not a top-level volumes: of named volumes.
		if strings.TrimRight(trimmed, " \t\r") == key+":" && len(trimmed) < len(l) {
			if start >= 0 {
				t.Fatalf("deploy/docker-compose.yml:%d: a second %s: block", i+1, key)
			}
			start, indent = i, len(l)-len(trimmed)
		}
	}
	if start < 0 {
		t.Fatalf("deploy/docker-compose.yml: no %s: block written as a list (this test reads its items)", key)
	}
	var items []composeItem
	for i := start + 1; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], " \t\r")
		trimmed := strings.TrimLeft(l, " \t")
		if trimmed == "" {
			continue
		}
		body, commented := strings.CutPrefix(trimmed, "#")
		if !commented && len(l)-len(trimmed) <= indent {
			break // the next key of the service
		}
		if item, ok := strings.CutPrefix(strings.TrimLeft(body, " \t"), "- "); ok {
			items = append(items, composeItem{strings.TrimSpace(item), commented, i + 1})
		} else if !commented {
			t.Errorf("deploy/docker-compose.yml:%d: %s: %q is not a list item (- \"...\"), which this test cannot compare", i+1, key, trimmed)
		}
	}
	return items
}

type templateConfig struct {
	Name     string `xml:"Name,attr"`
	Target   string `xml:"Target,attr"`
	Default  string `xml:"Default,attr"`
	Mode     string `xml:"Mode,attr"`
	Type     string `xml:"Type,attr"`
	Required string `xml:"Required,attr"`
	Value    string `xml:",chardata"`
}

type unraidTemplate struct {
	Name        string           `xml:"Name"`
	Repository  string           `xml:"Repository"`
	Privileged  string           `xml:"Privileged"`
	WebUI       string           `xml:"WebUI"`
	TemplateURL string           `xml:"TemplateURL"`
	ExtraParams string           `xml:"ExtraParams"`
	Configs     []templateConfig `xml:"Config"`
}

func readTemplate(t *testing.T) unraidTemplate {
	t.Helper()
	var tpl unraidTemplate
	if err := xml.Unmarshal([]byte(readRepoFile(t, "unraid/bunkarr.xml")), &tpl); err != nil {
		t.Fatalf("unraid/bunkarr.xml: %v", err)
	}
	return tpl
}

func (tpl unraidTemplate) config(typ, target string) (templateConfig, bool) {
	for _, c := range tpl.Configs {
		if c.Type == typ && c.Target == target {
			return c, true
		}
	}
	return templateConfig{}, false
}

var (
	// The list items of compose's volumes:, environment: and ports: blocks, after "- " (see
	// composeItems). Every item must match its block's form, so none escapes the comparison.
	//
	// "${VAR:-default}:/target[:mode]" or "${VAR:?message}:/target[:mode]"; commented out =
	// optional. Groups: 1 ":-" (a default; empty for ":?"), 2 the default, 3 the target, 4 the mode.
	composeVolume = regexp.MustCompile(`^"\$\{\w+(?:(:-)([^}]*)|:\?[^}]*)\}:(/[^:"]+)(?::([a-z,]+))?"$`)
	// "NAME=${VAR:-default}" or "NAME=${VAR:?message}". Groups: 1 the name, 2 ":-", 3 the default.
	composeEnv = regexp.MustCompile(`^"(\w+)=\$\{\w+(?:(:-)([^}]*)|:\?[^}]*)\}"$`)
	// A commented-out variable, an example the template leaves out (the engine paths): "NAME=...".
	composeEnvExample = regexp.MustCompile(`^"(\w+)=[^"]*"$`)
	// "host:container". Groups: 1 the host port, 2 the container port.
	composePort  = regexp.MustCompile(`^"(\d+):(\d+)"$`)
	composeField = func(key string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^[ \t]+` + key + `:[ \t]*"?([^"\n]*?)"?[ \t]*$`)
	}
	// release.yml's "Image name" step: name=ghcr.io/$(<lower-case GITHUB_REPOSITORY_OWNER>)/bunkarr.
	// Groups: 1 the registry, 2 the image name.
	releaseImage = regexp.MustCompile(`name=([a-z0-9.-]+)/\$\([^\n]*GITHUB_REPOSITORY_OWNER[^\n]*\)/([a-z0-9._-]+)"`)
)

// optionalOnUnraid names the mounts compose always makes that the template leaves optional, with
// the reason. Every other mount compose makes is Required="true" in the template, so Apply refuses
// an empty host path instead of starting without it.
var optionalOnUnraid = map[string]string{
	// Compose requires PLEX_DIR so that it never mounts a guessed path, and tells users without
	// the Plex DB backup to delete the line ("Not using the Plex DB backup? Delete this line."). A
	// template entry cannot be deleted, so it is optional instead.
	"/plex": "the Plex database backup is optional",
}

// The Unraid template is the compose file in dockerMan's words: the same image, container name,
// mounts with the same modes, port, variables, host name and stop grace. Differences are on purpose
// and pinned here: no TZ (Unraid injects the server's), PUID/PGID default to Unraid's 99/100, a
// mount compose requires without a default has no default path in the template either (Unraid
// creates missing host folders, so a guessed path would create an empty share), and every mount
// and variable compose requires is required in the template too, except optionalOnUnraid.
func TestUnraidTemplateMatchesCompose(t *testing.T) {
	compose := readRepoFile(t, "deploy/docker-compose.yml")
	tpl := readTemplate(t)

	field := func(key string) string {
		m := composeField(key).FindStringSubmatch(compose)
		if m == nil {
			t.Fatalf("deploy/docker-compose.yml: no %s", key)
		}
		return m[1]
	}
	if tpl.Repository != field("image") {
		t.Errorf("<Repository> %q, compose image %q", tpl.Repository, field("image"))
	}
	if tpl.Name != field("container_name") {
		t.Errorf("<Name> %q, compose container_name %q (the README's docker exec commands use it)", tpl.Name, field("container_name"))
	}
	if tpl.Privileged != "false" {
		t.Errorf("<Privileged> %q, want false", tpl.Privileged)
	}

	// Mounts.
	volumes := 0
	inCompose := map[string]bool{}
	for _, item := range composeItems(t, compose, "volumes") {
		v := composeVolume.FindStringSubmatch(item.text)
		if v == nil {
			t.Errorf("deploy/docker-compose.yml:%d: volume %s is not \"${VAR:?message}:/target[:mode]\" or \"${VAR:-default}:/target[:mode]\", "+
				"so this test cannot compare it with the template", item.line, item.text)
			continue
		}
		volumes++
		optional, def, target, mode := item.commented, v[2], v[3], v[4]
		if mode == "" {
			mode = "rw"
		}
		inCompose[target] = true
		c, ok := tpl.config("Path", target)
		if !ok {
			t.Errorf("unraid/bunkarr.xml: no Path %s (compose mounts it %s)", target, mode)
			continue
		}
		if c.Mode != mode {
			t.Errorf("Path %s: template Mode %q, compose %q", target, c.Mode, mode)
		}
		if c.Default != def || c.Value != def {
			t.Errorf("Path %s: template Default %q and value %q, want %q like compose", target, c.Default, c.Value, def)
		}
		why, allowed := optionalOnUnraid[target]
		switch {
		case optional && c.Required != "false":
			t.Errorf("Path %s is optional in compose (commented out): want Required=\"false\", got %q", target, c.Required)
		case optional && allowed:
			t.Errorf("optionalOnUnraid names %s, which compose already leaves optional: remove it from the list", target)
		case !optional && allowed && c.Required != "false":
			t.Errorf("Path %s: want Required=\"false\" (%s), got %q; if it is required now, remove it from optionalOnUnraid", target, why, c.Required)
		case !optional && !allowed && c.Required != "true":
			t.Errorf("Path %s: compose always mounts it, so the template needs Required=\"true\", got %q", target, c.Required)
		}
	}
	if volumes < 5 {
		t.Fatalf("deploy/docker-compose.yml: found %d volumes, want /config, /media, /plex, /backup and /arr/*", volumes)
	}
	for target := range optionalOnUnraid {
		if !inCompose[target] {
			t.Errorf("optionalOnUnraid names %s, which deploy/docker-compose.yml does not mount", target)
		}
	}
	for _, c := range tpl.Configs {
		if c.Type == "Path" && !inCompose[c.Target] {
			t.Errorf("Path %s is in the template but not in deploy/docker-compose.yml", c.Target)
		}
	}

	// Port and web UI, both ways.
	ports := map[string]bool{}
	for _, item := range composeItems(t, compose, "ports") {
		p := composePort.FindStringSubmatch(item.text)
		if p == nil || item.commented {
			t.Errorf("deploy/docker-compose.yml:%d: port %s is not \"host:container\" (published, numeric), so this test cannot compare it with the template", item.line, item.text)
			continue
		}
		ports[p[2]] = true
		c, ok := tpl.config("Port", p[2])
		if !ok || c.Mode != "tcp" || c.Default != p[1] || c.Value != p[1] {
			t.Errorf("unraid/bunkarr.xml: want Port %s (tcp, default host port %s) like compose", p[2], p[1])
		}
		if !strings.Contains(tpl.WebUI, "[PORT:"+p[2]+"]") {
			t.Errorf("<WebUI> %q does not use container port %s", tpl.WebUI, p[2])
		}
	}
	if len(ports) == 0 {
		t.Error("deploy/docker-compose.yml publishes no port: the web UI port 8787 is missing")
	}
	for _, c := range tpl.Configs {
		if c.Type == "Port" && !ports[c.Target] {
			t.Errorf("Port %s is in the template but not published by deploy/docker-compose.yml", c.Target)
		}
	}

	// Variables: compose's, minus TZ; Unraid's user and group as defaults. Commented-out variables
	// are examples (other restic and rclone programs) and stay out of the template.
	allowed := map[string]bool{"BUNKARR_LOG_LEVEL": true}
	for _, item := range composeItems(t, compose, "environment") {
		if item.commented && composeEnvExample.MatchString(item.text) {
			continue
		}
		e := composeEnv.FindStringSubmatch(item.text)
		if e == nil || item.commented {
			t.Errorf("deploy/docker-compose.yml:%d: variable %s is not \"NAME=${VAR:?message}\" or \"NAME=${VAR:-default}\", "+
				"so this test cannot compare it with the template", item.line, item.text)
			continue
		}
		name, required, def := e[1], e[2] == "", e[3]
		allowed[name] = true
		if name == "TZ" {
			continue
		}
		c, ok := tpl.config("Variable", name)
		if !ok {
			t.Errorf("unraid/bunkarr.xml: no Variable %s (compose sets it)", name)
			continue
		}
		if def != "" && (c.Default != def || c.Value != def) {
			t.Errorf("Variable %s: template Default %q and value %q, compose default %q", name, c.Default, c.Value, def)
		}
		if required && c.Required != "true" {
			t.Errorf("Variable %s: compose requires it, so the template needs Required=\"true\", got %q", name, c.Required)
		}
	}
	for name, want := range map[string]string{"PUID": "99", "PGID": "100"} {
		if c, ok := tpl.config("Variable", name); ok && (c.Default != want || c.Value != want) {
			t.Errorf("Variable %s: want %s (Unraid's), got Default %q value %q", name, want, c.Default, c.Value)
		}
	}
	for _, c := range tpl.Configs {
		switch {
		case c.Target == "TZ":
			t.Error("unraid/bunkarr.xml has a TZ entry: Unraid injects the server's TZ, and a template value overrides it")
		case c.Type == "Variable" && !allowed[c.Target]:
			t.Errorf("Variable %s is not in deploy/docker-compose.yml (test-only and engine-path variables stay out of the template)", c.Target)
		}
	}

	// Host name and stop grace travel in Extra Parameters.
	if !strings.HasPrefix(field("hostname"), "bunkarr-${SERVER_NAME") {
		t.Errorf("compose hostname %q: this test expects bunkarr-${SERVER_NAME...}", field("hostname"))
	}
	if !regexp.MustCompile(`(^|\s)--hostname=bunkarr-[a-z0-9-]+(\s|$)`).MatchString(tpl.ExtraParams) {
		t.Errorf("<ExtraParams> %q: want --hostname=bunkarr-NAME (compose: hostname bunkarr-${SERVER_NAME})", tpl.ExtraParams)
	}
	grace := strings.TrimSuffix(field("stop_grace_period"), "s")
	if !strings.Contains(" "+tpl.ExtraParams+" ", " --stop-timeout="+grace+" ") {
		t.Errorf("<ExtraParams> %q: want --stop-timeout=%s (compose stop_grace_period %ss)", tpl.ExtraParams, grace, grace)
	}
}

// The template installs the image the release workflow pushes, and follows :latest: release.yml
// moves :latest only for a full release (never a pre-release), and Unraid's update check follows
// the tag in <Repository>. Its owner is lower case, like every Docker image name: the workflow
// lower-cases the repository owner.
func TestUnraidTemplateImageMatchesRelease(t *testing.T) {
	tpl := readTemplate(t)
	m := releaseImage.FindStringSubmatch(readRepoFile(t, ".github/workflows/release.yml"))
	if m == nil {
		t.Fatal(".github/workflows/release.yml: no \"Image name\" step writing name=REGISTRY/$(<lower-case owner>)/NAME")
	}
	image, ok := strings.CutSuffix(tpl.Repository, ":latest")
	if !ok {
		t.Fatalf("<Repository> %q: want the :latest tag (release.yml moves it for full releases only)", tpl.Repository)
	}
	parts := strings.Split(image, "/")
	if len(parts) != 3 || parts[0] != m[1] || parts[2] != m[2] || parts[1] == "" || parts[1] != strings.ToLower(parts[1]) {
		t.Errorf("<Repository> %q: want %s/<lower-case owner>/%s:latest, the image release.yml pushes", tpl.Repository, m[1], m[2])
	}
}

// The install guides give the template's raw URL for a manual install and describe every mount;
// they must name the published file and the template's container paths.
func TestUnraidDocsMatchTemplate(t *testing.T) {
	tpl := readTemplate(t)
	for _, doc := range []string{"README.md", "unraid/README.md"} {
		text := readRepoFile(t, doc)
		if !strings.Contains(text, tpl.TemplateURL) {
			t.Errorf("%s does not give the template URL %s", doc, tpl.TemplateURL)
		}
		if !strings.Contains(text, "--hostname=bunkarr-") {
			t.Errorf("%s does not explain --hostname in Extra Parameters", doc)
		}
	}
	guide := readRepoFile(t, "unraid/README.md")
	for _, c := range tpl.Configs {
		if (c.Type == "Path" || c.Type == "Port" || c.Type == "Variable") && !strings.Contains(guide, c.Target) {
			t.Errorf("unraid/README.md does not describe %s %s", c.Type, c.Target)
		}
	}
}
