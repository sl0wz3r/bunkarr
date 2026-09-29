#!/bin/sh
# shellcheck disable=SC2016 # backticks in printf formats are Markdown code spans, not expansions
# Dependency report: which dependencies have newer releases, and whether a known vulnerability
# reaches the code. Markdown on stdout, progress and errors on stderr. It runs weekly in the
# maintainer's CI and by hand from the repository root:
#
#   (cd web && npm ci) && sh scripts/dependency-report.sh > dependency-report.md
#
# Sections: Go modules with a newer release (go list -m -u, direct requirements in a table, indirect
# ones folded away); the Go and Node.js release lines against the newest releases (go list -m -u go,
# go.dev, nodejs.org); the web UI's outdated npm packages (npm outdated); the base images whose tag
# now points at a newer digest than the one the Dockerfiles pin, and the Go release the pinned
# golang image builds with (what docker/check-go-version.sh checks on a built image, here read from
# the registry); the restic and rclone versions of docker/engines/versions.env against the packages
# the image's alpine release ships now (its package index); govulncheck; npm audit.
#
# Exit status: 0 however many updates are available; 1 when govulncheck finds a vulnerability the
# code reaches, or npm audit --omit=dev an advisory of moderate or higher severity in the UI's
# runtime packages (the gates CI has); 2 when a check could not run (network, tool error). The
# report is complete in every case and says which check failed.
#
# Needs: go (the toolchain govulncheck checks; in CI the latest patch release of the project's Go
# line), npm with web/node_modules installed (npm ci), jq, curl, tar. Network: proxy.golang.org,
# vuln.go.dev, go.dev, nodejs.org, registry.npmjs.org, Docker Hub (anonymous),
# dl-cdn.alpinelinux.org.
set -eu

# Keep in step with the CI workflows' govulncheck.
GOVULNCHECK=${GOVULNCHECK:-golang.org/x/vuln/cmd/govulncheck@v1.8.0}

cd "$(dirname "$0")/.."
for tool in go npm jq curl tar; do
	if ! command -v "$tool" > /dev/null 2>&1; then
		echo "dependency-report.sh: $tool is required" >&2
		exit 2
	fi
done

TMP=$(mktemp -d)
# shellcheck disable=SC2317,SC2329 # run by the EXIT trap
cleanup() {
	rm -rf "$TMP"
}
trap cleanup EXIT

STATUS=0
SUMMARY=$TMP/summary.md
BODY=$TMP/body.md
: > "$SUMMARY"
: > "$BODY"

log() {
	echo "dependency-report.sh: $*" >&2
}
row() {
	printf '| %s | %s |\n' "$1" "$2" >> "$SUMMARY"
}
# broken CHECK [ERRFILE]: a check could not run. The report says so; the script exits 2.
broken() {
	log "$1 could not run"
	if [ -s "${2:-/dev/null}" ]; then
		sed 's/^/  /' "$2" >&2
	fi
	STATUS=2
	row "$1" "**could not run** (the error is in the log)"
	printf '## %s\n\n**Could not run**: the error is in the log (stderr).\n\n' "$1" >> "$BODY"
}
# gate: a vulnerability gate failed (exit 1, unless a check could not run).
gate() {
	if [ "$STATUS" = 0 ]; then
		STATUS=1
	fi
}
fetch() {
	curl -fsSL --retry 3 --retry-delay 2 --max-time 60 "$@"
}

