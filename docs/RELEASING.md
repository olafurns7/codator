# Releasing Codator

The owner runs this sequence. Builds and tests stay local. The release workflow is one manual signing job: it downloads only SHA256SUMS from the matching draft, attests that manifest, and uploads only attestations.jsonl. It does not check out source, build or test, execute payloads, or publish the release.

## v0.5.0

At the final merged commit, run the local gates and release build with Go 1.26.8:

~~~sh
test -z "$(gofmt -l .)"
GOTOOLCHAIN=go1.26.8 go vet -p=1 ./...
GOTOOLCHAIN=go1.26.8 GOMAXPROCS=2 go test -race -p=1 ./...
for script in install.sh scripts/cdx scripts/cdl scripts/release.sh; do sh -n "$script"; done
GOTOOLCHAIN=go1.26.8 scripts/release.sh
./dist/codator-linux-amd64 --help
(cd dist && sha256sum -c SHA256SUMS)
~~~

Create the annotated tag only after those checks pass, and confirm the build source is exactly that tag commit:

~~~sh
git tag -a v0.5.0 -m 'Codator v0.5.0'
test "$(git rev-parse HEAD)" = "$(git rev-list -n1 v0.5.0)"
git push origin refs/tags/v0.5.0
~~~

Create a draft release and upload exactly these nine payloads:

~~~sh
gh release create v0.5.0 --repo olafurns7/codator --draft --verify-tag \
  --title 'Codator v0.5.0' \
  dist/codator-linux-amd64 \
  dist/codator-linux-arm64 \
  dist/codator-darwin-amd64 \
  dist/codator-darwin-arm64 \
  dist/cdx \
  dist/cdl \
  dist/install.sh \
  dist/LICENSE \
  dist/SHA256SUMS
~~~

Invoke the signing job manually with the hash of the local manifest:

~~~sh
checksums_sha256=$(sha256sum dist/SHA256SUMS | awk '{print $1}')
gh workflow run release.yml --repo olafurns7/codator --ref v0.5.0 \
  -f checksums_sha256="$checksums_sha256"
~~~

After the run completes, verify the returned manifest attestation and the local payloads before publishing. The attestation is for SHA256SUMS; the manifest then authenticates the eight payloads through their hashes.

~~~sh
verify_dir=$(mktemp -d "${TMPDIR:-/tmp}/codator-release-verify.XXXXXX")
trap 'rm -rf "$verify_dir"' 0 HUP INT TERM
gh release download v0.5.0 --repo olafurns7/codator --dir "$verify_dir" --pattern SHA256SUMS
gh release download v0.5.0 --repo olafurns7/codator --dir "$verify_dir" --pattern attestations.jsonl
cmp dist/SHA256SUMS "$verify_dir/SHA256SUMS"
gh attestation verify "$verify_dir/SHA256SUMS" \
  --bundle "$verify_dir/attestations.jsonl" \
  --repo olafurns7/codator \
  --cert-identity "https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/v0.5.0" \
  --source-ref refs/tags/v0.5.0 \
  --deny-self-hosted-runners \
  --hostname github.com
(cd dist && sha256sum -c SHA256SUMS)
~~~

Publish only after those local checks succeed:

~~~sh
gh release edit v0.5.0 --repo olafurns7/codator --draft=false
~~~
