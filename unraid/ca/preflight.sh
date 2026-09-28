#!/bin/sh
# preflight.sh: read-only, anonymous checks of everything the Community Applications scan looks at,
# run against the LIVE public repository and registry right before you press Validate at
# https://ca.unraid.net/submit/new (unraid/README.md, Publishing to Community Applications).
#
#   sh unraid/ca/preflight.sh [-e unraid/ca/publish.env]      (or: make ca-preflight)
#
# It only sends anonymous GET requests (GitHub API, raw.githubusercontent.com, the registry's
# anonymous pull-token endpoint). It never logs in, pushes, or changes anything. Values come from
# the same env file as the template (unraid/ca/render.sh --get).
#
# Exit 0 = no FAIL. Before the repository and the image are public every network check fails;
# that is expected. Needs curl, plus xmllint for the local template checks.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
env_file=$here/publish.env
while [ $# -gt 0 ]; do
	case $1 in
	-e) [ $# -ge 2 ] || { echo "usage: preflight.sh [-e FILE.env]" >&2; exit 2; }; env_file=$2; shift 2 ;;
	*) echo "usage: preflight.sh [-e FILE.env]" >&2; exit 2 ;;
	esac
done
command -v curl >/dev/null 2>&1 || { echo "preflight.sh: curl is required" >&2; exit 2; }

get() { sh "$here/render.sh" -e "$env_file" --get "$1"; }
npass=0 nwarn=0 nfail=0
ok() { npass=$((npass + 1)); printf 'PASS  %s\n' "$*"; }
warn() { nwarn=$((nwarn + 1)); printf 'WARN  %s\n' "$*"; }
bad() { nfail=$((nfail + 1)); printf 'FAIL  %s\n' "$*"; }
manual() { printf 'TODO  %s\n' "$*"; }
UA='User-Agent: bunkarr-ca-preflight'
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ca-preflight.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

owner=$(get PUBLIC_OWNER) repo=$(get PUBLIC_REPO) trepo=$(get TEMPLATES_REPO)
branch=$(get PUBLIC_BRANCH) host=$(get PUBLIC_HOST)
template_url=$(get TEMPLATE_URL) icon_url=$(get ICON_URL) profile_url=$(get PROFILE_URL)
support_url=$(get SUPPORT_URL) forum_url=$(get FORUM_URL) image=$(get IMAGE)
version=$(get VERSION) registry=$(get REGISTRY) template_path=$(get TEMPLATE_PATH)
printf 'Checking %s/%s (templates: %s/%s), image %s, version %s\n\n' "$owner" "$repo" "$owner" "$trepo" "$image" "$version"

# jfield FILE KEY: first "key": value of a flat JSON field (good enough for the GitHub API).
jfield() { tr -d '\n' <"$1" | sed -nE "s/.*\"$2\": *(\"([^\"]*)\"|(true|false|null|[0-9]+)).*/\\2\\3/p"; }

# ---- 1. GitHub repositories: public, active, default branch, license
[ "$host" = github.com ] || bad "PUBLIC_HOST is $host: the CA form only accepts GitHub repositories"
for r in $(printf '%s\n%s\n' "$repo" "$trepo" | sort -u); do
	code=$(curl -sS -o "$tmp/repo.json" -w '%{http_code}' -H "$UA" -H 'Accept: application/vnd.github+json' \
		"https://api.github.com/repos/$owner/$r" 2>/dev/null) || code=000
	if [ "$code" != 200 ]; then
		bad "github.com/$owner/$r: HTTP $code (not created yet, private, or renamed)"
		continue
	fi
	[ "$(jfield "$tmp/repo.json" full_name)" = "$owner/$r" ] || warn "github.com/$owner/$r redirects to $(jfield "$tmp/repo.json" full_name) (renamed? CA blacklists renamed repositories)"
	for f in private archived disabled; do
		if [ "$(jfield "$tmp/repo.json" $f)" = false ]; then ok "github.com/$owner/$r: $f = false"; else bad "github.com/$owner/$r: $f is not false (must be public and active)"; fi
	done
	db=$(jfield "$tmp/repo.json" default_branch)
	if [ "$db" = "$branch" ]; then ok "default branch is $db"; else bad "default branch is '$db', templates are expected on '$branch'"; fi
	spdx=$(tr -d '\n' <"$tmp/repo.json" | sed -nE 's/.*"license": *\{[^}]*"spdx_id": *"([^"]*)".*/\1/p')
	case $spdx in
	GPL-3.0* | GPL-2.0* | AGPL-3.0* | LGPL-* | MIT | Apache-2.0 | BSD-* | MPL-2.0 | ISC | Unlicense)
		ok "GitHub detects the license as $spdx (OSI-approved)" ;;
	'' | NOASSERTION) bad "GitHub does not recognize the license (spdx_id '${spdx:-none}'); CA's Validate step needs an OSI license" ;;
	*) warn "GitHub detects the license as $spdx: check that it is OSI-approved" ;;
	esac
