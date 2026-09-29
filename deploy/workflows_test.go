package deploy

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// The release workflow builds the published image and pushes it with a packages:write token.
// The containers that build runs are trusted with it: binfmt runs privileged and registers the
// QEMU that the arm64 runtime stage's RUN executes under, the BuildKit daemon writes every layer
// next to the registry credentials, and the SBOM scanner reads each platform's result. The
// actions default to mutable Docker Hub tags (tonistiigi/binfmt:latest,
// moby/buildkit:buildx-stable-1, docker/buildkit-syft-scanner:stable-1), so whoever moved one of
// them at release time would ship in every install. The release workflow therefore pins them by
// digest, like the Dockerfile's base images, and Renovate's custom.regex manager keeps the pins
// current.

const releaseWorkflow = ".github/workflows/release.yml"

var (
	// A step key "uses: owner/action@ref", on the step's "- " line or below it. Groups: 1 the
	// indentation, 2 "- " when the key opens the step, 3 the action.
	workflowUses = regexp.MustCompile(`^( *)(- )?uses: *([^@\s]+)@`)
	// A digest-pinned image reference, name:tag@sha256:<hex>. The tag stays for readability and
	// Renovate. Groups: 1 the name, 2 the tag, 3 the digest.
	pinnedImage = regexp.MustCompile(`^([a-z0-9][a-z0-9._/-]*):([A-Za-z0-9._-]+)@(sha256:[0-9a-f]{64})$`)
	// Tags Renovate's docker versioning can read ([v]X.Y.Z, then a suffix); any other form needs a
	// regex versioning rule, or Renovate never proposes a newer tag.
	dockerVersioning = regexp.MustCompile(`^v?\d+(?:\.\d+)*\w*(?:-.*)?$`)
)

// workflowStep is a workflow step that uses an action: the action, the line of its uses: key and
// its with: inputs.
type workflowStep struct {
	action string
	line   int
	with   map[string][]inputLine
}

// inputLine is one line of a with: input's value: the key's own line for a plain value (text
// unquoted, without a trailing comment), each content line for a block scalar (|, >). n is the
// 1-based line number in the workflow.
type inputLine struct {
	n    int
	text string
}

// workflowSteps returns every step of the workflow at rel that uses an action, with its with:
// inputs, and the workflow's lines. Plain values and block scalars are read; any other form of a
// with: entry fails the test, because the checks below would not see its value.
func workflowSteps(t *testing.T, rel string) ([]workflowStep, []string) {
	t.Helper()
	lines := strings.Split(readRepoFile(t, rel), "\n")
	indent := func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	ignorable := func(l string) bool {
		tr := strings.TrimSpace(l)
		return tr == "" || strings.HasPrefix(tr, "#")
	}
	plain := func(v string) string {
		if k := strings.Index(v, " #"); k >= 0 {
			v = v[:k]
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		return v
	}
	var steps []workflowStep
	for i, l := range lines {
		m := workflowUses.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		// The step's keys are at keyIndent; its "- " line is two columns left of them.
		keyIndent, start := len(m[1]), i
		if m[2] != "" {
			keyIndent += 2
		} else {
			start = -1
			for j := i - 1; j >= 0 && start < 0; j-- {
				if ignorable(lines[j]) || indent(lines[j]) >= keyIndent {
					continue
				}
				if indent(lines[j]) != keyIndent-2 || !strings.HasPrefix(strings.TrimLeft(lines[j], " "), "- ") {
					break
				}
				start = j
			}
			if start < 0 {
				t.Fatalf("%s:%d: uses: is not a key of a step (- ...)", rel, i+1)
			}
		}
		step := workflowStep{action: m[3], line: i + 1, with: map[string][]inputLine{}}
		inWith := false
		for j := start; j < len(lines); j++ {
			key := lines[j]
			if j == start {
				key = strings.Repeat(" ", keyIndent) + strings.TrimLeft(key, " ")[2:]
			} else if ignorable(key) {
				continue
			}
			ind := indent(key)
			if ind < keyIndent {
				break // the next step, or the end of the steps
			}
			if ind == keyIndent {
				inWith = plain(key) == "with:"
				continue
			}
			if !inWith {
				continue // the value of another key of the step
			}
			entry := strings.TrimSpace(key)
			name, value, ok := strings.Cut(entry, ":")
			if !ok || strings.ContainsAny(name, " \"'") {
				t.Fatalf("%s:%d: with: entry %q is not \"key: value\", which this test cannot read", rel, j+1, entry)
			}
			value = strings.TrimSpace(value)
			if strings.HasPrefix(value, "|") || strings.HasPrefix(value, ">") {
				var block []inputLine
				for k := j + 1; k < len(lines); k++ {
					if strings.TrimSpace(lines[k]) == "" {
						continue
					}
					if indent(lines[k]) <= ind {
						break
					}
					block = append(block, inputLine{k + 1, strings.TrimSpace(lines[k])})
					j = k
				}
				step.with[name] = block
				continue
			}
			if value == "" || strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") {
				t.Fatalf("%s:%d: with: %s is not a plain value or a block scalar (|), which this test cannot read", rel, j+1, name)
			}
			step.with[name] = []inputLine{{j + 1, plain(value)}}
		}
		steps = append(steps, step)
	}
	return steps, lines
}