# --- Go modules ----------------------------------------------------------------------------------
go_modules() {
	log "go list -m -u"
	if ! go list -m -u -retracted -json all > "$TMP/gomod.json" 2> "$TMP/gomod.err"; then
		broken "Go modules" "$TMP/gomod.err"
		return 0
	fi
	# Direct: the requirements go.mod lists without "// indirect". A retracted or deprecated
	# version is listed even without a newer release.
	jq -s '[.[] | select(.Main != true)
		| .note = ([(if .Retracted then "retracted: " + (.Retracted | join("; ")) else empty end),
			(if .Deprecated then "deprecated: " + .Deprecated else empty end)]
			| join("; ") | gsub("[|\n\r]"; " "))]' "$TMP/gomod.json" > "$TMP/mods.json"
	listed=$(jq '[.[] | select(.Indirect != true and (.Update != null or .note != ""))] | length' "$TMP/mods.json")
	# go list -m -u reports nothing, and no error, when it cannot reach the module proxy: ask the
	# proxy for one direct requirement's newest version, which fails loudly.
	probe=$(jq -r '[.[] | select(.Indirect != true)][0].Path // empty' "$TMP/mods.json")
	if [ -n "$probe" ] && ! go list -m -json "$probe@latest" > /dev/null 2> "$TMP/gomod.err"; then
		broken "Go modules" "$TMP/gomod.err"
		return 0
	fi
	direct=$(jq '[.[] | select(.Indirect != true and .Update != null)] | length' "$TMP/mods.json")
	indirect=$(jq '[.[] | select(.Indirect == true and .Update != null)] | length' "$TMP/mods.json")
	flagged=$(jq '[.[] | select(.note != "")] | length' "$TMP/mods.json")
	summary="$direct direct, $indirect indirect with a newer release"
	if [ "$flagged" -gt 0 ]; then
		summary="$summary; **$flagged retracted or deprecated**"
	fi
	row "Go modules" "$summary"
	{
		printf '## Go modules\n\n'
		if [ "$listed" -gt 0 ]; then
			printf '| Module | Current | Newest | Note |\n|---|---|---|---|\n'
			jq -r '.[] | select(.Indirect != true and (.Update != null or .note != ""))
				| "| `\(.Path)` | \(.Version) | \(.Update.Version // "") | \(.note) |"' "$TMP/mods.json"
			printf '\n'
		else
			printf 'No direct requirement has a newer release.\n\n'
		fi
		if [ "$indirect" -gt 0 ]; then
			printf '<details><summary>%s indirect module(s) with a newer release</summary>\n\n' "$indirect"
			printf 'Indirect modules follow the direct ones that need them (`go get <direct>@<version>`, then `go mod tidy`): do not update them on their own. `modernc.org/libc` in particular must stay at the version `modernc.org/sqlite` requires.\n\n'
			printf '| Module | Current | Newest | Note |\n|---|---|---|---|\n'
			jq -r '.[] | select(.Indirect == true and .Update != null)
				| "| `\(.Path)` | \(.Version) | \(.Update.Version) | \(.note) |"' "$TMP/mods.json"
			printf '\n</details>\n\n'
		fi
	} >> "$BODY"
}

