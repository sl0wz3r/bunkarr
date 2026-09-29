package deploy

// Dependency tracking and SBOMs: the Renovate configuration, the release workflow's SBOMs, GitHub
// release and attestations, the workflows that run Renovate and the weekly dependency report on the
// private Gitea, and the permissions and time limits every workflow must declare. Like
// workflows_test.go, these pin properties of configuration that nothing else would notice losing.
// The public repository has no .gitea/ (scripts/public/exclude.txt): every test of a file there
// skips when it is absent, so the public CI runs this package too.

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// optionalRepoFile returns a file's content by its path from the repository root, and false when
// the file does not exist (the public repository has no .gitea/workflows).
func optionalRepoFile(t *testing.T, rel string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data), true
}

var jobHeader = regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)

// workflowJobs splits a workflow's jobs: section into its jobs, by job id.
func workflowJobs(text string) map[string]string {
	jobs := map[string]string{}
	_, body, ok := strings.Cut(text, "\njobs:\n")
	if !ok {
		return jobs
	}
	name := ""
	var cur []string
	flush := func() {
		if name != "" {
			jobs[name] = strings.Join(cur, "\n")
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if len(line) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "#") {
			break // the next top-level key
		}
		if m := jobHeader.FindStringSubmatch(line); m != nil {
			flush()
			name, cur = m[1], nil
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return jobs
}

// jobPermissions returns a job's permissions: block (scope -> access), comment lines skipped.
func jobPermissions(job string) map[string]string {
	perms := map[string]string{}
	in := false
	for _, line := range strings.Split(job, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 4:
			in = trimmed == "permissions:"
		case in && indent == 6:
			scope, access, _ := strings.Cut(trimmed, ":")
			if k := strings.Index(access, " #"); k >= 0 {
				access = access[:k]
			}
			perms[scope] = strings.TrimSpace(access)
		case indent < 6:
			in = false
		}
	}
	return perms
}

var (
	readOnlyDefault = regexp.MustCompile(`(?m)^permissions:\n  contents: read\n(?:\n|[^ ])`)
	jobTimeout      = regexp.MustCompile(`(?m)^    timeout-minutes: \d+\s*$`)
	jobWritePerm    = regexp.MustCompile(`(?m)^      [a-z-]+: write\s*$`)
)

// Every workflow's token is read-only unless a job asks for more, and every job has a time limit:
// a hung job would otherwise hold the shared self-hosted runner until the runner's own limit
// (GitHub's: 6 hours).
func TestWorkflowsDefaultToReadOnlyWithTimeouts(t *testing.T) {
	for _, file := range workflowFiles(t) {
		text := readRepoFile(t, file)
		if !readOnlyDefault.MatchString(text) {
			t.Errorf("%s: the workflow-level token must be read-only (permissions: contents: read, nothing else)", file)
		}
		jobs := workflowJobs(text)
		if len(jobs) == 0 {
			t.Errorf("%s: no jobs found", file)
		}
		for name, job := range jobs {
			if !jobTimeout.MatchString(job) {
				t.Errorf("%s: job %s has no timeout-minutes", file, name)
			}
		}
	}
}

// walkJSON calls fn for every object member of a decoded JSON document, with its path.
func walkJSON(v any, path string, fn func(path, key string, value any)) {
	switch v := v.(type) {
	case map[string]any:
		for k, val := range v {
			fn(path, k, val)
			walkJSON(val, path+"."+k, fn)
		}
	case []any:
		for _, val := range v {
			walkJSON(val, path+"[]", fn)
		}
	}
}

// Renovate reads .github/renovate.json. A syntax error there silently stops every update; a rule
// that merges on its own or stops pinning digests would undo the pins the other tests check; and
// the Go and Node.js release lines move only on purpose (go.mod, the Dockerfiles, the workflows
// together). A moved action tag waits for a tick on the Dependency Dashboard, and a
// minimumReleaseAge never reaches an update without a release date (the Renovate image on
// ghcr.io, the package-lock.json refresh), where Renovate would hold it pending for good.
func TestRenovateConfig(t *testing.T) {
	const file = ".github/renovate.json"
	text := readRepoFile(t, file)
	var tree any
	dec := json.NewDecoder(strings.NewReader(text))
	if err := dec.Decode(&tree); err != nil {
		t.Fatalf("%s: not valid JSON: %v", file, err)
	}
	if dec.More() {
		t.Fatalf("%s: data after the JSON object", file)
	}
	var cfg struct {
		Schema              string           `json:"$schema"`
		Extends             []string         `json:"extends"`
		EnabledManagers     []string         `json:"enabledManagers"`
		DependencyDashboard bool             `json:"dependencyDashboard"`
		PackageRules        []map[string]any `json:"packageRules"`
	}
	if err := json.Unmarshal([]byte(text), &cfg); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	if cfg.Schema != "https://docs.renovatebot.com/renovate-schema.json" {
		t.Errorf("%s: $schema = %q", file, cfg.Schema)
	}
	for _, want := range []string{"config:recommended", "docker:pinDigests", "helpers:pinGitHubActionDigestsToSemver"} {
		if !slices.Contains(cfg.Extends, want) {
			t.Errorf("%s: extends %q lacks %q", file, cfg.Extends, want)
		}
	}
	// custom.regex reads the builder image pins (TestRenovateUpdatesReleaseBuilderPins).
	for _, want := range []string{"dockerfile", "github-actions", "custom.regex", "gomod", "npm"} {
		if !slices.Contains(cfg.EnabledManagers, want) {
			t.Errorf("%s: enabledManagers %q lacks %q", file, cfg.EnabledManagers, want)
		}
	}
	if !cfg.DependencyDashboard {
		t.Errorf("%s: dependencyDashboard must be true", file)
	}
	walkJSON(tree, "", func(path, key string, value any) {
		switch {
		case (key == "automerge" || key == "platformAutomerge") && value == true:
			t.Errorf("%s: %s.%s is true: updates must never be merged automatically", file, path, key)
		case key == "pinDigests" && value == false:
			t.Errorf("%s: %s.pinDigests is false: images and actions stay pinned by digest", file, path)
		}
	})

	has := func(rule map[string]any, key, value string) bool {
		list, _ := rule[key].([]any)
		return slices.Contains(list, any(value))
	}
	var goDirective, lines, actionsAge, movedTag, renovateImage bool
	for i, rule := range cfg.PackageRules {
		disabled := rule["enabled"] == false
		if disabled && has(rule, "matchManagers", "gomod") && has(rule, "matchDepTypes", "golang") {
			goDirective = true
		}
		if disabled && has(rule, "matchDatasources", "docker") && has(rule, "matchPackageNames", "golang") &&
			has(rule, "matchPackageNames", "node") && has(rule, "matchUpdateTypes", "minor") {
			lines = true
		}
		if has(rule, "matchManagers", "github-actions") && rule["minimumReleaseAge"] != nil {
			actionsAge = true
		}
		if has(rule, "matchManagers", "github-actions") && has(rule, "matchUpdateTypes", "digest") &&
			rule["dependencyDashboardApproval"] == true &&
			(rule["matchDatasources"] == nil || has(rule, "matchDatasources", "github-tags")) {
			movedTag = true
		}
		// ghcr.io has no release dates: a minimumReleaseAge (this rule's or the github-actions one)
		// would leave every Renovate release pending unless missing dates are accepted.
		if has(rule, "matchPackageNames", "ghcr.io/renovatebot/renovate") {
			renovateImage = true
			if rule["minimumReleaseAgeBehaviour"] != "timestamp-optional" {
				t.Errorf("%s: packageRules[%d] (the Renovate image) needs minimumReleaseAgeBehaviour timestamp-optional: ghcr.io has no release dates", file, i)
			}
		}
		// The npm lock file refresh has no release date either; a rule with a minimumReleaseAge that
		// matches it would leave a pending renovate/stability-days status on its pull request.
		if rule["minimumReleaseAge"] != nil && (rule["matchManagers"] == nil || has(rule, "matchManagers", "npm")) &&
			rule["matchDatasources"] == nil &&
			(rule["matchUpdateTypes"] == nil || has(rule, "matchUpdateTypes", "lockFileMaintenance")) {
			t.Errorf("%s: packageRules[%d] sets minimumReleaseAge for the npm lock file refresh too: limit it with matchDatasources or matchUpdateTypes", file, i)
		}
	}
	if !goDirective {
		t.Errorf("%s: no rule keeps Renovate off go.mod's go directive (matchManagers gomod, matchDepTypes golang, enabled false)", file)
	}
	if !lines {
		t.Errorf("%s: no rule keeps the golang and node base images on their minor line", file)
	}
	if !actionsAge {
		t.Errorf("%s: no minimumReleaseAge for github-actions updates", file)
	}
	if !movedTag {
		t.Errorf("%s: no rule holds a moved action tag (github-actions, update type digest) for approval on the Dependency Dashboard", file)
	}
	if !renovateImage {
		t.Errorf("%s: no rule for the Renovate image (matchPackageNames ghcr.io/renovatebot/renovate)", file)
	}
}

// inOrder fails the test unless text contains every string of want, each one after the one
// before it (first occurrences).
func inOrder(t *testing.T, file, what, text string, want []string) {
	t.Helper()
	last := -1
	for _, w := range want {
		i := strings.Index(text, w)
		if i < 0 {
			t.Errorf("%s: %s lacks %q", file, what, w)
			continue
		}
		if i < last {
			t.Errorf("%s: in %s, %q comes too early (want the order %q)", file, what, w, want)
		}
		last = i
	}
}

// The public release publishes SBOMs of the source and of each image platform, in both formats,
// made by a job that can only read, after the image job released the digest they describe. The
// github-release job writes checksums.txt before the attestation step, whose subjects are every
// file it publishes (the SBOMs and checksums.txt itself), and names the image by digest in the
// notes. Write permissions stay with the jobs that need them: packages for the candidate push and
// the release tags, the OIDC token and the attestation store for the two attestations, contents
// for the release.
func TestReleaseWorkflowPublishesSBOMs(t *testing.T) {
	const file = ".github/workflows/release.yml"
	text := readRepoFile(t, file)
	jobs := workflowJobs(text)
	candidate, image, sbom, release := jobs["candidate"], jobs["image"], jobs["sbom"], jobs["github-release"]
	if candidate == "" || image == "" || sbom == "" || release == "" {
		t.Fatalf("%s: want the jobs candidate, image, sbom and github-release", file)
	}
	needs := func(job, name string) bool {
		return regexp.MustCompile(`(?m)^    needs: \[[^\]\n]*\b` + name + `\b[^\]\n]*\]`).MatchString(job)
	}
	if !needs(sbom, "image") {
		t.Errorf("%s: the sbom job must wait for the image job (the image SBOMs describe the released digest)", file)
	}
	if !needs(release, "image") || !needs(release, "sbom") {
		t.Errorf("%s: the github-release job must wait for the image and sbom jobs", file)
	}

	for _, want := range []string{
		"uses: anchore/sbom-action/download-syft@",
		"scan dir:. ",
		`"registry:${IMAGE}@${DIGEST}"`,
		"for platform in linux/amd64 linux/arm64",
		"spdx-json@2.3=",
		"cyclonedx-json@1.6=",
		"_source.spdx.json",
		"_source.cdx.json",
		"bunkarr_${version}_image_",
		"uses: actions/upload-artifact@",
		"name: sbom",
		"if-no-files-found: error",
	} {
		if !strings.Contains(sbom, want) {
			t.Errorf("%s: the sbom job lacks %q", file, want)
		}
	}
	inOrder(t, file, "the github-release job", release, []string{
		"uses: actions/download-artifact@",
		"name: sbom",
		"sha256sum bunkarr_* > checksums.txt",
		"subject-path: |\n            dist/*.spdx.json\n            dist/*.cdx.json\n            dist/checksums.txt\n",
		"${IMAGE}:${VERSION}@${DIGEST}",
		"gh attestation verify oci://${IMAGE}:${VERSION}",
		"dist/*.spdx.json dist/*.cdx.json dist/checksums.txt",
		"gh release upload",
		"--clobber",
		"gh release create",
	})
	for _, want := range []string{"--verify-tag", "--prerelease --latest=false", "GH_TOKEN: ${{ github.token }}", "section Unreleased"} {
		if !strings.Contains(release, want) {
			t.Errorf("%s: the github-release job lacks %q", file, want)
		}
	}
	// The image attestation is on the tested digest, and the optional signature too (never a tag,
	// which could move between the push and the signature).
	for _, want := range []string{
		"uses: actions/attest@",
		"subject-digest: ${{ needs.candidate.outputs.digest }}",
		"push-to-registry: true",
		`cosign sign --yes "${IMAGE}@${DIGEST}"`,
	} {
		if !strings.Contains(image, want) {
			t.Errorf("%s: the image job lacks %q", file, want)
		}
	}
	// COSIGN_SIGN signs only when exactly "true": if: compares case-insensitively and the shell does
	// not, so one shell step reads the variable and the cosign steps and the notes read its output.
	if got := strings.Count(image, "if: steps.cosign.outputs.sign == 'true'"); got != 2 {
		t.Errorf("%s: want the cosign install and signing steps both behind if: steps.cosign.outputs.sign == 'true', found %d", file, got)
	}
	for _, want := range []string{`if [ "${COSIGN_SIGN:-}" = true ]; then`, "signed: ${{ steps.cosign.outputs.sign }}"} {
		if !strings.Contains(image, want) {
			t.Errorf("%s: the image job lacks %q", file, want)
		}
	}
	if !strings.Contains(release, "SIGNED: ${{ needs.image.outputs.signed }}") {
		t.Errorf("%s: the release notes must read the image job's signed output, not the variable", file)
	}
	if got := strings.Count(text, "vars.COSIGN_SIGN"); got != 1 {
		t.Errorf("%s: vars.COSIGN_SIGN appears %d times: only the image job's signing setting step may read it", file, got)
	}
	// BuildKit's in-image SBOM and provenance stay as well.
	for _, want := range []string{"sbom: generator=", "provenance: mode=max"} {
		if !strings.Contains(candidate, want) {
			t.Errorf("%s: the candidate job lacks %q", file, want)
		}
	}

	allowed := map[string][]string{
		"candidate":      {"packages"},
		"image":          {"packages", "id-token", "attestations"},
		"github-release": {"contents", "id-token", "attestations"},
	}
	for name, job := range jobs {
		for scope, access := range jobPermissions(job) {
			if access == "write" && !slices.Contains(allowed[name], scope) {
				t.Errorf("%s: job %s has %s: write, which only the jobs %v need", file, name, scope, allowed)
			}
		}
	}
	for name, scopes := range allowed {
		perms := jobPermissions(jobs[name])
		for _, scope := range scopes {
			if perms[scope] != "write" {
				t.Errorf("%s: job %s needs %s: write (has %q)", file, name, scope, perms[scope])
			}
		}
	}
	if m := jobWritePerm.FindString(sbom); m != "" {
		t.Errorf("%s: the sbom job needs no write permission: %s", file, strings.TrimSpace(m))
	}
}

// withoutComments drops a YAML file's comment lines (not trailing comments, such as a pin's version).
func withoutComments(text string) string {
	var kept []string
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

var (
	scheduled     = regexp.MustCompile(`(?m)^  schedule:\n(?:\s*#.*\n)*    - cron: "[^"]+"\s*$`)
	dispatchable  = regexp.MustCompile(`(?m)^  workflow_dispatch:\s*$`)
	secretEnvLine = regexp.MustCompile(`^\s+[A-Z_]+: \$\{\{ secrets\.[A-Z_]+(?: != '')? \}\}\s*$`)
)

// Renovate and the dependency report run on the private Gitea only. Renovate works on this
// repository alone and gets its token only after a settings check that refuses plain HTTP without
// the REGISTRY_PLAIN_HTTP opt-in; the report needs no secret at all and uploads nothing.
func TestGiteaDependencyWorkflows(t *testing.T) {
	renovate, okRenovate := optionalRepoFile(t, ".gitea/workflows/renovate.yml")
	report, okReport := optionalRepoFile(t, ".gitea/workflows/dependency-report.yml")
	if !okRenovate && !okReport {
		t.Skip(".gitea/workflows is not present (public repository layout)")
	}
	if !okRenovate || !okReport {
		t.Fatalf("want both .gitea/workflows/renovate.yml and dependency-report.yml (found renovate: %v, report: %v)", okRenovate, okReport)
	}
	for file, text := range map[string]string{"renovate.yml": renovate, "dependency-report.yml": report} {
		if !scheduled.MatchString(text) || !dispatchable.MatchString(text) {
			t.Errorf("%s: want a schedule and workflow_dispatch", file)
		}
	}

	for _, want := range []string{
		"uses: docker://ghcr.io/renovatebot/renovate:",
		"RENOVATE_PLATFORM: gitea",
		"RENOVATE_REPOSITORIES: ${{ github.repository }}",
		`RENOVATE_AUTODISCOVER: "false"`,
		`RENOVATE_ONBOARDING: "false"`,
		"PLAIN_HTTP: ${{ vars.REGISTRY_PLAIN_HTTP }}",
		"https://*) ;;",
		`if [ "${PLAIN_HTTP:-}" != true ]; then`,
		`if [ "$HAS_TOKEN" != true ]; then`,
		"::notice::",
	} {
		if !strings.Contains(renovate, want) {
			t.Errorf("renovate.yml: missing %q", want)
		}
	}
	// Secrets only ever reach a step through an env: mapping (never interpolated into a script), and
	// the token only reaches the Renovate step, which runs only when the settings check said so.
	for i, line := range strings.Split(renovate, "\n") {
		if strings.Contains(line, "secrets.") && !strings.HasPrefix(strings.TrimSpace(line), "#") && !secretEnvLine.MatchString(line) {
			t.Errorf("renovate.yml:%d: a secret outside an env: mapping: %s", i+1, strings.TrimSpace(line))
		}
	}
	tokenSteps := 0
	for _, step := range workflowStepTexts(renovate) {
		if !strings.Contains(step, "${{ secrets.RENOVATE_TOKEN }}") {
			continue
		}
		tokenSteps++
		if !strings.Contains(step, "if: steps.check.outputs.run == 'true'") || !strings.Contains(step, "uses: docker://") {
			t.Errorf("renovate.yml: the step that gets RENOVATE_TOKEN must be the Renovate container step, gated by the settings check:\n%s", step)
		}
	}
	if tokenSteps != 1 {
		t.Errorf("renovate.yml: RENOVATE_TOKEN reaches %d steps, want exactly the Renovate step", tokenSteps)
	}
	// Nothing may let repository content run code next to that token. A checkout would put the
	// repository in Renovate's working directory, and Renovate loads a config.js found there as admin
	// configuration, running it as JavaScript. The admin-only settings below let the repository
	// config run commands, scripts or plugins, or see the environment; NODE_OPTIONS can load code
	// into Renovate's own process.
	code := withoutComments(renovate)
	if strings.Contains(code, "actions/checkout") {
		t.Error("renovate.yml: the Renovate job must not check out the repository (Renovate would load a config.js from it as admin configuration)")
	}
	for _, name := range []string{
		"RENOVATE_CONFIG", "RENOVATE_CONFIG_FILE", "RENOVATE_ADDITIONAL_CONFIG_FILE",
		"RENOVATE_ALLOWED_COMMANDS", "RENOVATE_ALLOWED_POST_UPGRADE_COMMANDS",
		"RENOVATE_ALLOW_SHELL_EXECUTOR_FOR_POST_UPGRADE_COMMANDS", "RENOVATE_ALLOWED_UNSAFE_EXECUTIONS",
		"RENOVATE_ALLOW_SCRIPTS", "RENOVATE_ALLOW_PLUGINS", "RENOVATE_EXPOSE_ALL_ENV", "RENOVATE_ALLOWED_ENV",
		"NODE_OPTIONS",
	} {
		if regexp.MustCompile(`\b` + name + `\b`).MatchString(code) {
			t.Errorf("renovate.yml: sets %s, which lets repository content run code next to RENOVATE_TOKEN", name)
		}
	}

	if strings.Contains(report, "secrets.") {
		t.Error("dependency-report.yml: the report needs no secret")
	}
	for _, bad := range []string{"upload-artifact", "push: true", "curl -X POST", "contents: write"} {
		if strings.Contains(report, bad) {
			t.Errorf("dependency-report.yml: the report must not publish anything (%q)", bad)
		}
	}
	if !strings.Contains(report, "sh scripts/dependency-report.sh") {
		t.Error("dependency-report.yml: does not run scripts/dependency-report.sh")
	}
}

var govulncheckVersion = regexp.MustCompile(`golang\.org/x/vuln/cmd/govulncheck@(v\d+\.\d+\.\d+)`)

// The dependency report runs the same govulncheck release as CI, so both judge the code alike.
func TestDependencyReportScript(t *testing.T) {
	const file = "scripts/dependency-report.sh"
	script := readRepoFile(t, file)
	if !strings.HasPrefix(script, "#!/bin/sh\n") || !regexp.MustCompile(`(?m)^set -eu$`).MatchString(script) {
		t.Errorf("%s: want a POSIX sh script with set -eu", file)
	}
	m := govulncheckVersion.FindStringSubmatch(script)
	ci := govulncheckVersion.FindStringSubmatch(readRepoFile(t, ".github/workflows/ci.yml"))
	if m == nil || ci == nil || m[1] != ci[1] {
		t.Errorf("%s: govulncheck %v, CI %v: keep them in step", file, m, ci)
	}
}
