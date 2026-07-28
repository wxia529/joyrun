#!/bin/sh

set -eu

repository_root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
installer="${repository_root}/scripts/install.sh"
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/joyrun-installer-test.XXXXXX")
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

case "$(uname -s)" in
Linux) goos=linux ;;
Darwin) goos=darwin ;;
*) printf 'installer test is unsupported on %s\n' "$(uname -s)" >&2; exit 1 ;;
esac

case "$(uname -m)" in
x86_64 | amd64) goarch=amd64 ;;
arm64 | aarch64) goarch=arm64 ;;
*) printf 'installer test is unsupported on %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

release_dir="${temporary_dir}/releases"
mock_bin="${temporary_dir}/bin"
install_dir="${temporary_dir}/install"
mkdir -p "$release_dir" "$mock_bin"

cat >"${mock_bin}/curl" <<'EOF'
#!/bin/sh
set -eu

output=
write_out=
url=
while [ "$#" -gt 0 ]; do
	case "$1" in
	--output)
		output=$2
		shift 2
		;;
	--write-out)
		write_out=$2
		shift 2
		;;
	--fail | --silent | --show-error | --location)
		shift
		;;
	*)
		url=$1
		shift
		;;
	esac
done

if [ -n "$write_out" ]; then
	printf 'https://github.com/wxia529/joyrun/releases/tag/v0.1.1'
	exit 0
fi

[ -n "$output" ] && [ -n "$url" ]
cp "${JOYRUN_TEST_RELEASES}/${url##*/}" "$output"
EOF
chmod 0755 "${mock_bin}/curl"

make_release() {
	release_version=$1
	package="joyrun-${release_version}-${goos}-${goarch}"
	mkdir -p "${temporary_dir}/build/${package}"
	(
		cd "$repository_root"
		go build -buildvcs=false -trimpath \
			-ldflags "-X main.version=${release_version}" \
			-o "${temporary_dir}/build/${package}/joyrun" ./cmd/joyrun
	)
	tar -C "${temporary_dir}/build" \
		-czf "${release_dir}/${package}.tar.gz" "$package"
	if command -v sha256sum >/dev/null 2>&1; then
		(
			cd "$release_dir"
			sha256sum "${package}.tar.gz" >SHA256SUMS
		)
	else
		checksum=$(shasum -a 256 "${release_dir}/${package}.tar.gz" | awk '{print $1}')
		printf '%s  %s\n' "$checksum" "${package}.tar.gz" \
			>"${release_dir}/SHA256SUMS"
	fi
}

export JOYRUN_TEST_RELEASES="$release_dir"
PATH="${mock_bin}:${PATH}"
export PATH

check_dir="${temporary_dir}/check-only"
check_output=$("$installer" --check --install-dir "$check_dir")
printf '%s\n' "$check_output" | grep -q 'Latest stable: v0.1.1'
[ ! -e "$check_dir" ]

make_release v0.1.0
"$installer" --version v0.1.0 --install-dir "$install_dir"
[ "$("${install_dir}/joyrun" version)" = "joyrun v0.1.0" ]

cp "${release_dir}/joyrun-v0.1.0-${goos}-${goarch}.tar.gz" \
	"${temporary_dir}/untampered.tar.gz"
printf 'tampered\n' >>"${release_dir}/joyrun-v0.1.0-${goos}-${goarch}.tar.gz"
if "$installer" --version v0.1.0 --install-dir "$install_dir"; then
	printf 'tampered archive was accepted\n' >&2
	exit 1
fi
[ "$("${install_dir}/joyrun" version)" = "joyrun v0.1.0" ]

make_release v0.1.1
"$installer" --version v0.1.1 --install-dir "$install_dir"
[ "$("${install_dir}/joyrun" version)" = "joyrun v0.1.1" ]
[ "$("${install_dir}/joyrun.previous" version)" = "joyrun v0.1.0" ]

printf 'installer integration test passed\n'
