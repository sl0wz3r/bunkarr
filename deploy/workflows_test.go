package deploy

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The release workflows build the published image and push it with a registry token (GitHub's
// packages:write GITHUB_TOKEN; on the private Gitea, when publishing is on, REGISTRY_TOKEN). The
// containers that build runs are trusted with it: binfmt runs privileged and registers the QEMU
// that the arm64 runtime stage's RUN executes under, the BuildKit daemon writes every layer next to
// the registry credentials, and the SBOM scanner reads each platform's result. The actions default
// to mutable Docker Hub tags (tonistiigi/binfmt:latest, moby/buildkit:buildx-stable-1,
// docker/buildkit-syft-scanner:stable-1), so whoever moved one of them at release time would ship
// in every install. The release workflows therefore pin them by digest, like the Dockerfile's base
// images, and Renovate's custom.regex manager keeps the pins current.
//
// Every action any workflow uses is pinned to a commit (a tag runs whatever it points to when the
// job starts, with the job's token), the workflows gate on known vulnerabilities and on the Go
// patch release compiled into the image, no run: script expands an expression (the value would
// become shell code), and the Gitea release keeps plain HTTP and publishing behind explicit opt-ins.
// The public repository has no .gitea/ (scripts/public/exclude.txt): what reads a file there skips
// when it is absent.

const (
	releaseWorkflow      = ".github/workflows/release.yml"
	giteaReleaseWorkflow = ".gitea/workflows/release.yml"
)

// releaseWorkflows returns the release workflows present: GitHub's always, the Gitea one in the
// private repository.
func releaseWorkflows(t *testing.T) []string {
	t.Helper()
	files := []string{releaseWorkflow}
	if _, ok := optionalRepoFile(t, giteaReleaseWorkflow); ok {
		files = append(files, giteaReleaseWorkflow)
	}
	return files
}

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

// builderPin is an image reference a workflow step runs, with the workflow and the line that holds
// it (what Renovate's regex manager reads).
type builderPin struct {
	file, where, line, what string
	name, tag, digest       string
}

// releaseBuilderPins returns every image the docker/* steps of the release workflow at file run to
// build, and fails the test for each one that is not pinned by digest.
func releaseBuilderPins(t *testing.T, file string) []builderPin {
	t.Helper()
	steps, lines := workflowSteps(t, file)
	var pins []builderPin
	pin := func(l inputLine, ref, what string) {
		where := fmt.Sprintf("%s:%d", file, l.n)
		m := pinnedImage.FindStringSubmatch(ref)
		if m == nil {
			t.Errorf("%s: %s %q is not pinned by digest (name:tag@sha256:...): the action would run whatever that tag points to at release time", where, what, ref)
			return
		}
		pins = append(pins, builderPin{file, where, lines[l.n-1], what, m[1], m[2], m[3]})
	}
	one := func(s workflowStep, input string) (inputLine, bool) {
		v := s.with[input]
		if len(v) > 1 {
			t.Errorf("%s:%d: %s: with: %s has %d lines, want one value", file, s.line, s.action, input, len(v))
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
				t.Errorf("%s:%d: %s without with: image: runs its default, docker.io/tonistiigi/binfmt:latest, privileged", file, s.line, s.action)
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
				t.Errorf("%s:%d: %s: append: nodes run BuildKit images this test does not check", file, s.line, s.action)
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
					t.Errorf("%s:%d: %s without driver-opts: image=...: starts its default BuildKit, moby/buildkit:buildx-stable-1, privileged", file, s.line, s.action)
					continue
				}
				pin(*image, image.text, "the BuildKit image (setup-buildx-action driver-opts image=)")
			default:
				t.Errorf("%s:%d: %s: driver %q: this test knows which image only the docker and docker-container drivers run", file, s.line, s.action, driver)
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
					t.Errorf("%s:%d: %s: the SBOM attestation has no generator=: BuildKit runs its default scanner, docker/buildkit-syft-scanner:stable-1", file, a.n, s.action)
					continue
				}
				pin(a, generator, "the SBOM scanner (build-push-action sbom generator=)")
			}
		}
	}
	if buildx == 0 {
		t.Fatalf("%s: no docker/setup-buildx-action step found: this test no longer reads the workflow", file)
	}
	return pins
}

// allReleaseBuilderPins returns the builder image pins of every release workflow present.
func allReleaseBuilderPins(t *testing.T) []builderPin {
	t.Helper()
	var pins []builderPin
	for _, file := range releaseWorkflows(t) {
		got := releaseBuilderPins(t, file)
		if len(got) == 0 {
			t.Errorf("%s: no pinned builder image found", file)
		}
		pins = append(pins, got...)
	}
	return pins
}

