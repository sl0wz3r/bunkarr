#!/bin/sh
# validate-template.sh: check the Bunkarr Unraid template (or ca_profile.xml) against the
# Community Applications template rules (summarized in unraid/ca/README.md) and Bunkarr's own.
#
#   sh unraid/ca/validate-template.sh [--icon unraid/icon.png] [--repo . [--scratch unraid/ca/out]] [--as unraid/bunkarr.xml] unraid/bunkarr.xml
#   sh unraid/ca/validate-template.sh --profile [--as ca_profile.xml] ca_profile.xml
#
#   --icon F     also check the local icon file the <Icon> URL serves: PNG, transparency, not
#                animated, square, big enough, image chunks only and nothing after IEND (an
#                allow-list): the public export's term scan skips binary files, so nothing may
#                hide in them
#   --repo D     also check repository D the way CA clones it: one template per Name, no
#                deprecated/ folder, a LICENSE, ca_profile.xml at the root. When D is the top
#                level of a git work tree the files are git ls-files; otherwise (an exported
#                tree, even one unpacked inside another repository) every file under D
#   --scratch S  the render scratch folder (make's CA_OUT): left out of that file list
#   --as P       the repository path the file is published at (default: the file's own path);
#                used when a freshly rendered file is checked before it is moved into place
#
# Every file checked here is public: https URLs only, no private addresses or LAN host names.
#
# Prints PASS / WARN / FAIL / INFO per rule, then a summary. Exit 0 = no FAIL, 1 = at least one
# FAIL, 2 = usage error or xmllint missing. Needs xmllint (libxml2: macOS ships it; Debian and
# Ubuntu: apt install libxml2-utils) plus sed, grep, awk, od and tr (and git or find for --repo).
set -eu

prog=validate-template.sh
usage() {
	sed -n '5,6p' "$0" | sed 's/^# *//' >&2
	exit 2
}

mode=template icon='' repo='' scratch='' as='' file=''
while [ $# -gt 0 ]; do
	case $1 in
	--profile) mode=profile; shift ;;
	--icon) [ $# -ge 2 ] || usage; icon=$2; shift 2 ;;
	--repo) [ $# -ge 2 ] || usage; repo=$2; shift 2 ;;
	--scratch) [ $# -ge 2 ] || usage; scratch=$2; shift 2 ;;
	--as) [ $# -ge 2 ] || usage; as=$2; shift 2 ;;
	-h | --help) usage ;;
	-*) usage ;;
	*) [ -z "$file" ] || usage; file=$1; shift ;;
	esac