// builderPin is an image reference a workflow step runs, with the workflow line that holds it
// (what Renovate's regex manager reads).
type builderPin struct {
	where, line, what string
	name, tag, digest string
}

// releaseBuilderPins returns every image the release workflow's docker/* steps run to build, and
// fails the test for each one that is not pinned by digest.
func releaseBuilderPins(t *testing.T) []builderPin {
	t.Helper()
	steps, lines := workflowSteps(t, releaseWorkflow)
	var pins []builderPin
	pin := func(l inputLine, ref, what string) {
		where := fmt.Sprintf("%s:%d", releaseWorkflow, l.n)
		m := pinnedImage.FindStringSubmatch(ref)
		if m == nil {
			t.Errorf("%s: %s %q is not pinned by digest (name:tag@sha256:...): the action would run whatever that tag points to at release time", where, what, ref)
			return
		}
		pins = append(pins, builderPin{where, lines[l.n-1], what, m[1], m[2], m[3]})
	}
	one := func(s workflowStep, input string) (inputLine, bool) {
		v := s.with[input]
		if len(v) > 1 {
			t.Errorf("%s:%d: %s: with: %s has %d lines, want one value", releaseWorkflow, s.line, s.action, input, len(v))
		}
		if len(v) == 0 {
			return inputLine{}, false
		}
		return v[0], true
	}
	buildx := 0
	for _, s := range steps {
		switch s.action {
		case "docker/setup-qemu-action":
			l, ok := one(s, "image")
			if !ok {
				t.Errorf("%s:%d: %s without with: image: runs its default, docker.io/tonistiigi/binfmt:latest, privileged", releaseWorkflow, s.line, s.action)
				continue
			}
			pin(l, l.text, "the binfmt image (setup-qemu-action image:)")
		case "docker/setup-buildx-action":
			buildx++
			driver := "docker-container"
			if l, ok := one(s, "driver"); ok {
				driver = l.text
			}
			if len(s.with["append"]) > 0 {
				t.Errorf("%s:%d: %s: append: nodes run BuildKit images this test does not check", releaseWorkflow, s.line, s.action)
			}
			switch driver {
			case "docker":
				// The Docker Engine's own BuildKit: no builder container is pulled or started.
			case "docker-container":
				var image *inputLine
				for _, l := range s.with["driver-opts"] {
					if ref, ok := strings.CutPrefix(l.text, "image="); ok {
						image = &inputLine{l.n, ref}
					}
				}
				if image == nil {
					t.Errorf("%s:%d: %s without driver-opts: image=...: starts its default BuildKit, moby/buildkit:buildx-stable-1, privileged", releaseWorkflow, s.line, s.action)
					continue
				}
				pin(*image, image.text, "the BuildKit image (setup-buildx-action driver-opts image=)")
			default:
				t.Errorf("%s:%d: %s: driver %q: this test knows which image only the docker and docker-container drivers run", releaseWorkflow, s.line, s.action, driver)
			}
		case "docker/build-push-action":
			// sbom: true (or any SBOM attestation without a generator) runs BuildKit's default
			// scanner, docker/buildkit-syft-scanner:stable-1.
			var attests []inputLine
			if l, ok := one(s, "sbom"); ok && l.text != "false" {
				attests = append(attests, inputLine{l.n, "type=sbom," + l.text})
			}
			for _, l := range s.with["attests"] {
				if strings.Contains(l.text, "type=sbom") {
					attests = append(attests, l)
				}
			}
			for _, a := range attests {
				generator := ""
				for _, attr := range strings.Split(a.text, ",") {
					if g, ok := strings.CutPrefix(strings.TrimSpace(attr), "generator="); ok {
						generator = g
					}
				}
				if generator == "" {
					t.Errorf("%s:%d: %s: the SBOM attestation has no generator=: BuildKit runs its default scanner, docker/buildkit-syft-scanner:stable-1", releaseWorkflow, a.n, s.action)
					continue
				}
				pin(a, generator, "the SBOM scanner (build-push-action sbom generator=)")
			}
		}
	}
	if buildx == 0 {
		t.Fatalf("%s: no docker/setup-buildx-action step found: this test no longer reads the workflow", releaseWorkflow)
	}
	return pins
}