// Every image the release builds run is pinned by digest, and one image has one pin across the
// release workflows (a second, stale copy would be missed by whoever bumps the first).
func TestReleaseBuilderImagesPinnedByDigest(t *testing.T) {
	first := map[string]builderPin{}
	for _, p := range allReleaseBuilderPins(t) {
		if f, ok := first[p.name]; ok && f.tag+"@"+f.digest != p.tag+"@"+p.digest {
			t.Errorf("%s: %s is %s:%s@%s, but %s pins %s@%s", p.where, p.what, p.name, p.tag, p.digest, f.where, f.tag, f.digest)
		} else if !ok {
			first[p.name] = p
		}
	}
}

// Renovate reads the pins: .github/renovate.json's regex manager matches every one on its
// workflow line, in its workflow file, with the image, tag and digest this test read, and can
// order the image's tags.
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
	for _, p := range allReleaseBuilderPins(t) {
		matched := false
		for _, mg := range managers {
			inFile := false
			for _, f := range mg.files {
				inFile = inFile || f.MatchString(p.file)
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
			t.Errorf("%s: %s %s:%s@%s: no custom regex manager of .github/renovate.json reads it in %s (depName, currentValue, currentDigest), so Renovate never updates it", p.where, p.what, p.name, p.tag, p.digest, p.file)
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

// workflowFiles lists the GitHub workflow files and, in the private repository, the Gitea ones.
func workflowFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, dir := range []string{".github/workflows", ".gitea/workflows"} {
		for _, ext := range []string{"*.yml", "*.yaml"} {
			m, err := filepath.Glob(filepath.Join("..", dir, ext))
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range m {
				rel, err := filepath.Rel("..", f)
				if err != nil {
					t.Fatal(err)
				}
				files = append(files, filepath.ToSlash(rel))
			}
		}
	}
	if !slices.Contains(files, releaseWorkflow) {
		t.Fatalf("no %s found among the workflow files %q", releaseWorkflow, files)
	}
	return files
}

var (
	usesLine = regexp.MustCompile(`^\s*(?:-\s+)?uses:\s*(\S+)(.*)$`)
	// owner/repo[/path]@<40 hex> # vX.Y.Z
	pinnedAction = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_./-]+@[0-9a-f]{40}$`)
	versionNote  = regexp.MustCompile(`^\s*#\s*v\d+\.\d+\.\d+\s*$`)
)

// An action referenced by a tag runs whatever the tag points to when the job starts, with the
// job's token and secrets (the release jobs hand theirs write access to the registry and the
// repository). A full commit SHA cannot move; the comment names its release for Renovate
// (helpers:pinGitHubActionDigestsToSemver). A container action (docker://, the Gitea Renovate job)
// is pinned by digest, its tag kept for Renovate.
func TestWorkflowActionsPinnedToCommits(t *testing.T) {
	total := 0
	for _, file := range workflowFiles(t) {
		for i, line := range strings.Split(readRepoFile(t, file), "\n") {
			m := usesLine.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			total++
			ref, rest := strings.Trim(m[1], `"'`), m[2]
			switch {
			case strings.HasPrefix(ref, "./"):
				// an action in this repository
			case strings.HasPrefix(ref, "docker://"):
				if !pinnedImage.MatchString(strings.TrimPrefix(ref, "docker://")) {
					t.Errorf("%s:%d: container action %q is not pinned by digest (docker://name:tag@sha256:...)", file, i+1, ref)
				}
			case !pinnedAction.MatchString(ref):
				t.Errorf("%s:%d: action %q is not pinned to a full commit SHA", file, i+1, ref)
			case !versionNote.MatchString(rest):
				t.Errorf("%s:%d: pinned action %q lacks its release as a comment (# vX.Y.Z)", file, i+1, ref)
			}
		}
	}
	if total == 0 {
		t.Fatal("no uses: lines found")
	}
}

var (
	govulncheckPinned = regexp.MustCompile(`go run golang\.org/x/vuln/cmd/govulncheck@v\d+\.\d+\.\d+ \./\.\.\.`)
	// A docker/check-go-version.sh command line, and its first argument.
	goVersionCheck = regexp.MustCompile(`(?m)^[^#\n]*\bsh docker/check-go-version\.sh +(\S+)`)
)

// CI and the releases must fail on known, reachable vulnerabilities in the Go code and the UI's
// runtime dependencies (govulncheck and npm audit, one govulncheck release everywhere), and a
// release must not ship an image built with an outdated Go patch release: the golang base image is
// pinned by digest, so standard library fixes need a digest bump. Branch CI only warns about that
// (docker/check-go-version.sh --warn); the release workflows fail on it.
func TestWorkflowsGateOnKnownVulnerabilities(t *testing.T) {
	files := []string{".github/workflows/ci.yml", releaseWorkflow}
	for _, f := range []string{".gitea/workflows/ci.yml", giteaReleaseWorkflow} {
		if _, ok := optionalRepoFile(t, f); ok {
			files = append(files, f)
		}
	}
	versions := map[string]string{}
	for _, file := range files {
		text := readRepoFile(t, file)
		if !govulncheckPinned.MatchString(text) {
			t.Errorf("%s: no govulncheck step with a pinned version (go run golang.org/x/vuln/cmd/govulncheck@vX.Y.Z ./...)", file)
		}
		for _, m := range govulncheckVersion.FindAllStringSubmatch(text, -1) {
			versions[m[1]] = file
		}
		if !strings.Contains(text, "npm audit --omit=dev") {
			t.Errorf("%s: no npm audit step for the web UI's runtime dependencies", file)
		}
		checks := goVersionCheck.FindAllStringSubmatch(text, -1)
		if len(checks) == 0 {
			t.Errorf("%s: the image's Go version is not checked (sh docker/check-go-version.sh)", file)
		}
		if file == releaseWorkflow || file == giteaReleaseWorkflow {
			for _, m := range checks {
				if m[1] == "--warn" {
					t.Errorf("%s: the Go version check of the release image must fail the job (no --warn): %s", file, strings.TrimSpace(m[0]))
				}
			}
		}
	}
	if len(versions) > 1 {
		t.Errorf("the workflows run more than one govulncheck release (release -> a workflow that runs it): %v", versions)
	}
}

// runScript is a run: key's script, with the line number of the key.
type runScript struct {
	line int
	text string
}

// runScripts returns every run: script of a workflow: the key's own value plus every line indented
// deeper than the key (a block scalar, or a plain value continued on the next lines).
func runScripts(text string) []runScript {
	lines := strings.Split(text, "\n")
	indent := func(l string) int { return len(l) - len(strings.TrimLeft(l, " ")) }
	key := regexp.MustCompile(`^( *)(- )?run:(.*)$`)
	var scripts []runScript
	for i, l := range lines {
		m := key.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		keyIndent := len(m[1]) + len(m[2])
		body := []string{m[3]}
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) != "" && indent(lines[j]) <= keyIndent {
				break
			}
			body = append(body, lines[j])
		}
		scripts = append(scripts, runScript{i + 1, strings.Join(body, "\n")})
	}
	return scripts
}