done
[ -n "$file" ] || usage
[ -f "$file" ] || { printf '%s: %s not found\n' "$prog" "$file" >&2; exit 2; }
command -v xmllint >/dev/null 2>&1 || {
	printf '%s: xmllint is required (macOS: built in; Debian/Ubuntu: apt install libxml2-utils)\n' "$prog" >&2
	exit 2
}
[ -n "$as" ] || as=$file
as=${as#./}
# A path inside --repo counts as repository-relative.
if [ -n "$repo" ] && [ -d "$repo" ]; then
	repo_abs=$(cd "$repo" && pwd)
	case $as in /*) as_abs=$as ;; *) as_abs=$(pwd)/$as ;; esac
	case $as_abs in "$repo_abs"/*) as=${as_abs#"$repo_abs"/} ;; esac
fi

npass=0 nwarn=0 nfail=0
ok() { npass=$((npass + 1)); printf 'PASS  %s\n' "$*"; }
warn() { nwarn=$((nwarn + 1)); printf 'WARN  %s\n' "$*"; }
bad() { nfail=$((nfail + 1)); printf 'FAIL  %s\n' "$*"; }
info() { printf 'INFO  %s\n' "$*"; }
# x XPATH [FILE]: string/number result of an XPath expression ('' when nothing matches).
x() { xmllint --xpath "$1" "${2:-$file}" 2>/dev/null || :; }
# has TEXT ERE / hasi: does any line of TEXT match (case-insensitive for hasi)?
has() { printf '%s\n' "$1" | grep -Eq -- "$2"; }
hasi() { printf '%s\n' "$1" | grep -Eiq -- "$2"; }
count() { x "count($1)"; }
summary() {
	printf '\n%s: %s: %d passed, %d warnings, %d failed\n' "$prog" "$as" "$npass" "$nwarn" "$nfail"
	[ "$nfail" -eq 0 ]
}

# Private and loopback IPv4 ranges, plus LAN-only host names.
PRIVATE_RE='(^|[^0-9.])(10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}|127\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|169\.254\.[0-9]{1,3}\.[0-9]{1,3})([^0-9]|$)|localhost|\.(local|lan|home\.arpa|internal)([/:"<]|$)'
# Placeholders left by render.sh or copied from the CA starter repository.
PLACEHOLDER_RE='\{\{|\}\}|YOUR_[A-Z_]+|CHANGE_?ME|container_name|example-app|YOUR_SUPPORT_TOPIC'
REFERRAL_RE='[?&](ref|aff|affiliate|affid|tag|utm_[a-z]+)='
URL_ELEMENTS='Support Project ReadMe TemplateURL Icon Registry'

# ------------------------------------------------------------------------------ common checks
if ! err=$(xmllint --noout "$file" 2>&1); then
	bad "well-formed XML: $(printf '%s' "$err" | head -n 3 | tr '\n' ' ')"
	summary || exit 1
fi
ok "well-formed XML (xmllint)"

# A bare & breaks the template in CA; say where, even though xmllint would already have failed.
if lines=$(sed -E 's/&(amp|lt|gt|quot|apos|#[0-9]+|#x[0-9A-Fa-f]+);//g' "$file" | grep -n '&'); then
	bad "bare & (write &amp;) on line(s): $(printf '%s' "$lines" | cut -d: -f1 | tr '\n' ' ')"
else
	ok "no bare & characters"
fi

if lines=$(grep -nE -- "$PLACEHOLDER_RE" "$file"); then
	bad "placeholder text left (render.sh or CA starter values): line(s) $(printf '%s' "$lines" | cut -d: -f1 | tr '\n' ' ')"
else
	ok "no placeholders or starter values"
fi

nonascii=$(LC_ALL=C tr -d '\011\012\015\040-\176' <"$file" | wc -c | tr -d ' ')
if [ "$nonascii" -gt 0 ]; then
	warn "$nonascii non-ASCII byte(s): keep templates plain ASCII to avoid encoding surprises"
else
	ok "plain ASCII"
fi

if lines=$(grep -nEi -- "$PRIVATE_RE" "$file"); then
	bad "private address or LAN-only host name in a public file: $(printf '%s' "$lines" | head -n 3 | cut -c1-160 | tr '\n' ' ')"
else
	ok "no private IP addresses or LAN host names"
fi

# ------------------------------------------------------------------------------ ca_profile.xml
if [ "$mode" = profile ]; then
	base=${as##*/}
	if [ "$base" = ca_profile.xml ]; then ok "file name is exactly ca_profile.xml"; else bad "file name must be exactly ca_profile.xml (is $base)"; fi
	case $as in
	*/*) bad "ca_profile.xml must be at the repository root (is $as)" ;;
	*) ok "ca_profile.xml is at the repository root ($as)" ;;
	esac
	root=$(x 'name(/*)')
	if [ "$root" = CommunityApplications ]; then ok "root element CommunityApplications"; else bad "root element must be CommunityApplications (is '$root')"; fi
	profile=$(x 'string(/CommunityApplications/Profile)')
	trimmed=$(printf '%s' "$profile" | tr -d ' \t\r\n')
	if [ -z "$trimmed" ]; then
		bad "<Profile> is empty: CA blocks the submission"
	elif [ "${#profile}" -lt 80 ]; then
		warn "<Profile> is very short (${#profile} characters): say what the repository holds and where to get support"
	else
		ok "<Profile> present (${#profile} characters)"
	fi
	if has "$profile" '<[A-Za-z/!?]'; then bad "<Profile> contains HTML; use Markdown"; else ok "<Profile> has no HTML"; fi
	if has "$profile" '^    '; then warn "<Profile> has a line indented by 4+ spaces (Markdown code block)"; fi
	for el in Icon WebPage Forum Discord Reddit Twitter Facebook DonateLink; do
		v=$(x "string(/CommunityApplications/$el)")
		n=$(count "/CommunityApplications/$el")
		if [ "$n" -eq 0 ]; then
			case $el in Icon | WebPage) warn "<$el> missing (recommended)" ;; Forum) info "<Forum> not set (add it once the support thread exists)" ;; esac
			continue
		fi
		if [ -z "$v" ]; then bad "<$el> is present but empty (remove it or fill it in)"; continue; fi
		if ! has "$v" '^https://'; then bad "<$el> must be an https URL: $v"; continue; fi
		if hasi "$v" "$REFERRAL_RE"; then bad "<$el> looks like a referral/affiliate link: $v"; continue; fi
		if [ "$el" = Icon ] && hasi "$v" 'unraid-community-apps-starter'; then
			bad "<Icon> is the CA starter icon (the scan rejects it): $v"
			continue
		fi
		ok "<$el> $v"
	done
	summary || exit 1
	exit 0
fi

# ------------------------------------------------------------------------------ Docker template
root=$(x 'name(/*)')
ver=$(x 'string(/Container/@version)')
if [ "$root" = Container ] && [ "$ver" = 2 ]; then ok "root element <Container version=\"2\">"; else bad "root element must be <Container version=\"2\"> (is <$root version=\"$ver\">)"; fi

# One element each, where CA reads a single value.
for el in Name Repository Registry Network Shell Privileged Support Project ReadMe Overview Category WebUI TemplateURL Icon ExtraParams PostArgs Requires Beta MinVer MaxVer Date Changes License; do
	n=$(count "/Container/$el")
	[ "$n" -le 1 ] || bad "<$el> appears $n times (one only)"
done

name=$(x 'string(/Container/Name)')
if has "$name" '^[a-zA-Z0-9][a-zA-Z0-9_.-]+$'; then ok "<Name> '$name' is a valid container name"; else bad "<Name> '$name' is not a valid container name ([a-zA-Z0-9][a-zA-Z0-9_.-]+)"; fi

# ---- Repository (image)
image=$(x 'string(/Container/Repository)')
if [ -z "$image" ]; then
	bad "<Repository> is empty (minimum field)"
else
	imgname=${image%%@*}
	digest=''
	[ "$imgname" = "$image" ] || digest=${image#*@}
	path=${imgname%:*}
	tag=''
	case $path in */*) tag=${imgname##*:} ;; *) path=$imgname ;; esac
	[ "$path" != "$imgname" ] || tag=''
	case $path in */*) first=${path%%/*} ;; *) first='' ;; esac
	host=''
	case $first in *.* | *:* | localhost) host=$first ;; esac
	if has "$image" '[[:space:]]'; then bad "<Repository> contains whitespace: '$image'"; fi
	if has "$path" '[A-Z]'; then bad "<Repository> image name must be lower-case: $path"; else ok "<Repository> $image"; fi
	case $host in
	'' | docker.io | index.docker.io | registry-1.docker.io | ghcr.io | lscr.io | quay.io | registry.gitlab.com | codeberg.org)
		ok "public registry (${host:-docker.io})" ;;
	*:* | localhost | [0-9]*.[0-9]*.[0-9]*.[0-9]*)
		bad "registry '$host' is an address or has a port: CA needs a publicly pullable image (ghcr.io or Docker Hub)" ;;
	*) warn "registry '$host' is not one CA commonly uses; make sure anyone can pull from it over HTTPS" ;;
	esac
	if [ -n "$digest" ]; then
		bad "<Repository> pins a digest: use :latest so Unraid's update check works (offer pinned tags with <Branch>)"
	elif [ -n "$tag" ] && [ "$tag" != latest ]; then
		bad "<Repository> uses tag ':$tag': use :latest so Unraid's update check follows releases"
	else
		ok "<Repository> follows :latest"
	fi
fi

# ---- URL elements
for el in $URL_ELEMENTS; do
	v=$(x "string(/Container/$el)")
	if [ -z "$v" ]; then
		case $el in
		Support | Project) : ;; # checked together below
		Registry | ReadMe) warn "<$el> missing or empty (recommended)" ;;
		*) bad "<$el> missing or empty (recommended by CA; required by this project)" ;;
		esac
		continue
	fi
	if ! has "$v" '^https?://[^[:space:]]+$'; then bad "<$el> is not a URL: $v"; continue; fi
	if ! has "$v" '^https://'; then bad "<$el> must use https://: $v"; continue; fi
	if hasi "$v" "$REFERRAL_RE"; then bad "<$el> looks like a referral/affiliate link (disallowed): $v"; continue; fi
	ok "<$el> $v"
done
support=$(x 'string(/Container/Support)')
project=$(x 'string(/Container/Project)')
if [ -z "$support" ] && [ -z "$project" ]; then bad "neither <Support> nor <Project>: CA drops such templates automatically"; else ok "<Support> or <Project> present"; fi

# ---- TemplateURL must be the raw URL of this very file
turl=$(x 'string(/Container/TemplateURL)')
if [ -n "$turl" ]; then
	base=${as##*/}
	if has "$turl" '^https://raw\.githubusercontent\.com/[^/]+/[^/]+/[^/]+/'; then
		ok "<TemplateURL> is a raw.githubusercontent.com URL"
		branch=$(printf '%s' "$turl" | sed -E 's#^https://raw\.githubusercontent\.com/[^/]+/[^/]+/([^/]+)/.*#\1#')
		case $branch in main | master) ok "<TemplateURL> reads the $branch branch" ;; *) warn "<TemplateURL> reads branch '$branch': CA reads the default branch (main)" ;; esac
	else
		bad "<TemplateURL> must be the raw.githubusercontent.com URL of this file: $turl"
	fi
	case $turl in
	*/"$as") ok "<TemplateURL> ends with this file's repository path ($as)" ;;
	*/"$base") warn "<TemplateURL> ends with $base but not with $as (fine only for a separate templates repository)" ;;
	*) bad "<TemplateURL> does not point at this file ($as): $turl" ;;
	esac