# --- Go and Node.js release lines ----------------------------------------------------------------
toolchains() {
	log "Go and Node.js releases"
	directive=$(sed -n 's/^go[[:space:]]\{1,\}\([0-9][0-9.]*\)[[:space:]]*$/\1/p' go.mod | head -n 1)
	toolchain=$(sed -n 's/^toolchain[[:space:]]\{1,\}\(go[0-9][0-9.]*\)[[:space:]]*$/\1/p' go.mod | head -n 1)
	goline=$(printf '%s' "$directive" | cut -d. -f1,2)
	# The Node.js line: the Dockerfile's node image tag (node:24-alpine), which the workflows match.
	nodeline=$(sed -n 's/^FROM.*[[:space:]]node:\([0-9]\{1,\}\)[-@].*/\1/p' Dockerfile | head -n 1)
	if ! go list -m -u -json go > "$TMP/go-directive.json" 2> "$TMP/go-directive.err" ||
		! fetch 'https://go.dev/dl/?mode=json' > "$TMP/go-dl.json" 2> "$TMP/go-dl.err" ||
		! fetch 'https://nodejs.org/dist/index.json' > "$TMP/node-dist.json" 2> "$TMP/node-dist.err"; then
		cat "$TMP/go-directive.err" "$TMP/go-dl.err" "$TMP/node-dist.err" > "$TMP/toolchains.err" 2> /dev/null || true
		broken "Go and Node.js releases" "$TMP/toolchains.err"
		return 0
	fi
	gonewest=$(jq -r '[.[] | select(.stable)][0].version // empty' "$TMP/go-dl.json")
	golatest=$(jq -r --arg p "go${goline}." '[.[] | select(.stable and (.version | startswith($p)))][0].version // empty' "$TMP/go-dl.json")
	goupdate=$(jq -r '.Update.Version // empty' "$TMP/go-directive.json")
	nodelatest=$(jq -r --arg p "v${nodeline}." '[.[] | select(.version | startswith($p))][0].version // empty' "$TMP/node-dist.json")
	nodelts=$(jq -r '[.[] | select(.lts != false)][0] | "\(.version) (\(.lts))"' "$TMP/node-dist.json")
	nodenewest=$(jq -r '.[0].version' "$TMP/node-dist.json")
	gopath=$(go env GOVERSION)
	nodepath=$(node --version 2> /dev/null || echo "not installed")

	if [ -z "$golatest" ]; then
		gonote="**Go ${goline} is no longer supported** (go.dev lists only the two newest lines): move to ${gonewest%.*}"
	elif [ "${gonewest%.*}" != "go${goline}" ]; then
		gonote="Go ${goline} is still supported; **${gonewest%.*} is out** (moving is deliberate: go.mod, the Dockerfiles' golang tag, the workflows' go-version)"
	else
		gonote="Go ${goline} is the newest line"
	fi
	nodemajor=$(printf '%s' "$nodelts" | sed 's/^v\([0-9]*\).*/\1/')
	if [ -n "$nodemajor" ] && [ "$nodemajor" -gt "${nodeline:-0}" ]; then
		nodenote="**a newer LTS line is out: ${nodelts}** (moving is deliberate: the Dockerfile's node tag, the workflows' node-version, @types/node)"
	else
		nodenote="Node.js ${nodeline} is the newest LTS line"
	fi
	row "Go release line" "$gonote"
	row "Node.js release line" "$nodenote"
	{
		printf '## Go and Node.js release lines\n\n'
		printf '| | Version | Note |\n|---|---|---|\n'
		printf '| go.mod `go` directive | %s | The minimum Go version for the module; `go list -m -u go`: %s |\n' \
			"$directive" "${goupdate:-no newer release}"
		printf '| go.mod `toolchain` directive | %s | None by design: CI and the Dockerfiles use the latest patch release of the line |\n' \
			"${toolchain:-none}"
		printf '| Latest Go %s patch release | %s | What CI (setup-go with check-latest) and the golang image should build with |\n' \
			"$goline" "${golatest:-none}"
		printf '| Newest Go release | %s | %s |\n' "$gonewest" "$gonote"
		printf '| Go on PATH (this run) | %s | |\n' "$gopath"
		printf '| Latest Node.js %s release | %s | The web UI build (Dockerfile, workflows) |\n' "$nodeline" "${nodelatest:-none}"
		printf '| Newest Node.js LTS | %s | %s |\n' "$nodelts" "$nodenote"
		printf '| Newest Node.js release | %s | |\n' "$nodenewest"
		printf '| Node.js on PATH (this run) | %s | |\n\n' "$nodepath"
	} >> "$BODY"
}

# --- Base images -----------------------------------------------------------------------------------
INDEX_TYPES=application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json
MANIFEST_TYPES=application/vnd.oci.image.manifest.v1+json,application/vnd.docker.distribution.manifest.v2+json

