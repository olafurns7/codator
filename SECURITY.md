# Security policy

## Supported code and releases

Security fixes are developed on main and supported in the latest release. The normal installer is a checksum-only convenience path: it downloads a GitHub release asset and SHA256SUMS over HTTPS and explicitly trusts GitHub and the maintainer's release assets. It does not require gh, and a checksum manifest provides consistency rather than independent authenticity.

CODATOR_VERIFY_ATTESTATION=1 enables the optional stronger path. The release workflow attests only SHA256SUMS, which contains the hashes of the eight release payloads. The installer verifies that manifest's attestation against the exact repository, release workflow, tag, source ref, GitHub issuer, and hosted-runner policy, then applies the manifest's unique checksum to the selected binary before staging it. Invalid values, missing bundles, verifier failures, manifest mismatches, and binary checksum failures fail closed. The optional provenance check is available for releases such as v0.5.4; it is not an everyday prerequisite.

## Optional provenance bootstrap

This recipe uses a real GitHub CLI to verify the downloaded installer's manifest before executing it. It then enables manifest verification again for the binary installer. Set CODATOR_VERSION to a concrete v... tag when overriding the documented v0.5.4 default.

~~~sh
(
set -eu
umask 077
repo=https://github.com/olafurns7/codator
repo_slug=olafurns7/codator
version=${CODATOR_VERSION-v0.5.4}

fail() {
  printf '%s\n' "codator secure bootstrap: $*" >&2
  exit 1
}

for command in awk curl gh mktemp rm; do
  command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done
case "$version" in
  v) fail "CODATOR_VERSION must be a concrete v-prefixed release tag" ;;
  v*[!A-Za-z0-9._-]*) fail "CODATOR_VERSION must be a concrete v-prefixed release tag" ;;
  v*) ;;
  *) fail "CODATOR_VERSION must be a concrete v-prefixed release tag" ;;
esac

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

tmp=$(mktemp -d "${TMPDIR-/tmp}/codator-secure-bootstrap.XXXXXX") || fail "cannot create temporary directory"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

download() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$2" "$1"
}

download "$repo/releases/download/$version/install.sh" "$tmp/install.sh" || fail "cannot download install.sh"
download "$repo/releases/download/$version/SHA256SUMS" "$tmp/SHA256SUMS" || fail "cannot download SHA256SUMS"
download "$repo/releases/download/$version/attestations.jsonl" "$tmp/attestations.jsonl" || fail "cannot download attestations.jsonl"
[ -s "$tmp/install.sh" ] || fail "downloaded install.sh is empty"
[ -s "$tmp/SHA256SUMS" ] || fail "downloaded SHA256SUMS is empty"
[ -s "$tmp/attestations.jsonl" ] || fail "downloaded attestations.jsonl is empty"

gh attestation verify "$tmp/SHA256SUMS" \
  --bundle "$tmp/attestations.jsonl" \
  --repo "$repo_slug" \
  --cert-identity "https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/$version" \
  --source-ref "refs/tags/$version" \
  --deny-self-hosted-runners \
  --hostname github.com || fail "attestation verification failed for SHA256SUMS"

expected=$(awk -v name=install.sh '
($2 == name || $2 == "*" name) { count++; value = $1 }
END {
  if (count != 1 || length(value) != 64 || value !~ /^[0-9A-Fa-f]+$/) exit 1
  print value
}' "$tmp/SHA256SUMS") || fail "SHA256SUMS has no unique valid checksum for install.sh"
printf '%s  install.sh\n' "$expected" > "$tmp/checksum"
case "$checksum_tool" in
  sha256sum) (cd "$tmp" && sha256sum -c checksum) || fail "installer checksum verification failed" ;;
  shasum) (cd "$tmp" && shasum -a 256 -c checksum) || fail "installer checksum verification failed" ;;
esac

CODATOR_VERSION="$version" CODATOR_VERIFY_ATTESTATION=1 sh "$tmp/install.sh"
)
~~~

The optional attestation proves that GitHub's hosted release workflow approved the maintainer-built files represented by the signed manifest. It does not prove that GitHub built the binaries. The default bootstrap remains intentionally simpler; use this recipe when that additional provenance check is worth the gh dependency.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting route for confidential reports:

<https://github.com/olafurns7/codator/security/advisories/new>

If that private route is unavailable, open a public issue only to request private contact:

<https://github.com/olafurns7/codator/issues/new?title=Please%20provide%20a%20private%20security%20contact>

Do not include vulnerability details, exploit code, credentials, tokens, or other secrets in that fallback issue. Wait for a private channel before sharing sensitive information.

## Trust limits

The optional installer check binds the signed manifest to this repository, the current release workflow, the exact tag and source ref, GitHub's issuer, and the hosted-runner policy. That establishes release-workflow approval of the manifest and the maintainer-built bytes whose hashes it contains; it does not prove that the source, repository, maintainer account, GitHub, native CLIs, plugins, PATH, or resulting code are uncompromised or safe. Profile separation is not OS isolation, and Codator does not replace provider authentication, authorization, or spending controls.