fi

# ---- the same owner everywhere (typo guard)
gh_owner() { printf '%s' "$1" | sed -nE 's#^https://(github\.com|raw\.githubusercontent\.com)/([^/]+)/.*#\2#p' | tr '[:upper:]' '[:lower:]'; }
powner=$(gh_owner "$project")
if [ -n "$powner" ]; then
	for el in Support ReadMe TemplateURL Icon Registry; do
		o=$(gh_owner "$(x "string(/Container/$el)")")
		[ -z "$o" ] || [ "$o" = "$powner" ] || warn "<$el> owner '$o' differs from <Project> owner '$powner'"
	done
	case $image in
	ghcr.io/*) iowner=$(printf '%s' "${image#ghcr.io/}" | cut -d/ -f1)
		if [ "$iowner" = "$powner" ]; then ok "image owner matches <Project> owner ($powner)"; else
			warn "image owner '$iowner' differs from <Project> owner '$powner'"; fi ;;
	esac
fi

# ---- Icon
iurl=$(x 'string(/Container/Icon)')
case $iurl in
'') : ;;
*.png | *.png\?*) ok "<Icon> is a PNG" ;;
*.svg | *.svg\?*) warn "<Icon> is an SVG: a transparent PNG is what CA recommends" ;;
*) bad "<Icon> should be a transparent PNG: $iurl" ;;
esac
if [ -n "$icon" ]; then
	if [ ! -f "$icon" ]; then
		bad "icon file not found: $icon"
	else
		sig=$(od -An -tx1 -N8 "$icon" | tr -d ' \n')
		if [ "$sig" != 89504e470d0a1a0a ]; then
			bad "$icon is not a PNG file"
		else
			# walk the PNG chunks: length(4) type(4) data crc(4), up to IEND
			bytes=$(wc -c <"$icon" | tr -d ' ')
			off=8 types='' w=0 h=0 ctype=0 iend=false badtype=''
			while [ $((off + 12)) -le "$bytes" ]; do
				# shellcheck disable=SC2046 # split od's byte list into $1..$8 on purpose
				set -- $(od -An -tu1 -j"$off" -N8 "$icon")
				[ $# -eq 8 ] || break
				# a chunk type is four ASCII letters; anything else is not a chunk boundary
				for b in "$5" "$6" "$7" "$8"; do
					if [ "$b" -lt 65 ] || [ "$b" -gt 122 ] || { [ "$b" -gt 90 ] && [ "$b" -lt 97 ]; }; then badtype=$off; fi
				done
				[ -z "$badtype" ] || break
				len=$((($1 << 24) | ($2 << 16) | ($3 << 8) | $4))
				# shellcheck disable=SC2059 # octal escapes built from the byte values
				t=$(printf "\\$(printf %03o "$5")\\$(printf %03o "$6")\\$(printf %03o "$7")\\$(printf %03o "$8")")
				types="$types $t"
				if [ "$t" = IHDR ]; then
					# shellcheck disable=SC2046
					set -- $(od -An -tu1 -j$((off + 8)) -N10 "$icon")
					w=$((($1 << 24) | ($2 << 16) | ($3 << 8) | $4))
					h=$((($5 << 24) | ($6 << 16) | ($7 << 8) | $8))
					ctype=${10}
				fi
				off=$((off + 12 + len))
				if [ "$t" = IEND ]; then iend=true; break; fi
			done
			case " $types " in *" acTL "*) bad "$icon is an animated PNG (not allowed for app icons)" ;; *) ok "$icon is not animated" ;; esac
			# The public export's private-term scan skips binary files (grep -I), so the icon may
			# hold image data only: text, EXIF, time-stamp, ICC-profile and private chunks can carry
			# dates, comments, user names or paths, and so can bytes after IEND. An allow-list of
			# the chunks that only describe pixels, not a list of known metadata chunks.
			meta=''
			for c in $types; do
				case $c in
				IHDR | PLTE | IDAT | IEND | tRNS | bKGD | pHYs | sRGB | gAMA | cHRM | sBIT) ;;
				*) case "$meta " in *" $c "*) ;; *) meta="$meta $c" ;; esac ;;
				esac
			done
			hidden=true
			if [ -n "$badtype" ]; then
				bad "$icon: no valid chunk type at byte $badtype (corrupt, or a chunk length is wrong)"
			elif [ "$iend" = false ]; then
				bad "$icon has no IEND chunk (truncated, or a chunk length points past the end of the file)"
			elif [ "$off" -ne "$bytes" ]; then
				bad "$icon has $((bytes - off)) byte(s) after IEND (published unscanned): render it again with make ca-icon"
			else
				hidden=false
			fi
			if [ -n "$meta" ]; then
				bad "$icon has chunk(s) outside the image allow-list:$meta (published unscanned): render it again with make ca-icon"
			elif [ "$hidden" = false ]; then
				ok "$icon holds image chunks only (no text, EXIF, time stamp, ICC profile or private chunks) and nothing after IEND"
			fi
			case $ctype in
			4 | 6) ok "$icon has an alpha channel (transparent background possible)" ;;
			*) case " $types " in *" tRNS "*) ok "$icon has transparency (tRNS)" ;; *) bad "$icon has no transparency: use an RGBA PNG" ;; esac ;;
			esac
			if [ "$w" -ne "$h" ]; then warn "$icon is not square (${w}x${h})"; fi
			if [ "$w" -lt 64 ]; then bad "$icon is too small (${w}x${h})"; elif [ "$w" -lt 256 ]; then warn "$icon is small (${w}x${h}); CA asks for a high-resolution icon"; else ok "$icon is ${w}x${h}, $bytes bytes"; fi
			[ "$bytes" -le 1048576 ] || warn "$icon is larger than 1 MiB ($bytes bytes)"
			case $iurl in */"${icon##*/}" | */"${icon##*/}"\?*) ok "<Icon> URL serves ${icon##*/}" ;; '') : ;; *) warn "<Icon> URL does not end with ${icon##*/}: $iurl" ;; esac
		fi
	fi