# registry_token REPO: an anonymous pull token for a Docker Hub repository (library/golang).
registry_token() {
	fetch "https://auth.docker.io/token?service=registry.docker.io&scope=repository:$1:pull" | jq -er .token
}
# tag_digest REPO TAG TOKEN: the digest the tag points at now (a HEAD request: no pull counted).
tag_digest() {
	curl -fsSI --retry 3 --retry-delay 2 --max-time 60 -H "Authorization: Bearer $3" \
		-H "Accept: $INDEX_TYPES, $MANIFEST_TYPES" \
		"https://registry-1.docker.io/v2/$1/manifests/$2" |
		tr -d '\r' | sed -n 's/^[Dd]ocker-[Cc]ontent-[Dd]igest:[[:space:]]*//p'
}
# image_env REPO DIGEST TOKEN NAME: the value of an environment variable in the linux/amd64 image of
# a pinned index (GOLANG_VERSION, NODE_VERSION).
image_env() {
	manifest=$(fetch -H "Authorization: Bearer $3" -H "Accept: $INDEX_TYPES" \
		"https://registry-1.docker.io/v2/$1/manifests/$2" |
		jq -er '[.manifests[] | select(.platform.os == "linux" and .platform.architecture == "amd64")][0].digest')
	config=$(fetch -H "Authorization: Bearer $3" -H "Accept: $MANIFEST_TYPES" \
		"https://registry-1.docker.io/v2/$1/manifests/$manifest" | jq -er .config.digest)
	fetch -H "Authorization: Bearer $3" "https://registry-1.docker.io/v2/$1/blobs/$config" |
		jq -er --arg n "$4=" '.config.Env[] | select(startswith($n)) | ltrimstr($n)'
}

base_images() {
	log "base images"
	tab=$(printf '\t')
	# "file<TAB>image:tag@sha256:..." for every digest-pinned FROM and # syntax= line.
	for file in Dockerfile docker/engines/Dockerfile; do
		[ -f "$file" ] || continue
		awk -v f="$file" '
			/^# syntax=/ { sub(/^# syntax=/, ""); print f "\t" $1; next }
			toupper($1) == "FROM" {
				for (i = 2; i <= NF; i++) if ($i !~ /^--/) { if ($i ~ /@sha256:/) print f "\t" $i; break }
			}' "$file"
	done > "$TMP/images.tsv"
	total=0
	behind=0
	unchecked=0
	errors=0
	gocheck=""
	: > "$TMP/images.md"
	: > "$TMP/images.err"
	# One row per image reference, with the files that pin it.
	cut -f2 "$TMP/images.tsv" | sort -u > "$TMP/refs.txt"
	while IFS= read -r ref; do
		total=$((total + 1))
		files=$(awk -F "$tab" -v r="$ref" '$2 == r { printf "%s%s", sep, $1; sep = ", " }' "$TMP/images.tsv")
		name=${ref%%@*}
		pinned=${ref#*@}
		case "${name##*/}" in
			*:*) ;;
			*)
				unchecked=$((unchecked + 1))
				printf '| `%s` | %s | `%.19s…` | not checked (no tag to compare with) |\n' "$name" "$files" "$pinned" >> "$TMP/images.md"
				continue
				;;
		esac
		tag=${name##*:}
		repo=${name%:*}
		case "$repo" in
			*.*/* | *:*/* | localhost/*)
				unchecked=$((unchecked + 1))
				printf '| `%s` | %s | `%.19s…` | not checked (only Docker Hub images are) |\n' "$name" "$files" "$pinned" >> "$TMP/images.md"
				continue
				;;
			*/*) ;;
			*) repo="library/$repo" ;;
		esac
		if ! token=$(registry_token "$repo" 2> "$TMP/registry.err") ||
			! current=$(tag_digest "$repo" "$tag" "$token" 2>> "$TMP/registry.err") || [ -z "$current" ]; then
			errors=$((errors + 1))
			cat "$TMP/registry.err" >> "$TMP/images.err"
			printf '| `%s` | %s | `%.19s…` | **could not check** |\n' "$name" "$files" "$pinned" >> "$TMP/images.md"
			continue
		fi
		if [ "$current" = "$pinned" ]; then
			state="current"
		else
			behind=$((behind + 1))
			state="**newer digest** \`$(printf '%.19s' "$current")…\` (the tag moved, usually a rebuild with fixes)"
		fi
		# The Go release compiled into the binary is the pinned golang image's (the release workflow
		# refuses an image built with an older patch release: docker/check-go-version.sh).
		case "$repo" in
			library/golang)
				if built=$(image_env "$repo" "$pinned" "$token" GOLANG_VERSION 2>> "$TMP/images.err"); then
					latest=$(jq -r --arg p "go${built%.*}." '[.[] | select(.stable and (.version | startswith($p)))][0].version // empty' "$TMP/go-dl.json" 2> /dev/null || true)
					if [ -n "$latest" ] && [ "go$built" != "$latest" ]; then
						state="$state; builds with **go$built**, $latest is out: the release workflows refuse this digest, bump it"
						gocheck="the pinned golang image builds with **go$built** ($latest is out)"
					else
						state="$state; builds with go$built"
					fi
				else
					errors=$((errors + 1))
					state="$state; **could not read its Go version**"
				fi
				;;
			library/node)
				if built=$(image_env "$repo" "$pinned" "$token" NODE_VERSION 2>> "$TMP/images.err"); then
					state="$state; Node.js $built"
				fi
				;;
		esac
		printf '| `%s` | %s | `%.19s…` | %s |\n' "$name" "$files" "$pinned" "$state" >> "$TMP/images.md"
	done < "$TMP/refs.txt"
	summary="$behind of $total pinned digest(s) behind their tag"
	if [ -n "$gocheck" ]; then
		summary="$summary; $gocheck"
	fi
	if [ "$errors" -gt 0 ]; then
		# The table below still shows what could be checked.
		log "base images: $errors check(s) could not run"
		sed 's/^/  /' "$TMP/images.err" >&2 2> /dev/null || true
		STATUS=2
		summary="$summary; **$errors check(s) could not run** (the error is in the log)"
	fi
	row "Base images" "$summary"
	{
		printf '## Base images\n\n'
		printf 'Pinned by digest (`image:tag@sha256:...`); a newer digest behind the same tag usually carries security fixes (Renovate groups them as "base image digests").\n\n'
		printf '| Image | Pinned in | Pinned digest | Status |\n|---|---|---|---|\n'
		cat "$TMP/images.md"
		if [ "$unchecked" -gt 0 ]; then
			printf '\n%s image(s) outside Docker Hub were not checked.\n' "$unchecked"
		fi
		printf '\n'
	} >> "$BODY"
}

