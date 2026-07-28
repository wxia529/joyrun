#!/bin/sh

set -eu

repository="wxia529/joyrun"
releases_url="https://github.com/${repository}/releases"
version="latest"
install_dir="${JOYRUN_INSTALL_DIR:-${HOME}/.local/bin}"
check_only=false

usage() {
	cat <<'EOF'
Install or upgrade JoyRun from its official GitHub releases.

Usage:
  install.sh [--version VERSION] [--install-dir DIRECTORY] [--check]

Options:
  --version VERSION       Install a specific tag such as v0.1.0.
  --install-dir DIRECTORY Install into DIRECTORY (default: ~/.local/bin).
  --check                 Report the installed and latest versions without
                          changing any files.
  -h, --help              Show this help.

Environment:
  JOYRUN_INSTALL_DIR      Alternative default installation directory.
EOF
}

fail() {
	printf 'joyrun installer: %s\n' "$*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 ||
		fail "required command not found: $1"
}

download() {
	url=$1
	output=$2
	curl --fail --silent --show-error --location --output "$output" "$url"
}

resolve_latest_version() {
	effective_url=$(
		curl --fail --silent --show-error --location \
			--output /dev/null --write-out '%{url_effective}' \
			"${releases_url}/latest"
	)
	resolved=${effective_url##*/}
	validate_version "$resolved"
	printf '%s\n' "$resolved"
}

validate_version() {
	printf '%s\n' "$1" |
		grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
		fail "invalid release version: $1"
}

installed_version() {
	binary="${install_dir}/joyrun"
	if [ ! -x "$binary" ]; then
		binary=$(command -v joyrun 2>/dev/null || true)
	fi
	if [ -n "$binary" ] && [ -x "$binary" ]; then
		"$binary" version 2>/dev/null | sed -n 's/^joyrun //p'
	fi
}

detect_platform() {
	case "$(uname -s)" in
	Linux) goos=linux ;;
	Darwin) goos=darwin ;;
	*) fail "unsupported operating system: $(uname -s)" ;;
	esac

	case "$(uname -m)" in
	x86_64 | amd64) goarch=amd64 ;;
	arm64 | aarch64) goarch=arm64 ;;
	*) fail "unsupported CPU architecture: $(uname -m)" ;;
	esac
}

verify_checksum() {
	checksums=$1
	archive=$2
	asset=$3
	expected=$(
		awk -v asset="$asset" '
			{
				name = $2
				sub(/^\*/, "", name)
				if (name == asset) {
					print $1
					exit
				}
			}
		' "$checksums"
	)
	printf '%s\n' "$expected" | grep -Eq '^[0-9a-fA-F]{64}$' ||
		fail "SHA256SUMS does not contain a valid checksum for ${asset}"

	if command -v sha256sum >/dev/null 2>&1; then
		actual=$(sha256sum "$archive" | awk '{print $1}')
	elif command -v shasum >/dev/null 2>&1; then
		actual=$(shasum -a 256 "$archive" | awk '{print $1}')
	else
		fail "sha256sum or shasum is required to verify the download"
	fi

	[ "$actual" = "$expected" ] ||
		fail "checksum verification failed for ${asset}"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
	--version)
		[ "$#" -ge 2 ] || fail "--version requires a value"
		version=$2
		shift 2
		;;
	--install-dir)
		[ "$#" -ge 2 ] || fail "--install-dir requires a value"
		install_dir=$2
		shift 2
		;;
	--check)
		check_only=true
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		fail "unknown option: $1"
		;;
	esac
done

require_command curl

if [ "$version" = "latest" ]; then
	version=$(resolve_latest_version)
else
	validate_version "$version"
fi

current=$(installed_version)
if [ "$check_only" = true ]; then
	if [ -n "$current" ]; then
		printf 'Installed: %s\nLatest stable: %s\n' "$current" "$version"
	else
		printf 'Installed: not found\nLatest stable: %s\n' "$version"
	fi
	exit 0
fi

detect_platform
require_command tar

package="joyrun-${version}-${goos}-${goarch}"
asset="${package}.tar.gz"
download_base="${releases_url}/download/${version}"
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/joyrun-install.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

printf 'Downloading JoyRun %s for %s/%s...\n' "$version" "$goos" "$goarch"
download "${download_base}/${asset}" "${temporary_dir}/${asset}"
download "${download_base}/SHA256SUMS" "${temporary_dir}/SHA256SUMS"
verify_checksum \
	"${temporary_dir}/SHA256SUMS" \
	"${temporary_dir}/${asset}" \
	"$asset"

tar -xzf "${temporary_dir}/${asset}" -C "$temporary_dir"
candidate="${temporary_dir}/${package}/joyrun"
[ -f "$candidate" ] || fail "release archive does not contain ${package}/joyrun"
chmod 0755 "$candidate"

candidate_version=$("$candidate" version 2>/dev/null || true)
[ "$candidate_version" = "joyrun ${version}" ] ||
	fail "downloaded binary reports an unexpected version: ${candidate_version:-unknown}"

mkdir -p "$install_dir"
[ -d "$install_dir" ] || fail "installation path is not a directory: $install_dir"
[ -w "$install_dir" ] || fail "installation directory is not writable: $install_dir"

target="${install_dir}/joyrun"
backup="${install_dir}/joyrun.previous"
staged=$(mktemp "${install_dir}/.joyrun.new.XXXXXX")
cp "$candidate" "$staged"
chmod 0755 "$staged"

if [ -e "$target" ]; then
	cp -p "$target" "$backup"
fi

if ! mv -f "$staged" "$target"; then
	fail "cannot replace ${target}"
fi

if [ "$("$target" version 2>/dev/null || true)" != "joyrun ${version}" ]; then
	if [ -f "$backup" ]; then
		cp -p "$backup" "$target"
	else
		rm -f "$target"
	fi
	fail "installed binary failed verification; the previous installation was restored"
fi

printf 'Installed JoyRun %s to %s\n' "$version" "$target"
if [ -n "$current" ] && [ "$current" != "$version" ]; then
	printf 'Previous version: %s\n' "$current"
fi

case ":${PATH:-}:" in
*":${install_dir}:"*) ;;
*)
	printf '%s\n' "Add ${install_dir} to PATH before invoking joyrun by name." >&2
	;;
esac