fi

# ---- Overview
ov=$(x 'string(/Container/Overview)')
if [ -z "$(printf '%s' "$ov" | tr -d ' \t\r\n')" ]; then
	bad "<Overview> is empty: every app needs a reasonable description"
else
	if [ "${#ov}" -lt 150 ]; then warn "<Overview> is short (${#ov} characters)"; else ok "<Overview> present (${#ov} characters)"; fi
	if has "$ov" '<[A-Za-z/!?]'; then bad "<Overview> contains HTML (Unraid strips it; use plain text or Markdown)"; else ok "<Overview> has no HTML"; fi
	if has "$ov" '[][]'; then bad "<Overview> contains [ or ]: CA turns them into < > and strips them as tags (Markdown links do not work either)"; else ok "<Overview> has no square brackets"; fi
	if has "$ov" '^    '; then warn "<Overview> has a line indented by 4+ spaces (shown as non-breaking spaces)"; fi
	stars=$(printf '%s' "$ov" | tr -cd '*' | wc -c | tr -d ' ')
	[ $((stars % 2)) -eq 0 ] || warn "<Overview> has an odd number of * characters (unbalanced Markdown emphasis; write arr, not *arr)"
	# Bunkarr policy: a backup tool states its data-safety boundaries up front (CA's policy allows
	# blacklisting an app over a bug that loses data): the sources are mounted read-only and never
	# changed, bunkarr.key has to be backed up with the database, a Beta says so, and until the
	# Phase 5 restore wizard exists the Overview says that restores are manual (it must not
	# promise an automatic restore; drop the 'restore wizard' rule together with that sentence).
	missing=''
	hasi "$ov" 'never modifies or deletes' || missing="$missing 'never modifies or deletes'"
	hasi "$ov" 'read-only' || missing="$missing 'read-only'"
	hasi "$ov" 'bunkarr[.]key' || missing="$missing 'bunkarr.key'"
	hasi "$ov" 'restore wizard' || missing="$missing 'restore wizard'"
	[ "$(x 'string(/Container/Beta)')" != true ] || hasi "$ov" 'beta' || missing="$missing 'Beta'"
	if [ -n "$missing" ]; then
		bad "<Overview> must state the read-only, never-changed sources, the bunkarr.key backup, the Beta status and the missing restore wizard (missing:$missing)"
	else
		ok "<Overview> states the read-only sources, the bunkarr.key backup, the Beta status and the manual restores"
	fi