# --- restic and rclone -------------------------------------------------------------------------------
# The image installs the alpine packages at the upstream versions of docker/engines/versions.env
# (apk add "restic~X.Y.Z"), and an alpine release's index keeps only the newest build of each
# package: once the release moves a package to another upstream version, the pinned one is gone and
# every image build fails until versions.env follows (after make test-engines and make
# test-offsite, docs/design/phase4.md §20.3). Read from the index itself, for both platforms.
engines() {
	log "restic and rclone packages"
	# The runtime stage's base (the Dockerfile's last FROM alpine:X.Y@sha256:... line).
	alpine=$(sed -n 's/^FROM[[:space:]]\{1,\}alpine:\([0-9]\{1,\}\.[0-9]\{1,\}\)@.*/\1/p' Dockerfile | tail -n 1)
	# Plain NAME=value lines (the file says so): read, not sourced.
	restic=$(sed -n 's/^RESTIC_VERSION=//p' docker/engines/versions.env)
	rclone=$(sed -n 's/^RCLONE_VERSION=//p' docker/engines/versions.env)
	if [ -z "$alpine" ] || [ -z "$restic" ] || [ -z "$rclone" ]; then
		echo "want a FROM alpine:X.Y@sha256:... line in the Dockerfile and RESTIC_VERSION, RCLONE_VERSION in docker/engines/versions.env" > "$TMP/apk.err"
		broken "restic and rclone" "$TMP/apk.err"
		return 0
	fi
	: > "$TMP/apk.tsv"
	for arch in x86_64 aarch64; do
		if ! fetch "https://dl-cdn.alpinelinux.org/alpine/v$alpine/community/$arch/APKINDEX.tar.gz" > "$TMP/apkindex.tar.gz" 2> "$TMP/apk.err" ||
			! tar -xzOf "$TMP/apkindex.tar.gz" APKINDEX > "$TMP/apkindex" 2> "$TMP/apk.err"; then
			broken "restic and rclone" "$TMP/apk.err"
			return 0
		fi
		# "arch<TAB>package<TAB>version"; in each record of the index, P: comes before V:.
		awk -v a="$arch" '/^P:/ { p = substr($0, 3) }
			/^V:/ && (p == "restic" || p == "rclone") { print a "\t" p "\t" substr($0, 3) }' "$TMP/apkindex" >> "$TMP/apk.tsv"
	done
	moved=0
	: > "$TMP/apk.md"
	for pkg in restic rclone; do
		if [ "$pkg" = restic ]; then pinned=$restic; else pinned=$rclone; fi
		cells=""
		state="current"
		for arch in x86_64 aarch64; do
			v=$(awk -F '\t' -v a="$arch" -v p="$pkg" '$1 == a && $2 == p { print $3; exit }' "$TMP/apk.tsv")
			cells="$cells | ${v:-missing}"
			if [ "$state" != current ]; then
				continue
			elif [ -z "$v" ]; then
				state="**not in the $arch index**: image builds fail"
			elif [ "${v%-r*}" != "$pinned" ]; then
				state="**alpine v$alpine ships ${v%-r*}**: \`$pkg~$pinned\` no longer resolves, so image builds fail until versions.env moves"
			fi
		done
		if [ "$state" != current ]; then
			moved=$((moved + 1))
		fi
		printf '| %s | %s%s | %s |\n' "$pkg" "$pinned" "$cells" "$state" >> "$TMP/apk.md"
	done
	if [ "$moved" -gt 0 ]; then
		row "restic and rclone" "**$moved of 2 no longer at the pinned version in alpine v$alpine**: image builds fail"
	else
		row "restic and rclone" "restic $restic and rclone $rclone: current in alpine v$alpine"
	fi
	{
		printf '## restic and rclone\n\n'
		printf 'The image installs the alpine v%s packages at the upstream versions of `docker/engines/versions.env` (`apk add "restic~X.Y.Z"`). An alpine release keeps only the newest build of each package: once it moves to another upstream version, image builds fail until versions.env follows (run make test-engines and make test-offsite first). Upstream releases alpine does not ship yet are not listed.\n\n' "$alpine"
		printf '| Package | versions.env | alpine v%s x86_64 | alpine v%s aarch64 | Status |\n|---|---|---|---|---|\n' "$alpine" "$alpine"
		cat "$TMP/apk.md"
		printf '\n'
	} >> "$BODY"
}

