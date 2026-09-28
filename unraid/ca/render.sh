#!/bin/sh
# render.sh: render a Community Applications file (Docker template or ca_profile.xml) from its
# .tmpl source, with every public value taken from ONE settings file.
#
#   sh unraid/ca/render.sh -e unraid/ca/publish.env -o unraid/bunkarr.xml unraid/ca/bunkarr.xml.tmpl
#   sh unraid/ca/render.sh -e unraid/ca/publish.env --print        # every value after defaults
#   sh unraid/ca/render.sh -e unraid/ca/publish.env --get IMAGE    # one value
#
# The Makefile drives it (make ca-template, ca-profile, ca-validate, ca-vars); see
# unraid/ca/README.md.
#
# Values come only from the -e files (a later file overrides an earlier one, for example a local
# overlay to try another owner). The environment is ignored on purpose, so a stray exported
# VERSION or IMAGE never leaks into a published template.
# Env files are parsed, never sourced: one KEY=value per line, # starts a comment line, optional
# '...' or "..." quotes around the value, no inline comments. An empty value means "use the
# default" (see derive below).
#
# Placeholders in a .tmpl file:
#   {{KEY}}    the value of KEY, XML-escaped (& < >)
#   {{?KEY}}   the same, but the whole line is dropped when KEY is empty (optional elements)
#   {{# ...    a line that starts with this is a template comment and is dropped
# Nothing is written unless everything resolved: an unknown placeholder, a missing required
# value, a malformed value, a first <Changes> heading other than "### VERSION (RELEASE_DATE)"
# or any "{{" or "}}" left in the result fails the run (exit 1) and leaves the output file
# untouched.
#
# POSIX sh; needs only sed, grep, awk, sort, tr, mktemp, basename, dirname, chmod and mv.
set -eu

prog=render.sh
die() { printf '%s: %s\n' "$prog" "$*" >&2; exit 1; }
usage() {
	cat >&2 <<'EOF'
usage: render.sh -e FILE.env [-e FILE.env ...] -o OUTPUT TEMPLATE.tmpl
       render.sh -e FILE.env [-e FILE.env ...] --print
       render.sh -e FILE.env [-e FILE.env ...] --get KEY
EOF
	exit "${1:-2}"
}

# ---------------------------------------------------------------------------------------------
# Key/value store: shell variables V_<KEY>; keys are validated before they reach eval.
keys=''
valid_key() {
	case $1 in '' | [!A-Z]* | *[!A-Z0-9_]*) return 1 ;; esac
	return 0
}
setv() {
	valid_key "$1" || die "invalid key '$1'"
	eval "V_$1=\$2"
	case " $keys " in *" $1 "*) ;; *) keys="$keys $1" ;; esac
}
getv() {
	valid_key "$1" || die "invalid key '$1'"
	eval "printf '%s' \"\${V_$1-}\""
}
known() {
	case " $keys " in *" $1 "*) return 0 ;; esac
	return 1
}
# def KEY DEFAULT: set KEY when it is unset or empty.
def() { [ -n "$(getv "$1")" ] || setv "$1" "$2"; }
matches() { printf '%s' "$1" | grep -Eq "$2"; }

# ---------------------------------------------------------------------------------------------
env_files='' env_names='' out='' mode=render get_key='' tmpl=''
while [ $# -gt 0 ]; do
	case $1 in
	-e) [ $# -ge 2 ] || usage; env_files="$env_files
$2"; env_names="${env_names:+$env_names + }$(basename "$2")"; shift 2 ;;
	-o) [ $# -ge 2 ] || usage; out=$2; shift 2 ;;
	--print) mode='print'; shift ;;
	--get) [ $# -ge 2 ] || usage; mode='get'; get_key=$2; shift 2 ;;
	-h | --help) usage 0 ;;
	-*) usage ;;
	*) [ -z "$tmpl" ] || usage; tmpl=$1; shift ;;
	esac
done
[ -n "$env_files" ] || usage
if [ "$mode" = render ]; then
	if [ -z "$tmpl" ] || [ -z "$out" ]; then usage; fi
	[ -f "$tmpl" ] || die "template not found: $tmpl"
fi

load_env() {
	[ -f "$1" ] || die "env file not found: $1"
	n=0
	while IFS= read -r line || [ -n "$line" ]; do
		n=$((n + 1))
		# trim surrounding whitespace (and a Windows CR)
		line=$(printf '%s' "$line" | tr -d '\r')
		line=${line#"${line%%[![:space:]]*}"}
		line=${line%"${line##*[![:space:]]}"}
		case $line in '' | '#'*) continue ;; esac
		key=${line%%=*}
		[ "$key" != "$line" ] || die "$1:$n: expected KEY=value"
		valid_key "$key" || die "$1:$n: invalid key '$key' (A-Z, 0-9 and _ only)"
		val=${line#*=}
		case $val in
		\"*\") val=${val#\"}; val=${val%\"} ;;
		\'*\') val=${val#\'}; val=${val%\'} ;;
		*' #'*) die "$1:$n: inline comments are not supported (put the comment on its own line)" ;;
		esac
		setv "$key" "$val"
	done <"$1"
}