fi

# ---- Category
cat=$(x 'string(/Container/Category)')
if [ -z "$cat" ]; then
	bad "<Category> missing"
else
	badtok=''
	for tok in $cat; do
		case $tok in
		AI: | Backup: | Cloud: | Crypto: | Downloaders: | Drivers: | GameServers: | HomeAutomation: | MediaApp: | MediaServer: | Network: | Plugins: | Productivity: | Security: | Tools: | Other:) ;;
		MediaApp:Books | MediaApp:Music | MediaApp:Photos | MediaApp:Video | MediaApp:Other) ;;
		MediaServer:Books | MediaServer:Music | MediaServer:Photos | MediaServer:Video | MediaServer:Other) ;;
		Network:DNS | Network:FTP | Network:Management | Network:Messenger | Network:Proxy | Network:Voip | Network:VPN | Network:Privacy | Network:Web | Network:Other) ;;
		Tools:System | Tools:Utilities) ;;
		*) badtok="$badtok $tok" ;;
		esac
	done
	if [ -n "$badtok" ]; then bad "<Category> has unknown token(s):$badtok"; else ok "<Category> '$cat' uses valid tokens"; fi
fi

# ---- WebUI and ports
webui=$(x 'string(/Container/WebUI)')
if [ -z "$webui" ]; then
	warn "<WebUI> missing (Bunkarr has a web UI)"
elif has "$webui" '^https?://\[IP\]:\[PORT:[0-9]+\]'; then
	wport=$(printf '%s' "$webui" | sed -E 's/.*\[PORT:([0-9]+)\].*/\1/')
	if [ "$(count "/Container/Config[@Type='Port' and @Target='$wport']")" -ge 1 ]; then
		ok "<WebUI> $webui uses container port $wport, which is a Port entry"
	else
		bad "<WebUI> uses [PORT:$wport] but no Config Type=\"Port\" has Target=\"$wport\" (CA removes or rewrites such templates)"
	fi
else
	bad "<WebUI> must be http://[IP]:[PORT:container-port]/... with no hard-coded address or port: $webui"
fi