done

# ---- 2. raw files CA and Unraid download
fetch() { curl -sSL -o "$2" -w '%{http_code} %{content_type}' -H "$UA" "$1" 2>/dev/null || echo 000; }
res=$(fetch "$template_url" "$tmp/template.xml")
case $res in
200*)
	ok "template reachable: $template_url"
	if [ "$trepo" = "$repo" ]; then
		if cmp -s "$tmp/template.xml" "$root/$template_path"; then ok "published template equals local $template_path"; else warn "published template differs from local $template_path (not pushed yet?)"; fi
	fi
	if sh "$here/validate-template.sh" --as "$template_path" "$tmp/template.xml" >"$tmp/validate.txt" 2>&1; then
		ok "published template passes validate-template.sh"
	else
		bad "published template fails validate-template.sh:"
		grep '^FAIL' "$tmp/validate.txt" | sed 's/^/      /' || :
	fi
	;;
*) bad "template not reachable (HTTP ${res%% *}): $template_url" ;;
esac
res=$(fetch "$profile_url" "$tmp/ca_profile.xml")
case $res in
200*)
	ok "ca_profile.xml reachable at the repository root"
	if sh "$here/validate-template.sh" --profile --as ca_profile.xml "$tmp/ca_profile.xml" >"$tmp/validate.txt" 2>&1; then
		ok "published ca_profile.xml passes validate-template.sh"
	else
		bad "published ca_profile.xml fails validate-template.sh:"
		grep '^FAIL' "$tmp/validate.txt" | sed 's/^/      /' || :
	fi
	;;
*) bad "ca_profile.xml not reachable (HTTP ${res%% *}): $profile_url (the submission cannot be finalized without it)" ;;
esac
res=$(fetch "$icon_url" "$tmp/icon")
case $res in
200*)
	sig=$(od -An -tx1 -N8 "$tmp/icon" | tr -d ' \n')
	if [ "$sig" = 89504e470d0a1a0a ]; then ok "icon reachable and a PNG ($(wc -c <"$tmp/icon" | tr -d ' ') bytes): $icon_url"; else
		bad "icon URL does not serve a PNG: $icon_url"; fi
	;;
*) bad "icon not reachable (HTTP ${res%% *}): $icon_url" ;;
esac

# ---- 3. image: anonymous pull, :latest and :VERSION, linux/amd64 (+ arm64)
name=${image%:*}
case $name in */*) ;; *) name=$image ;; esac
case $name in
ghcr.io/*) reg_host=ghcr.io; path=${name#ghcr.io/}
	token_url="https://ghcr.io/token?service=ghcr.io&scope=repository:$path:pull" ;;
*.*/*) reg_host=${name%%/*}; path=${name#*/}; token_url='' ;;
*) reg_host='registry-1.docker.io'; path=$name
	case $path in */*) ;; *) path=library/$path ;; esac
	token_url="https://auth.docker.io/token?service=registry.docker.io&scope=repository:$path:pull" ;;
esac
token=''
if [ -n "$token_url" ]; then
	token=$(curl -sS -H "$UA" "$token_url" 2>/dev/null | tr -d '\n' | sed -nE 's/.*"(token|access_token)": *"([^"]*)".*/\2/p' | head -n 1) || token=''