while IFS= read -r f; do
	[ -n "$f" ] || continue
	load_env "$f"
done <<EOF
$env_files
EOF

# ---------------------------------------------------------------------------------------------
# Defaults. Everything public is derived from PUBLIC_OWNER / PUBLIC_REPO (+ PUBLIC_HOST, REGISTRY),
# so changing the owner or repository name is a one-line edit in publish.env.
derive() {
	for k in PUBLIC_OWNER PUBLIC_REPO VERSION RELEASE_DATE; do
		[ -n "$(getv "$k")" ] || die "$k is not set (in $env_names)"
	done
	def PUBLIC_HOST github.com
	def PUBLIC_BRANCH main
	def TEMPLATES_REPO "$(getv PUBLIC_REPO)"
	def TEMPLATE_PATH unraid/bunkarr.xml
	def REGISTRY ghcr.io
	def IMAGE_NAME bunkarr
	def IMAGE_TAG latest
	def IMAGE_OWNER "$(getv PUBLIC_OWNER | tr '[:upper:]' '[:lower:]')"

	host=$(getv PUBLIC_HOST) owner=$(getv PUBLIC_OWNER) repo=$(getv PUBLIC_REPO)
	branch=$(getv PUBLIC_BRANCH) trepo=$(getv TEMPLATES_REPO)
	def PUBLIC_REPO_URL "https://$host/$owner/$repo"
	def TEMPLATES_REPO_URL "https://$host/$owner/$trepo"
	if [ "$host" = github.com ]; then
		def PUBLIC_RAW_BASE "https://raw.githubusercontent.com/$owner/$repo/$branch"
		def TEMPLATES_RAW_BASE "https://raw.githubusercontent.com/$owner/$trepo/$branch"
		def README_URL "$(getv PUBLIC_REPO_URL)/blob/$branch/unraid/README.md"
	else
		[ -n "$(getv PUBLIC_RAW_BASE)" ] || die "PUBLIC_HOST=$host is not github.com: set PUBLIC_RAW_BASE"
		[ -n "$(getv README_URL)" ] || die "PUBLIC_HOST=$host is not github.com: set README_URL"
		[ "$trepo" = "$repo" ] || [ -n "$(getv TEMPLATES_RAW_BASE)" ] ||
			die "PUBLIC_HOST=$host is not github.com: set TEMPLATES_RAW_BASE"
		def TEMPLATES_RAW_BASE "$(getv PUBLIC_RAW_BASE)"
	fi
	def TEMPLATE_URL "$(getv TEMPLATES_RAW_BASE)/$(getv TEMPLATE_PATH)"
	def ICON_URL "$(getv PUBLIC_RAW_BASE)/unraid/icon.png"
	def PROFILE_URL "$(getv TEMPLATES_RAW_BASE)/ca_profile.xml"

	reg=$(getv REGISTRY) iowner=$(getv IMAGE_OWNER) iname=$(getv IMAGE_NAME) tag=$(getv IMAGE_TAG)
	case $reg in
	docker.io | index.docker.io | registry-1.docker.io)
		def IMAGE "$iowner/$iname:$tag"
		def REGISTRY_URL "https://hub.docker.com/r/$iowner/$iname"
		;;
	ghcr.io)
		def IMAGE "ghcr.io/$iowner/$iname:$tag"
		[ "$host" != github.com ] || def REGISTRY_URL "https://github.com/$owner/$repo/pkgs/container/$iname"
		;;
	*) def IMAGE "$reg/$iowner/$iname:$tag" ;;
	esac
	[ -n "$(getv REGISTRY_URL)" ] || die "set REGISTRY_URL (the web page of the image) for REGISTRY=$reg"
	def SUPPORT_URL "$(getv PUBLIC_REPO_URL)/issues"
	# Optional, empty unless set: the forum support thread (ca_profile <Forum>).
	def FORUM_URL ''
	setv ENV_NAME "$env_names"
}