# ---- flags
priv=$(x 'string(/Container/Privileged)')
if [ "$priv" = false ]; then ok "<Privileged> false"; else bad "<Privileged> must be false (is '$priv')"; fi
if [ "$(count /Container/MaxVer)" -gt 0 ]; then bad "<MaxVer> is set: it hides the app on newer Unraid releases"; else ok "no <MaxVer>"; fi
minver=$(x 'string(/Container/MinVer)')
if [ -z "$minver" ] || has "$minver" '^[0-9]+(\.[0-9]+)*$'; then ok "<MinVer> '${minver:-none}'"; else warn "<MinVer> '$minver' is not a version number"; fi
net=$(x 'string(/Container/Network)')
case $net in bridge | host) ok "<Network> $net" ;; '') warn "<Network> missing (bridge)" ;; *) warn "<Network> '$net': bridge is the portable default" ;; esac
sh_=$(x 'string(/Container/Shell)')
case $sh_ in sh | bash | '') ok "<Shell> '${sh_:-default}'" ;; *) warn "<Shell> '$sh_' (sh or bash)" ;; esac
for el in Deprecated DeprecatedMaxVer; do
	[ "$(count "/Container/$el")" -eq 0 ] || bad "<$el> is set"
done

# ---- ExtraParams / PostArgs: flags only, never commands (instant blacklist otherwise)
ep=$(x 'string(/Container/ExtraParams)')
if [ -n "$ep" ]; then
	epok=true
	if has "$ep" '[;|&`<>\\]|\$\('; then bad "<ExtraParams> contains shell metacharacters: $ep"; epok=false; fi
	set -f
	for w in $ep; do
		case $w in -*) ;; *) bad "<ExtraParams> word '$w' is not a docker flag (no commands allowed)"; epok=false ;; esac
	done
	set +f
	if has "$ep" '(^|[[:space:]])--privileged'; then bad "<ExtraParams> uses --privileged"; epok=false; fi
	if has "$ep" '(^|[[:space:]])(--env(-file)?([=[:space:]]|$)|-e[A-Za-z_])'; then bad "<ExtraParams> passes environment variables: use a Variable Config entry (CA flags them)"; epok=false; fi
	if has "$ep" '(^|[[:space:]])--restart'; then warn "<ExtraParams> sets --restart (Unraid manages restarts; CA rewrites it)"; fi
	if has "$ep" '--pids-limit='; then warn "<ExtraParams> --pids-limit=N is not recognized by Unraid (use the space form)"; fi
	[ "$epok" = false ] || ok "<ExtraParams> are docker flags only: $ep"
fi
pa=$(x 'string(/Container/PostArgs)')
if [ -n "$pa" ] && has "$pa" '[;|&`<>\\]|\$\('; then
	bad "<PostArgs> contains shell metacharacters (CA blacklists the whole repository for commands after docker run): $pa"
elif [ -n "$pa" ]; then
	warn "<PostArgs> is set ('$pa'); CA's field reference does not list it"
fi

# ---- Requires
req=$(x 'string(/Container/Requires)')
if [ -n "$req" ]; then
	if has "$req" '<[A-Za-z/!?]'; then bad "<Requires> contains HTML"; fi
	if hasi "$req$ov" 'insecure-registr|plain[- ]http'; then
		bad "<Requires>/<Overview> mention a plain-HTTP registry or --insecure-registry (the image must be pullable anonymously over HTTPS)"
	else
		ok "<Requires> present, no plain-HTTP registry instructions"
	fi
fi

# ---- Date, Changes, Beta
date=$(x 'string(/Container/Date)')
if [ -z "$date" ]; then warn "<Date> missing"; elif has "$date" '^[0-9]{4}-[0-9]{2}-[0-9]{2}( [0-9]{2}:[0-9]{2}:[0-9]{2})?$'; then ok "<Date> $date"; else bad "<Date> must be YYYY-MM-DD: $date"; fi
changes=$(x 'string(/Container/Changes)')
cver=''
if [ -z "$changes" ]; then
	warn "<Changes> missing (feeds the Updated Apps list)"
else
	if has "$changes" '[][]'; then bad "<Changes> contains [ or ]: CA turns them into tags and strips them"; else ok "<Changes> has no square brackets"; fi
	if has "$changes" '<[A-Za-z/!?]'; then bad "<Changes> contains HTML"; fi
	if hasi "$changes" 'unreleased'; then warn "<Changes> still says 'unreleased'"; fi
	cver=$(printf '%s\n' "$changes" | sed -nE 's/^#+ *v?([0-9]+\.[0-9]+\.[0-9]+[^ ]*).*/\1/p' | head -n 1)
	if [ -n "$cver" ]; then ok "<Changes> starts with version $cver"; else warn "<Changes> has no '### X.Y.Z' heading"; fi
	if [ -n "$date" ] && ! printf '%s\n' "$changes" | head -n 1 | grep -qF "$date"; then bad "<Changes> first heading does not mention <Date> $date (a release names its date in both)"; fi
fi
beta=$(x 'string(/Container/Beta)')
case $cver in
0.*) if [ "$beta" = true ]; then ok "<Beta> true while the version is 0.x"; else bad "<Beta> must be true while the version is 0.x ($cver)"; fi ;;
*) [ "$beta" != true ] || [ -z "$cver" ] || warn "<Beta> is true for version $cver" ;;
esac

