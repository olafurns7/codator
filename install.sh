#!/bin/sh
# Install a verified Codator release binary on Linux or macOS.
set -eu
umask 077

repo=https://github.com/olafurns7/codator

fail() {
	printf '%s\n' "codator installer: $*" >&2
	exit 1
}

shell_quote() {
	printf "'"
	printf '%s' "$1" | sed "s/'/'\\\\''/g"
	printf "'"
}

for command in awk chmod cp curl gh mkdir mktemp mv rm sed uname; do
	command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done

gh_version=$(gh version 2>/dev/null | awk 'NR == 1 { print $3; exit }') || fail "cannot determine GitHub CLI version"
if ! printf '%s\n' "$gh_version" | awk -F. '
NF != 3 { exit 1 }
$1 !~ /^[0-9][0-9]*$/ || $2 !~ /^[0-9][0-9]*$/ || $3 !~ /^[0-9][0-9]*$/ { exit 1 }
($1 > 2 || ($1 == 2 && ($2 > 86 || ($2 == 86 && $3 >= 0)))) { exit 0 }
{ exit 1 }
'; then
	fail "GitHub CLI 2.86.0 or newer is required"
fi

if command -v sha256sum >/dev/null 2>&1; then
	checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
	checksum_tool=shasum
else
	fail "required command not found: sha256sum or shasum"
fi

case "$(uname -s)" in
	Linux) platform=linux ;;
	Darwin) platform=darwin ;;
	*) fail "unsupported OS: $(uname -s) (supported: Linux, Darwin)" ;;
esac

case "$(uname -m)" in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) fail "unsupported architecture: $(uname -m) (supported: x86_64, aarch64)" ;;
esac

version=${CODATOR_VERSION:-latest}
case "$version" in
	latest)
		latest_url=$(curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output /dev/null --write-out '%{url_effective}' "$repo/releases/latest") || fail "cannot resolve latest release"
		latest_prefix=$repo/releases/tag/
		case "$latest_url" in
			"$latest_prefix"*) version=${latest_url#"$latest_prefix"} ;;
			*) fail "latest release redirect is not a Codator tag" ;;
		esac
		;;
	v)
		fail "CODATOR_VERSION must be latest or a v-prefixed release tag"
		;;
	v*[!A-Za-z0-9._-]*)
		fail "CODATOR_VERSION must be latest or a v-prefixed release tag"
		;;
	v*) ;;
	*) fail "CODATOR_VERSION must be latest or a v-prefixed release tag" ;;
esac
case "$version" in
	v) fail "latest release redirect is not a valid v-prefixed release tag" ;;
	v*[!A-Za-z0-9._-]*) fail "latest release redirect is not a valid v-prefixed release tag" ;;
	v*) ;;
	*) fail "latest release redirect is not a valid v-prefixed release tag" ;;
esac
download_base=$repo/releases/download/$version

if [ -n "${CODATOR_INSTALL_DIR:-}" ]; then
	install_dir=$CODATOR_INSTALL_DIR
elif [ -n "${HOME:-}" ]; then
	install_dir=$HOME/.local/bin
else
	fail "HOME is not set; set CODATOR_INSTALL_DIR"
fi

asset=codator-$platform-$arch
tmp=$(mktemp -d "${TMPDIR:-/tmp}/codator.XXXXXX") || fail "cannot create temporary directory"
stage=
cleanup() {
	rm -rf "$tmp"
	if [ -n "$stage" ]; then
		rm -f "$stage"
	fi
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

download() {
	curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$2" "$1"
}

download "$download_base/SHA256SUMS" "$tmp/SHA256SUMS" || fail "cannot download SHA256SUMS"
download "$download_base/$asset" "$tmp/$asset" || fail "cannot download $asset"
download "$download_base/attestations.jsonl" "$tmp/attestations.jsonl" || fail "cannot download attestations.jsonl"

expected=$(awk -v name="$asset" '
($2 == name || $2 == "*" name) { count++; value = $1 }
END { if (count != 1) exit 1; print value }
' "$tmp/SHA256SUMS") || fail "SHA256SUMS has no unique checksum for $asset"
case "$expected" in
	''|*[!0123456789abcdefABCDEF]*) fail "invalid SHA256 checksum for $asset" ;;
esac
[ "${#expected}" -eq 64 ] || fail "invalid SHA256 checksum for $asset"
printf '%s  %s\n' "$expected" "$asset" > "$tmp/checksum"
case "$checksum_tool" in
	sha256sum) (cd "$tmp" && sha256sum -c checksum) || fail "checksum verification failed for $asset" ;;
	shasum) (cd "$tmp" && shasum -a 256 -c checksum) || fail "checksum verification failed for $asset" ;;
esac

# The exact certificate identity binds the hosted release workflow and tag.
gh attestation verify "$tmp/$asset" \
	--bundle "$tmp/attestations.jsonl" \
	--repo olafurns7/codator \
	--cert-identity "https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/$version" \
	--source-ref "refs/tags/$version" \
	--deny-self-hosted-runners \
	--hostname github.com || fail "attestation verification failed for $asset"

mkdir -p "$install_dir" || fail "cannot create $install_dir"
destination=$install_dir/codator
[ ! -d "$destination" ] || fail "$destination is a directory"
stage=$(mktemp "$install_dir/.codator.XXXXXX") || fail "cannot stage installation in $install_dir"
cp "$tmp/$asset" "$stage" || fail "cannot stage $asset"
chmod 0755 "$stage" || fail "cannot mark Codator executable"
mv -f "$stage" "$destination" || fail "cannot install Codator"
stage=

printf 'Installed Codator at %s\n' "$destination"
doctor_supported=false
if "$destination" --help 2>&1 | awk '
$1 == "codator" && $2 == "doctor" { found = 1 }
END { exit !found }
'; then
	doctor_supported=true
fi
native_found=false
for native in codex claude; do
	if command -v "$native" >/dev/null 2>&1; then
		native_found=true
		if [ "$doctor_supported" = true ]; then
			printf 'Checking %s prerequisites:\n' "$native"
			if ! "$destination" doctor "$native"; then
				printf 'Codator is installed, but %s prerequisites need attention. Fix the doctor result, then run codator doctor %s again.\n' "$native" "$native" >&2
			fi
		fi
	fi
done
if [ "$doctor_supported" = false ]; then
	printf 'This Codator release does not support doctor; skipping prerequisite checks.\n'
fi
if [ "$native_found" = false ]; then
	if [ "$doctor_supported" = true ]; then
		printf 'Codator is installed. Install the Codex or Claude native CLI: https://github.com/olafurns7/codator#requirements, then run codator doctor.\n'
	else
		printf 'Codator is installed. Install the Codex or Claude native CLI: https://github.com/olafurns7/codator#requirements.\n'
	fi
fi
case ":${PATH:-}:" in
	*":$install_dir:"*) ;;
	*)
		printf 'Add this to your shell profile, then open a new shell:\n'
		printf '  export PATH=%s:"$PATH"\n' "$(shell_quote "$install_dir")"
		;;
esac