// The runner substitutes ${{ ... }} into a run: script before the shell reads it, so a value with
// shell syntax in it (a tag or branch name, a pull request title, a step output built from one)
// would run as code. Every run: script takes such values through env: instead, where the shell
// only ever expands them as data.
func TestWorkflowRunScriptsTakeNoExpressions(t *testing.T) {
	for _, file := range workflowFiles(t) {
		scripts := runScripts(readRepoFile(t, file))
		if len(scripts) == 0 {
			t.Errorf("%s: no run: scripts found: this test no longer reads the workflow", file)
		}
		for _, s := range scripts {
			if strings.Contains(s.text, "${{") {
				t.Errorf("%s:%d: the run: script expands an expression (${{ ... }}); pass the value through env: instead", file, s.line)
			}
		}
	}
}

// workflowStepTexts splits a workflow into its steps (the text from one "      - " item to the
// next, without comment lines); job headers and top-level keys end a step too.
func workflowStepTexts(text string) []string {
	var steps []string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			steps = append(steps, strings.Join(cur, "\n"))
		}
		cur = nil
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue // comments configure nothing
		}
		if strings.HasPrefix(line, "      - ") || (len(line) > 0 && !strings.HasPrefix(line, " ")) ||
			strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") {
			flush()
		}
		cur = append(cur, line)
	}
	flush()
	return steps
}