# ---- Config entries: one line each, dockerMan attribute values, no tag-like text
if lines=$(awk '/<Config[ >]/ { n = gsub(/<Config[ >]/, "&"); if (n > 1 || ($0 !~ /<\/Config>/ && $0 !~ /\/>[[:space:]]*$/)) print NR }' "$file"); [ -n "$lines" ]; then
	bad "Config element(s) not on a single line (dockerMan format): line(s) $(printf '%s' "$lines" | tr '\n' ' ')"
else
	ok "every <Config> is on one line"
fi
n=$(count /Container/Config)
names='' targets='' cfgok=true
i=1
while [ "$i" -le "$n" ]; do
	c="/Container/Config[$i]"
	cn=$(x "string($c/@Name)") ct=$(x "string($c/@Target)") ctype=$(x "string($c/@Type)")
	cdisp=$(x "string($c/@Display)") creq=$(x "string($c/@Required)") cmask=$(x "string($c/@Mask)")
	cmode=$(x "string($c/@Mode)") cdesc=$(x "string($c/@Description)") cdef=$(x "string($c/@Default)")
	cval=$(x "string($c)")
	label="Config '$cn'"
	[ -n "$cn" ] || { bad "Config #$i has no Name"; cfgok=false; }
	[ -n "$ct" ] || { bad "$label has no Target"; cfgok=false; }
	case $ctype in Port | Path | Variable | Label | Device) ;; *) bad "$label Type '$ctype' (Port, Path, Variable, Label or Device)"; cfgok=false ;; esac
	case $cdisp in always | advanced | always-hide | advanced-hide) ;; *) bad "$label Display '$cdisp'"; cfgok=false ;; esac
	case $creq in true | false) ;; *) bad "$label Required '$creq' (true or false)"; cfgok=false ;; esac
	case $cmask in true | false) ;; *) bad "$label Mask '$cmask' (true or false)"; cfgok=false ;; esac
	case $ctype in
	Port)
		has "$ct" '^[0-9]+$' || { bad "$label port Target '$ct' is not a number"; cfgok=false; }
		case $cmode in tcp | udp) ;; *) bad "$label port Mode '$cmode' (tcp or udp)"; cfgok=false ;; esac ;;
	Path)
		case $ct in /*) ;; *) bad "$label path Target '$ct' is not absolute"; cfgok=false ;; esac
		has "$cmode" '^(rw|ro)(,(slave|shared))?$' || { bad "$label path Mode '$cmode' (rw or ro)"; cfgok=false; } ;;
	esac
	for a in "$cn" "$cdesc" "$cdef" "$cval"; do
		if has "$a" '[<>]'; then bad "$label has < or > in its text (write some_var, not <some_var>)"; cfgok=false; break; fi
	done
	if [ "$ct" = TZ ]; then
		if [ -z "$cdef$cval" ]; then bad "$label: an empty TZ overrides the time zone Unraid injects; drop the entry"; cfgok=false; else warn "$label: Unraid already injects TZ; a TZ entry overrides the server's time zone"; fi
	fi
	if printf '%s\n' "$names" | grep -qxF -- "$cn"; then bad "$label: duplicate Name"; cfgok=false; fi
	if printf '%s\n' "$targets" | grep -qxF -- "$ctype:$ct"; then bad "$label: duplicate $ctype Target $ct"; cfgok=false; fi
	names="$names
$cn"
	targets="$targets
$ctype:$ct"
	i=$((i + 1))
done
[ "$cfgok" = false ] || ok "$n Config entries: valid Type/Display/Required/Mask/Mode, unique, no tag-like text"

# ---- Bunkarr's own required entries (they mirror deploy/docker-compose.yml; deploy/unraid_test.go
# compares the two in detail)
need() { # need TYPE TARGET WHAT [MODE]
	if [ "$(count "/Container/Config[@Type='$1' and @Target='$2']")" -lt 1 ]; then bad "Bunkarr: $3 missing ($1 $2)"; return 0; fi
	if [ -n "${4-}" ]; then
		m=$(x "string(/Container/Config[@Type='$1' and @Target='$2']/@Mode)")
		if [ "$m" != "$4" ]; then bad "Bunkarr: $3 ($1 $2) needs Mode $4 (is '$m')"; return 0; fi
	fi
	ok "Bunkarr: $3 ($1 $2${4:+ $4})"
}
need Port 8787 "web UI port" tcp
need Path /config "appdata" rw
need Path /media "media, read-only" ro
need Path /plex "Plex data, read-only" ro
need Path /backup "backup destination, slave propagation for Unassigned Devices" rw,slave
for a in sonarr radarr lidarr; do need Path "/arr/$a-backups" "$a Backups folder, read-only" ro; done
need Variable PUID "PUID"
need Variable PGID "PGID"
need Variable UMASK "UMASK"
case " $ep " in
*' --hostname=bunkarr-'*) ok "Bunkarr: <ExtraParams> sets a stable host name" ;;
*) bad "Bunkarr: <ExtraParams> must set --hostname=bunkarr-NAME (restic tells stale locks apart by host name)" ;;
esac
[ "$(x "string(/Container/Config[@Target='PUID']/@Default)")" = 99 ] || warn "PUID default is not 99 (Unraid's nobody)"
[ "$(x "string(/Container/Config[@Target='PGID']/@Default)")" = 100 ] || warn "PGID default is not 100 (Unraid's users)"
if [ "$(count "/Container/Config[@Target='TZ']")" -eq 0 ]; then ok "no TZ entry (Unraid injects the server's TZ)"; fi

# ------------------------------------------------------------------------------ repository
if [ -n "$repo" ]; then
	if [ ! -d "$repo" ]; then
		bad "repository directory not found: $repo"
	else
		# git mode only when $repo is itself the top level: inside a parent work tree (an export
		# unpacked under a repository), git ls-files would list the parent's index, which holds
		# nothing of this tree, and every rule below would pass on an empty list. Physical paths
		# on both sides (git prints them resolved).
		repo_phys=$(cd "$repo" && pwd -P)
		if top=$(git -C "$repo" rev-parse --show-toplevel 2>/dev/null) && [ -d "$top" ]; then
			top=$(cd "$top" && pwd -P)
		else
			top=''
		fi
		if [ "$top" = "$repo_phys" ]; then
			files=$(git -C "$repo" ls-files)
			info "repository files: git ls-files in $repo (what a clone of the committed tree contains)"
		else
			files=$(cd "$repo" && find . -type f ! -path './.git/*' | sed 's#^\./##')
			# The scratch folder holds this run's own check files, which are never published.
			scratch_rel=''
			if [ -n "$scratch" ] && [ -d "$scratch" ]; then
				scratch_phys=$(cd "$scratch" && pwd -P)
				case $scratch_phys/ in "$repo_phys"/?*) scratch_rel=${scratch_phys#"$repo_phys"/} ;; esac
			fi
			if [ -n "$scratch_rel" ]; then
				files=$(printf '%s\n' "$files" | awk -v p="$scratch_rel/" 'index($0, p) != 1')
				info "repository files: every file under $repo except $scratch_rel/ (not the top level of a git work tree)"
			else
				info "repository files: every file under $repo (not the top level of a git work tree)"
			fi
		fi
		if dep=$(printf '%s\n' "$files" | grep -E '(^|/)deprecated/'); then
			bad "a folder named deprecated/ exists (CA deprecates everything in it): $(printf '%s' "$dep" | head -n 3 | tr '\n' ' ')"
		else
			ok "no deprecated/ folder"
		fi
		lic=''
		for f in LICENSE LICENSE.md LICENSE.txt COPYING; do
			[ -f "$repo/$f" ] && { lic=$f; break; }
		done
		if [ -z "$lic" ]; then
			bad "no LICENSE at the repository root (CA requires an OSI-approved license)"
		elif grep -q 'GNU GENERAL PUBLIC LICENSE' "$repo/$lic" && grep -q 'Version 3' "$repo/$lic"; then
			ok "$lic at the root: GNU GPL v3 (OSI-approved)"
		elif grep -Eq 'MIT License|Apache License|Mozilla Public License|BSD' "$repo/$lic"; then
			ok "$lic at the root (OSI-approved family)"
		else
			warn "$lic at the root: check that GitHub detects an OSI-approved SPDX id"
		fi
		if printf '%s\n' "$files" | grep -qx 'ca_profile.xml'; then
			ok "ca_profile.xml at the root is committed"
		elif [ -f "$repo/ca_profile.xml" ]; then
			warn "ca_profile.xml exists at the root but is not committed"
		else
			warn "no ca_profile.xml at the root yet: run make ca-profile and commit it (the submission cannot be finalized without it)"
		fi
		dupes='' others=''
		for f in $(printf '%s\n' "$files" | grep -Ei '\.xml$' || :); do
			[ "$f" != "$as" ] || continue
			[ "$f" != ca_profile.xml ] || continue
			p="$repo/$f"
			[ -f "$p" ] || continue
			cnt=$(x 'count(/Container/Repository | /Container/PluginURL)' "$p")
			if [ "${cnt:-0}" = 0 ]; then
				warn "$f is an .xml file without <Repository>: CA lists it as 'Not an unRaid Application' (harmless, but noisy)"
				continue
			fi
			on=$(x 'string(/Container/Name)' "$p")
			if [ "$on" = "$name" ]; then dupes="$dupes $f"; else others="$others $f($on)"; fi
			if grep -Eqi -- "$PRIVATE_RE" "$p"; then bad "$f: another template with private addresses would be published"; fi
		done
		if [ -n "$dupes" ]; then
			bad "another template named '$name' is committed:$dupes (CA scans every .xml and drops one of two templates sharing a name)"
		else
			ok "exactly one committed template named '$name' ($as)"
		fi
		[ -z "$others" ] || info "other templates CA would also list:$others"
		if printf '%s\n' "$files" | grep -q '^unraid/ca/out/'; then
			bad "rendered files under unraid/ca/out/ are in the repository (committed: git rm --cached them; elsewhere: delete them, or pass that folder as --scratch)"
		fi
	fi
fi

summary || exit 1
