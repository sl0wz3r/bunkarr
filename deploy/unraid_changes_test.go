package deploy

import (
	"regexp"
	"strings"
	"testing"
)

var (
	// Every second-level heading of CHANGELOG.md, whatever its form: group 1 its text.
	changelogHeading = regexp.MustCompile(`(?m)^## +([^\n]*?)[ \t\r]*$`)
	// The one form a release heading may take, "[0.1.0] - 2026-10-01": groups 1 the version (as
	// unraid/ca/render.sh accepts VERSION), 2 the release date.
	releaseHeading  = regexp.MustCompile(`^\[(\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)\] - (\d{4}-\d{2}-\d{2})$`)
	templateChanges = regexp.MustCompile(`(?s)<Changes>(.*?)</Changes>`)
	changesHeading  = regexp.MustCompile(`(?m)^### (\S+) \((\d{4}-\d{2}-\d{2})\)`)
)

// publishRelease returns VERSION and RELEASE_DATE from unraid/ca/publish.env.
func publishRelease(t *testing.T) (version, date string) {
	t.Helper()
	env := readRepoFile(t, "unraid/ca/publish.env")
	value := func(key string) string {
		m := regexp.MustCompile(`(?m)^` + key + `=(.*)$`).FindStringSubmatch(env)
		if m == nil {
			t.Fatalf("unraid/ca/publish.env: no %s", key)
		}
		return strings.Trim(strings.TrimSpace(m[1]), `"'`)
	}
	return value("VERSION"), value("RELEASE_DATE")
}

// The Unraid template's <Changes> is the change log Community Applications shows for the app, and
// <Date> puts it in CA's "Updated Apps" list. Both are maintained by hand (<Changes> in the
// template source, the version and date in unraid/ca/publish.env), so an edit could change one
// and not the other: pin that the newest <Changes> entry of the source and of the rendered
// template names VERSION and RELEASE_DATE, that <Date> is RELEASE_DATE, and that no version is
// listed twice. This holds before the first release too (the planned version and date).
func TestUnraidTemplateChangesMatchPublishEnv(t *testing.T) {
	version, date := publishRelease(t)
	for _, file := range []string{"unraid/ca/bunkarr.xml.tmpl", "unraid/bunkarr.xml"} {
		changes := templateChanges.FindStringSubmatch(readRepoFile(t, file))
		if changes == nil {
			t.Errorf("%s: no <Changes> element", file)
			continue
		}
		headings := changesHeading.FindAllStringSubmatch(changes[1], -1)
		if len(headings) == 0 || headings[0][1] != version || headings[0][2] != date {
			t.Errorf("%s: <Changes> must start with \"### %s (%s)\", VERSION and RELEASE_DATE in unraid/ca/publish.env", file, version, date)
		}
		seen := map[string]bool{}
		for _, h := range headings {
			if seen[h[1]] {
				t.Errorf("%s: <Changes> lists %s twice", file, h[1])
			}
			seen[h[1]] = true
		}
	}
	if !strings.Contains(readRepoFile(t, "unraid/bunkarr.xml"), "<Date>"+date+"</Date>") {
		t.Errorf("unraid/bunkarr.xml: want <Date>%s</Date> (RELEASE_DATE); run make ca-template", date)
	}
}

// From the first release on, VERSION and RELEASE_DATE in unraid/ca/publish.env (and with them the
// template's newest <Changes> entry and <Date>, TestUnraidTemplateChangesMatchPublishEnv) name the
// newest release in CHANGELOG.md, so a release cannot leave the template naming the previous
// version. Until then CHANGELOG.md has no "## " heading but [Unreleased] and publish.env holds the
// planned release, so the test skips. The first other "## " heading is the newest release, and
// one in any other form than "## [X.Y.Z] - YYYY-MM-DD" fails rather than skips.
func TestUnraidTemplateChangesMatchChangelog(t *testing.T) {
	var heading string
	for _, h := range changelogHeading.FindAllStringSubmatch(readRepoFile(t, "CHANGELOG.md"), -1) {
		if !strings.EqualFold(h[1], "[Unreleased]") {
			heading = h[1]
			break
		}
	}
	if heading == "" {
		t.Skip("CHANGELOG.md has no released version yet (only [Unreleased]): the template's release is checked against it from the first release on")
	}
	m := releaseHeading.FindStringSubmatch(heading)
	if m == nil {
		t.Fatalf("CHANGELOG.md: the newest release heading \"## %s\" is not \"## [X.Y.Z] - YYYY-MM-DD\"", heading)
	}
	version, released := publishRelease(t)
	if version != m[1] || released != m[2] {
		t.Errorf("unraid/ca/publish.env: VERSION=%s RELEASE_DATE=%s, want VERSION=%s RELEASE_DATE=%s (the newest release in CHANGELOG.md); "+
			"then update <Changes> in unraid/ca/bunkarr.xml.tmpl and run make ca-template", version, released, m[1], m[2])
	}
}