# --- npm ---------------------------------------------------------------------------------------------
npm_outdated() {
	log "npm outdated"
	if [ ! -d web/node_modules ]; then
		echo "web/node_modules is missing: run npm ci in web/ first" > "$TMP/npm-outdated.err"
		broken "npm packages" "$TMP/npm-outdated.err"
		return 0
	fi
	# Exit status 1 only means "something is outdated": the JSON tells an error apart.
	rc=0
	(cd web && npm outdated --long --json) > "$TMP/npm-outdated.json" 2> "$TMP/npm-outdated.err" || rc=$?
	if [ "$rc" -gt 1 ] || ! jq -e 'type == "object" and (has("error") | not)' "$TMP/npm-outdated.json" > /dev/null 2>&1; then
		cat "$TMP/npm-outdated.json" >> "$TMP/npm-outdated.err" 2> /dev/null || true
		broken "npm packages" "$TMP/npm-outdated.err"
		return 0
	fi
	# A package installed at several places is listed once per place: take the first.
	jq '[to_entries[] | {name: .key} + (.value | if type == "array" then .[0] else . end)]
		| sort_by((if .type == "dependencies" then 0 else 1 end), .name)' "$TMP/npm-outdated.json" > "$TMP/npm.json"
	runtime=$(jq '[.[] | select(.type == "dependencies")] | length' "$TMP/npm.json")
	tooling=$(jq '[.[] | select(.type != "dependencies")] | length' "$TMP/npm.json")
	row "npm packages" "$runtime runtime, $tooling build/test tooling with a newer release"
	{
		printf '## npm packages (web/)\n\n'
		if [ "$((runtime + tooling))" -gt 0 ]; then
			printf '*Wanted*: the newest release package.json allows (`npm update`); *Newest*: the newest release (a new major needs a package.json change).\n\n'
			printf '| Package | Type | Current | Wanted | Newest |\n|---|---|---|---|---|\n'
			jq -r '.[] | "| `\(.name)` | \(if .type == "dependencies" then "runtime" else "tooling" end) | \(.current // "missing") | \(.wanted) | \(.latest) |"' "$TMP/npm.json"
			printf '\n'
		else
			printf 'Every package is at its newest release.\n\n'
		fi
	} >> "$BODY"
}