check_values() {
	for k in $keys; do
		v=$(getv "$k")
		case $v in *'{{'* | *'}}'*) die "$k must not contain {{ or }}" ;; esac
		if matches "$v" 'CHANGE_?ME|YOUR_[A-Z_]+'; then die "$k still holds a placeholder: $v"; fi
	done
	matches "$(getv PUBLIC_OWNER)" '^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$' ||
		die "PUBLIC_OWNER is not a valid user or organization name: $(getv PUBLIC_OWNER)"
	for k in PUBLIC_REPO TEMPLATES_REPO; do
		matches "$(getv "$k")" '^[A-Za-z0-9._-]+$' || die "$k is not a valid repository name: $(getv "$k")"
	done
	matches "$(getv VERSION)" '^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$' ||
		die "VERSION must look like 1.2.3: $(getv VERSION)"
	matches "$(getv RELEASE_DATE)" '^[0-9]{4}-[0-9]{2}-[0-9]{2}$' ||
		die "RELEASE_DATE must be YYYY-MM-DD: $(getv RELEASE_DATE)"
	img=$(getv IMAGE)
	name=${img%:*}
	case $name in */*) ;; *) name=$img ;; esac # a ":" before the last "/" is a registry port
	matches "$name" '^[a-z0-9][a-z0-9._:/-]*$' ||
		die "IMAGE must be lower-case and contain no spaces (docker/ghcr.io rule): $img"
	for k in PUBLIC_REPO_URL TEMPLATES_REPO_URL PUBLIC_RAW_BASE TEMPLATES_RAW_BASE TEMPLATE_URL \
		ICON_URL PROFILE_URL README_URL REGISTRY_URL SUPPORT_URL FORUM_URL; do
		v=$(getv "$k")
		[ -n "$v" ] || continue
		matches "$v" '^https?://[^[:space:]"<>]+$' || die "$k is not a URL: $v"
	done
}

derive
check_values

case $mode in
print)
	for k in $keys; do printf '%s=%s\n' "$k" "$(getv "$k")"; done
	exit 0
	;;
get)
	known "$get_key" || die "unknown key: $get_key"
	getv "$get_key"
	printf '\n'
	exit 0
	;;
esac

# ---------------------------------------------------------------------------------------------
# Render.
xml_escape() { printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g'; }
sed_escape() { printf '%s' "$1" | sed -e 's/[\\|&]/\\&/g'; }

used=$(grep -v '^[[:space:]]*{{#' "$tmpl" | grep -o '{{?\{0,1\}[A-Za-z0-9_]*}}' |
	sed -e 's/^{{?\{0,1\}//' -e 's/}}$//' | sort -u) || used=''
for k in $used; do
	known "$k" || die "$tmpl: unknown placeholder {{$k}} (known:$keys)"
done

# A template with <Changes> (the app's change log in CA) names the release of <Date> in its first
# heading: "### VERSION (RELEASE_DATE)". Both are written by hand, the heading here and the values
# in the env file, and CA reads the template straight from the public branch, so a release that
# changes one without the other fails here, in every make ca-* target and the export's check.
if grep -v '^[[:space:]]*{{#' "$tmpl" | grep -q '<Changes>'; then
	want="### $(getv VERSION) ($(getv RELEASE_DATE))"
	first=$(grep -v '^[[:space:]]*{{#' "$tmpl" | awk '
		/<Changes>/ { on = 1; sub(/.*<Changes>/, "") }
		on {
			last = sub(/<\/Changes>.*/, "")
			sub(/&#([xX][dD]|13);[[:space:]]*$/, "")
			sub(/[[:space:]]+$/, "")
			if ($0 ~ /^### /) { print; exit }
			if (last) exit
		}')
	[ "$first" = "$want" ] ||
		die "$tmpl: the first <Changes> heading is '${first:-none}', want '$want' (VERSION and RELEASE_DATE in $env_names): add the release's entry at the top of <Changes>, or fix the env file"
fi

outdir=$(dirname "$out")
[ -d "$outdir" ] || die "output directory does not exist: $outdir"
sedscript=$(mktemp "${TMPDIR:-/tmp}/render.sed.XXXXXX")
tmpout=$(mktemp "$outdir/.render.XXXXXX")
trap 'rm -f "$sedscript" "$tmpout"' EXIT HUP INT TERM

{
	printf '%s\n' '/^[[:space:]]*{{#/d'
	for k in $keys; do
		[ -n "$(getv "$k")" ] || printf '/{{?%s}}/d\n' "$k"
	done
	for k in $keys; do
		e=$(sed_escape "$(xml_escape "$(getv "$k")")")
		printf 's|{{%s}}|%s|g\n' "$k" "$e"
		printf 's|{{?%s}}|%s|g\n' "$k" "$e"
	done
} >"$sedscript"

sed -f "$sedscript" "$tmpl" >"$tmpout"
[ -s "$tmpout" ] || die "$tmpl rendered to an empty file"
if left=$(grep -nE '\{\{|\}\}' "$tmpout"); then
	printf '%s\n' "$left" >&2
	die "placeholders left after rendering $tmpl (lines above); nothing written"
fi
chmod 644 "$tmpout"
mv -f "$tmpout" "$out"
trap - EXIT HUP INT TERM
rm -f "$sedscript"
printf '%s: wrote %s from %s (%s)\n' "$prog" "$out" "$tmpl" "$env_names"