// The Gitea release (private repository only) publishes nothing unless the repository variable
// PUBLISH_TO_GITEA says so, and speaks plain HTTP to the LAN registry only with the
// REGISTRY_PLAIN_HTTP opt-in: plain HTTP exposes the registry token and lets anyone on the path
// swap the image, so it must never be the default. The release notes name the image digest, so
// users can pin a verified pull.
func TestReleasePlainHTTPRegistryIsOptIn(t *testing.T) {
	const file = giteaReleaseWorkflow
	text, ok := optionalRepoFile(t, file)
	if !ok {
		t.Skipf("%s is not present (public repository layout)", file)
	}
	// A publishing step also carries the PUBLISH_TO_GITEA gate (checked below) in front of the opt-in.
	optIn := regexp.MustCompile(`(?m)^\s+if: (?:needs\.settings\.outputs\.publish == 'true' && )?vars\.REGISTRY_PLAIN_HTTP == 'true'\s*$`)
	plainSteps, buildxSteps := 0, 0
	for _, step := range workflowStepTexts(text) {
		if strings.Contains(step, "docker/setup-buildx-action@") {
			buildxSteps++
		}
		if !strings.Contains(step, "http = true") && !strings.Contains(step, "insecure = true") {
			continue
		}
		plainSteps++
		if !optIn.MatchString(step) {
			t.Errorf("%s: a step configures a plain-HTTP registry without the REGISTRY_PLAIN_HTTP opt-in:\n%s", file, step)
		}
	}
	if buildxSteps < 2 || plainSteps == 0 {
		t.Errorf("%s: want an HTTPS buildx step plus an opt-in plain-HTTP one, found %d buildx step(s), %d plain-HTTP", file, buildxSteps, plainSteps)
	}
	// The opt-in must protect the tokens too, not only the push: the Docker daemon behind
	// docker/login-action falls back to plain HTTP on its own for an insecure registry, and the
	// release step sends RELEASE_TOKEN to GITHUB_API_URL, whatever its scheme.
	httpsCheck := regexp.MustCompile(`(?m)^\s+if: (?:needs\.settings\.outputs\.publish == 'true' && )?vars\.REGISTRY_PLAIN_HTTP != 'true'\s*$`)
	checked, loggedIn, guarded := false, false, false
	for _, step := range workflowStepTexts(text) {
		switch {
		case strings.Contains(step, "docker/login-action@"):
			loggedIn = true
			if !checked {
				t.Errorf("%s: docker/login-action runs before an HTTPS check of the registry (without the REGISTRY_PLAIN_HTTP opt-in)", file)
			}
		case httpsCheck.MatchString(step) && strings.Contains(step, `"https://${REGISTRY}/v2/"`):
			checked = true
		case strings.Contains(step, "secrets.RELEASE_TOKEN"):
			guarded = strings.Contains(step, "PLAIN_HTTP: ${{ vars.REGISTRY_PLAIN_HTTP }}") &&
				strings.Contains(step, "https://*) ;;") && strings.Contains(step, `if [ "${PLAIN_HTTP:-}" != true ]; then`)
		}
	}
	if !loggedIn || !guarded {
		t.Errorf("%s: the release step must refuse a plain-HTTP GITHUB_API_URL without the REGISTRY_PLAIN_HTTP opt-in (login found: %v, guard found: %v)", file, loggedIn, guarded)
	}
	// Publishing is opt-in too (repository variable PUBLISH_TO_GITEA, read once by the "publish
	// settings" job): no step may hand a secret to anything, or push, unless that job said so.
	// That job itself only sees whether each secret is set (secrets.X != '').
	publishGate := regexp.MustCompile(`(?m)^\s+if: needs\.settings\.outputs\.publish == 'true'(?: && .+)?\s*$`)
	onlyChecksSet := regexp.MustCompile(`secrets\.\w+ != ''`)
	publishing := 0
	for _, step := range workflowStepTexts(text) {
		if !strings.Contains(onlyChecksSet.ReplaceAllString(step, ""), "secrets.") && !strings.Contains(step, "push: true") {
			continue
		}
		publishing++
		if !publishGate.MatchString(step) {
			t.Errorf("%s: a step uses a secret or pushes without the PUBLISH_TO_GITEA gate (if: needs.settings.outputs.publish == 'true'):\n%s", file, step)
		}
	}
	if publishing < 3 {
		t.Errorf("%s: want at least the registry login, the push and the Gitea release among the publishing steps, found %d", file, publishing)
	}
	if !strings.Contains(text, "TOKEN: ${{ secrets.RELEASE_TOKEN || secrets.REGISTRY_TOKEN }}") {
		t.Errorf("%s: the Gitea release step must take RELEASE_TOKEN, or REGISTRY_TOKEN without it, through env: (TOKEN)", file)
	}
	if !regexp.MustCompile(`\$\{IMAGE\}:\$\{?\w+\}?@\$\{DIGEST\}`).MatchString(withoutComments(text)) {
		t.Errorf("%s: the release notes do not name the image by digest (${IMAGE}:<tag>@${DIGEST})", file)
	}
}