# --- Vulnerabilities -----------------------------------------------------------------------------------
vuln_go() {
	log "govulncheck"
	rc=0
	go run "$GOVULNCHECK" ./... > "$TMP/govulncheck.txt" 2>&1 || rc=$?
	case "$rc" in
		0) verdict="no vulnerability the code reaches" ;;
		3)
			gate
			verdict="**vulnerabilities the code reaches**: update the module (or the Go toolchain) named below"
			;;
		*)
			broken "govulncheck" "$TMP/govulncheck.txt"
			return 0
			;;
	esac
	row "govulncheck" "$verdict"
	{
		printf '## govulncheck\n\nResult: %s.\n\n' "$verdict"
		printf '<details><summary>govulncheck output</summary>\n\n```text\n'
		cat "$TMP/govulncheck.txt"
		printf '```\n\n</details>\n\n'
	} >> "$BODY"
}

# audit_counts ARGS...: "critical high moderate low" for npm audit ARGS (empty on error).
audit_counts() {
	rc=0
	(cd web && npm audit --json "$@") > "$TMP/audit.json" 2> "$TMP/audit.err" || rc=$?
	if [ "$rc" -gt 1 ] || ! jq -er '.metadata.vulnerabilities | [.critical, .high, .moderate, .low]
		| if all(type == "number") then map(tostring) | join(" ") else error("npm audit reported no counts") end' \
		"$TMP/audit.json" 2>> "$TMP/audit.err"; then
		cat "$TMP/audit.json" >> "$TMP/audit.err" 2> /dev/null || true
		return 1
	fi
}

vuln_npm() {
	log "npm audit"
	if ! runtime=$(audit_counts --omit=dev) || ! all=$(audit_counts); then
		broken "npm audit" "$TMP/audit.err"
		return 0
	fi
	# shellcheck disable=SC2086 # word splitting intended: four counts
	set -- $runtime
	if [ "$(($1 + $2 + $3))" -gt 0 ]; then
		gate
		verdict="**runtime packages: $1 critical, $2 high, $3 moderate** (CI fails on these)"
	else
		verdict="no moderate or higher advisory in the runtime packages"
	fi
	row "npm audit" "$verdict"
	{
		printf '## npm audit (web/)\n\n'
		printf '| Packages | Critical | High | Moderate | Low |\n|---|---|---|---|---|\n'
		printf '| Runtime (`--omit=dev`, the CI gate) | %s | %s | %s | %s |\n' "$1" "$2" "$3" "$4"
		# shellcheck disable=SC2086 # word splitting intended: four counts
		set -- $all
		printf '| All, build and test tooling included | %s | %s | %s | %s |\n\n' "$1" "$2" "$3" "$4"
	} >> "$BODY"
}

go_modules
toolchains
base_images
engines
npm_outdated
vuln_go
vuln_npm

commit=$(git rev-parse --short HEAD 2> /dev/null || echo unknown)
printf '# Dependency report\n\n'
printf '%s, commit `%s`. Available updates never fail this report; a vulnerability the code reaches (govulncheck) or a moderate or higher advisory in the runtime packages of the web UI (npm audit) does, as in CI.\n\n' \
	"$(date -u +%Y-%m-%dT%H:%MZ)" "$commit"
printf '| Check | Result |\n|---|---|\n'
cat "$SUMMARY"
printf '\n'
cat "$BODY"
exit "$STATUS"