fi
ACCEPT='application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'
manifest() { # manifest REF OUT -> http code
	if [ -n "$token" ]; then
		curl -sS -o "$2" -w '%{http_code}' -H "$UA" -H "Accept: $ACCEPT" -H "Authorization: Bearer $token" "https://$reg_host/v2/$path/manifests/$1" 2>/dev/null || echo 000
	else
		curl -sS -o "$2" -w '%{http_code}' -H "$UA" -H "Accept: $ACCEPT" "https://$reg_host/v2/$path/manifests/$1" 2>/dev/null || echo 000
	fi
}
for t in latest "$version"; do
	code=$(manifest "$t" "$tmp/manifest.json")
	if [ "$code" != 200 ]; then
		bad "$reg_host/$path:$t is not anonymously pullable (HTTP $code; not pushed yet, or the package is still private)"
		continue
	fi
	ok "$reg_host/$path:$t is anonymously pullable"
	flat=$(tr -d '\n ' <"$tmp/manifest.json")
	case $flat in
	*'"manifests":'*)
		for arch in amd64 arm64; do
			case $flat in *"\"architecture\":\"$arch\""*) ok "$path:$t has linux/$arch" ;;
			*) if [ $arch = amd64 ]; then bad "$path:$t has no linux/amd64 image (Unraid is x86-64)"; else warn "$path:$t has no linux/arm64 image"; fi ;;
			esac
		done
		;;
	*) warn "$path:$t is a single-platform manifest: check it is linux/amd64 (docker buildx imagetools inspect)" ;;
	esac
	[ "$t" = latest ] || continue
	# The linux/amd64 image's OCI source label: ghcr.io links the package to the repository it names.
	mdigest=''
	case $flat in
	*'"manifests":'*)
		mdigest=$(printf '%s' "$flat" | sed 's/},{/}\
{/g' | grep '"architecture":"amd64"' | grep '"os":"linux"' |
			sed -n 's/.*"digest":"\(sha256:[0-9a-f]*\)".*/\1/p' | head -n 1) || mdigest=''
		if [ -n "$mdigest" ] && [ "$(manifest "$mdigest" "$tmp/amd64.json")" = 200 ]; then
			flat=$(tr -d '\n ' <"$tmp/amd64.json")
		else
			flat=''
		fi
		;;
	esac
	cdigest=$(printf '%s' "$flat" | sed -n 's/.*"config":{[^}]*"digest":"\(sha256:[0-9a-f]*\)".*/\1/p')
	src=''
	if [ -n "$cdigest" ]; then
		if [ -n "$token" ]; then
			curl -sSL -o "$tmp/config.json" -H "$UA" -H "Authorization: Bearer $token" "https://$reg_host/v2/$path/blobs/$cdigest" 2>/dev/null || :
		else
			curl -sSL -o "$tmp/config.json" -H "$UA" "https://$reg_host/v2/$path/blobs/$cdigest" 2>/dev/null || :
		fi
		[ ! -f "$tmp/config.json" ] || src=$(tr -d '\n' <"$tmp/config.json" | sed -nE 's/.*"org\.opencontainers\.image\.source": *"([^"]*)".*/\1/p')
	fi
	repo_url=$(get PUBLIC_REPO_URL)
	case $src in
	"$repo_url" | "$repo_url.git") ok "image label org.opencontainers.image.source = $src (links the package to the repository)" ;;
	'') warn "could not read the image's org.opencontainers.image.source label" ;;
	*) warn "image label org.opencontainers.image.source is $src, not $repo_url (ghcr.io links the package to the repository it names; fix the LABEL in the Dockerfile)" ;;
	esac
done

# ---- 4. support
res=$(curl -sSL -o /dev/null -w '%{http_code}' -H "$UA" "$support_url" 2>/dev/null) || res=000
case $res in 2*) ok "support link reachable: $support_url" ;; *) bad "support link not reachable (HTTP $res): $support_url" ;; esac
case $support_url in
https://forums.unraid.net/*) ok "support points at an Unraid forum thread" ;;
*) warn "support points at $support_url; the forum thread '[Support] $owner - Bunkarr' is the convention, not a scan requirement" ;;
esac
if [ -n "$forum_url" ]; then
	res=$(curl -sSL -o /dev/null -w '%{http_code}' -H "$UA" "$forum_url" 2>/dev/null) || res=000
	case $res in 2*) ok "forum thread reachable: $forum_url" ;; *) bad "forum thread not reachable (HTTP $res): $forum_url" ;; esac
fi

# ---- 5. local files (offline)
if sh "$here/validate-template.sh" --icon "$root/unraid/icon.png" --repo "$root" --as "$template_path" "$root/$template_path" >"$tmp/local.txt" 2>&1; then
	ok "local $template_path passes validate-template.sh (make ca-validate)"
else
	bad "local $template_path fails validate-template.sh:"
	grep '^FAIL' "$tmp/local.txt" | sed 's/^/      /' || :
fi

manual "2FA is still on for the GitHub account $owner (CA policy; not checkable anonymously)"
manual "the package $registry/$path is linked to github.com/$owner/$repo and its visibility is Public (Package settings)"
manual "a test install from the public template on a real Unraid server works (login, Plex data path, a destination test and a first sync; restart and reboot keep the host name)"

printf '\npreflight.sh: %d passed, %d warnings, %d failed\n' "$npass" "$nwarn" "$nfail"
[ "$nfail" -eq 0 ]
