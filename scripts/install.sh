#!/bin/sh
#
# Installs Lotus on macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/MohammadYzbk/lotus/main/scripts/install.sh | sh
#
# Why this exists: releases are not signed with an Apple Developer ID, so
# Gatekeeper refuses to open them and the writer has to go to System Settings
# and press "Open Anyway". A script the writer deliberately runs is not
# something Gatekeeper gates, so this can clear the download flag on the way
# past — on a copy the writer asked for, by name, from a URL they typed.
#
# That is the whole trick, and it is worth being plain about it: the last step
# removes the quarantine attribute from the app it just installed. Nothing
# else on the machine is touched, and the flag is removed from one path, never
# broadly. The honest fix is a signed release; this is the stopgap.

set -eu

REPO="MohammadYzbk/lotus"
APP="Lotus.app"
PREFIX="/Applications"
VERSION=""
LOCAL_DMG=""

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<'EOF'
Usage: install.sh [options]

  --version <v>   Install this release (e.g. 0.1.0). Default: the latest.
  --dmg <path>    Install from a .dmg already on this machine instead of
                  downloading one. Useful for a build someone handed you.
  --prefix <dir>  Install into this directory. Default: /Applications.
  -h, --help      Show this.
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || die "--version needs a value"; VERSION="$2"; shift 2 ;;
	--dmg) [ $# -ge 2 ] || die "--dmg needs a path"; LOCAL_DMG="$2"; shift 2 ;;
	--prefix) [ $# -ge 2 ] || die "--prefix needs a path"; PREFIX="$2"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) die "unknown option: $1" ;;
	esac
done

[ "$(uname -s)" = "Darwin" ] ||
	die "this installer is for macOS. On Linux, download the tar.gz from
       https://github.com/$REPO/releases and run ./lotus from it."

for tool in curl hdiutil ditto xattr shasum; do
	command -v "$tool" >/dev/null 2>&1 || die "$tool is required but not installed"
done

# An app that is open cannot be replaced coherently: the copy would land
# underneath a running process and the writer would keep using the old one
# until they happened to quit it.
if pgrep -x lotus >/dev/null 2>&1; then
	die "Lotus is running. Quit it and try again."
fi

case "$(uname -m)" in
arm64) ASSET="lotus-macos-arm64.dmg" ;;
x86_64) ASSET="lotus-macos-amd64.dmg" ;;
*) die "no Lotus build for $(uname -m)" ;;
esac

work="$(mktemp -d)"
mount=""
cleanup() {
	[ -n "$mount" ] && hdiutil detach "$mount" -quiet >/dev/null 2>&1 || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

dmg=""
if [ -n "$LOCAL_DMG" ]; then
	[ -f "$LOCAL_DMG" ] || die "no such file: $LOCAL_DMG"
	dmg="$LOCAL_DMG"
	say "Installing from $dmg"
else
	if [ -z "$VERSION" ]; then
		say "Looking up the latest release…"
		# One field, read with grep rather than a JSON parser: macOS ships no
		# jq, and requiring python3 would mean requiring the developer tools.
		tag="$(curl -fsL "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
			grep '"tag_name"' | head -n 1 | cut -d'"' -f4)" || tag=""
		[ -n "$tag" ] ||
			die "could not find a release. If none has been published yet, build from
       source instead: https://github.com/$REPO#development"
	else
		tag="v${VERSION#v}"
	fi
	say "Downloading Lotus ${tag}…"

	base="https://github.com/$REPO/releases/download/$tag"
	dmg="$work/$ASSET"
	curl -fL --progress-bar -o "$dmg" "$base/$ASSET" ||
		die "could not download $ASSET from $tag"

	# Checked against the release's own checksums file. This catches a
	# truncated or interrupted download, which is the failure that actually
	# happens; it is not an independent attestation, because the same host
	# serves both the file and the sums.
	if curl -fsSL -o "$work/checksums.txt" "$base/checksums.txt" 2>/dev/null; then
		# Matched on the exact name, with sha256sum's "./" prefix allowed for:
		# a grep pattern would treat the dot in ".dmg" as any character.
		want="$(awk -v want="$ASSET" \
			'{ name = $2; sub(/^\.\//, "", name); if (name == want) print $1 }' \
			"$work/checksums.txt")"
		got="$(shasum -a 256 "$dmg" | awk '{print $1}')"
		if [ -z "$want" ]; then
			say "Note: $ASSET is not listed in this release's checksums."
		elif [ "$want" != "$got" ]; then
			die "the download does not match its checksum — expected $want, got $got"
		else
			say "Checksum verified."
		fi
	else
		say "Note: this release publishes no checksums; skipping verification."
	fi
fi

say "Opening the disk image…"
mount="$work/mnt"
mkdir -p "$mount"
hdiutil attach "$dmg" -nobrowse -quiet -mountpoint "$mount" ||
	die "could not open $dmg"

[ -d "$mount/$APP" ] || die "$dmg does not contain $APP"

[ -d "$PREFIX" ] || die "no such directory: $PREFIX"
if [ ! -w "$PREFIX" ]; then
	die "$PREFIX is not writable. Either run this with sudo, or install somewhere
       you own:  install.sh --prefix ~/Applications"
fi

target="$PREFIX/$APP"
if [ -e "$target" ]; then
	say "Replacing the copy already in ${PREFIX}…"
	rm -rf "$target"
fi

# ditto rather than cp: it preserves the extended attributes and symlinks a
# bundle's signature is computed over, and cp -R has mangled both before.
say "Installing to ${PREFIX}…"
ditto "$mount/$APP" "$target" || die "could not copy $APP into $PREFIX"

hdiutil detach "$mount" -quiet >/dev/null 2>&1 || true
mount=""

# The point of the exercise. Scoped to the one bundle just installed.
if xattr -p com.apple.quarantine "$target" >/dev/null 2>&1; then
	say "Clearing the download flag so macOS will open it…"
	xattr -dr com.apple.quarantine "$target"
fi

# An app that will not launch is worth catching here rather than leaving the
# writer to discover it by double-clicking.
if ! codesign --verify --deep --strict "$target" >/dev/null 2>&1; then
	say ""
	say "Warning: the installed app's signature does not verify. It may not open."
	say "Please report this at https://github.com/$REPO/issues"
fi

say ""
say "Lotus is installed at $target"
say "Open it from Launchpad, or run: open -a \"$target\""