// Every image the release build runs is pinned by digest, and one image has one pin (a second,
// stale copy would be missed by whoever bumps the first).
func TestReleaseBuilderImagesPinnedByDigest(t *testing.T) {
	pins := releaseBuilderPins(t)
	if len(pins) == 0 {
		t.Fatalf("%s: no pinned builder image found", releaseWorkflow)
	}
	first := map[string]builderPin{}
	for _, p := range pins {
		if f, ok := first[p.name]; ok && f.tag+"@"+f.digest != p.tag+"@"+p.digest {
			t.Errorf("%s: %s is %s:%s@%s, but %s pins %s@%s", p.where, p.what, p.name, p.tag, p.digest, f.where, f.tag, f.digest)
		} else if !ok {
			first[p.name] = p
		}
	}
}

// Renovate reads the pins: .github/renovate.json's regex manager matches every one on its
// workflow line, with the image, tag and digest this test read, and can order the image's tags.
func TestRenovateUpdatesReleaseBuilderPins(t *testing.T) {
	var cfg struct {
		EnabledManagers []string `json:"enabledManagers"`
		CustomManagers  []struct {
			CustomType          string   `json:"customType"`
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
		} `json:"customManagers"`
		PackageRules []struct {
			MatchPackageNames []string `json:"matchPackageNames"`
			Versioning        string   `json:"versioning"`
		} `json:"packageRules"`
	}
	if err := json.Unmarshal([]byte(readRepoFile(t, ".github/renovate.json")), &cfg); err != nil {
		t.Fatalf(".github/renovate.json: %v", err)
	}
	enabled := len(cfg.EnabledManagers) == 0
	for _, m := range cfg.EnabledManagers {
		enabled = enabled || m == "custom.regex"
	}
	if !enabled {
		t.Errorf(".github/renovate.json: enabledManagers %v leaves out custom.regex: the builder image pins are never updated", cfg.EnabledManagers)
	}
	type manager struct{ files, match []*regexp.Regexp }
	var managers []manager
	for i, m := range cfg.CustomManagers {
		if m.CustomType != "regex" || m.DatasourceTemplate != "docker" {
			continue
		}
		var mg manager
		for _, p := range m.ManagerFilePatterns {
			re, ok := strings.CutPrefix(p, "/")
			if re, ok = strings.CutSuffix(re, "/"); !ok {
				t.Fatalf(".github/renovate.json: customManagers[%d]: managerFilePatterns %q: this test reads /regex/ patterns only", i, p)
			}
			mg.files = append(mg.files, regexp.MustCompile(re))
		}
		for _, s := range m.MatchStrings {
			mg.match = append(mg.match, regexp.MustCompile(s))
		}
		managers = append(managers, mg)
	}
	for _, p := range releaseBuilderPins(t) {
		matched := false
		for _, mg := range managers {
			inFile := false
			for _, f := range mg.files {
				inFile = inFile || f.MatchString(releaseWorkflow)
			}
			for _, re := range mg.match {
				m := re.FindStringSubmatch(p.line)
				if !inFile || m == nil {
					continue
				}
				got := func(group string) string { return m[re.SubexpIndex(group)] }
				if got("depName") == p.name && got("currentValue") == p.tag && got("currentDigest") == p.digest {
					matched = true
				}
			}
		}
		if !matched {
			t.Errorf("%s: %s %s:%s@%s: no custom regex manager of .github/renovate.json reads it (depName, currentValue, currentDigest), so Renovate never updates it", p.where, p.what, p.name, p.tag, p.digest)
		}
		versioned := false
		for _, r := range cfg.PackageRules {
			for _, n := range r.MatchPackageNames {
				re, ok := strings.CutPrefix(r.Versioning, "regex:")
				if n != p.name || !ok {
					continue
				}
				versioned = true
				if !regexp.MustCompile(re).MatchString(p.tag) {
					t.Errorf("%s: %s tag %q does not match its Renovate versioning %q", p.where, p.name, p.tag, r.Versioning)
				}
			}
		}
		if !versioned && !dockerVersioning.MatchString(p.tag) {
			t.Errorf("%s: %s tag %q is not [v]X.Y.Z: Renovate's docker versioning cannot order it; add a packageRules versioning regex for %s", p.where, p.name, p.tag, p.name)
		}
	}
}
