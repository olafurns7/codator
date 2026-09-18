# Codator

Codator is a Linux and macOS wrapper for separate [Codex](https://learn.chatgpt.com/docs/codex/cli) and [Claude Code](https://code.claude.com/docs/en/setup) profiles. Give each subscribed account a local label; Codator checks the native CLI's usage data and chooses the eligible label with the most percentage headroom. A label is only a local profile name, not an account ID.

Automatic selection skips busy, unknown, exhausted, spend-capped, and non-subscription profiles. Codator is not affiliated with either provider and does not provide or alter account access.

## Requirements

Codator release binaries support Linux x86_64/amd64 and aarch64/arm64, plus macOS 12 or later on Intel (amd64) and Apple Silicon (arm64). They need no Go runtime. Install the native CLI you plan to use and ensure it is on PATH. Native CLI requirements still apply: current Claude Code requires macOS 13 or later; Codator's macOS 12 support does not make Claude Code available on macOS 12.

~~~sh
# Codex: https://learn.chatgpt.com/docs/codex/cli
curl -fsSL https://chatgpt.com/codex/install.sh | sh

# Claude Code: https://code.claude.com/docs/en/setup
curl -fsSL https://claude.ai/install.sh | bash
~~~

The Codator installer needs curl, GitHub CLI `gh` 2.86.0 or newer, standard POSIX shell utilities, and `sha256sum` (Linux) or `shasum -a 256` (macOS). It downloads the selected GitHub release asset, checksum manifest, and attestation bundle over HTTPS, verifies the checksum and the binary's GitHub artifact attestation against this repository's release workflow and tag, and atomically replaces the binary only after verification. The bundled `gh attestation verify --bundle ...` check does not need `gh login`; the invoked GitHub CLI may still read its normal local configuration. See the [GitHub attestation verification documentation](https://cli.github.com/manual/gh_attestation_verify).

## Install

~~~sh
(
set -eu
umask 077
repo=https://github.com/olafurns7/codator

fail() {
  printf '%s\n' "codator bootstrap: $*" >&2
  exit 1
}

for command in awk curl gh mktemp rm; do
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
  v) fail "CODATOR_VERSION must be latest or a v-prefixed release tag" ;;
  v*[!A-Za-z0-9._-]*) fail "CODATOR_VERSION must be latest or a v-prefixed release tag" ;;
  v*) ;;
  *) fail "CODATOR_VERSION must be latest or a v-prefixed release tag" ;;
esac
case "$version" in
  v) fail "latest release redirect is not a valid v-prefixed release tag" ;;
  v*[!A-Za-z0-9._-]*) fail "latest release redirect is not a valid v-prefixed release tag" ;;
  v*) ;;
  *) fail "latest release redirect is not a valid v-prefixed release tag" ;;
esac

tmp=$(mktemp -d "${TMPDIR:-/tmp}/codator-bootstrap.XXXXXX") || fail "cannot create temporary directory"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

download() {
  curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 --output "$2" "$1"
}

download "$repo/releases/download/$version/install.sh" "$tmp/install.sh" || fail "cannot download install.sh"
download "$repo/releases/download/$version/attestations.jsonl" "$tmp/attestations.jsonl" || fail "cannot download attestations.jsonl"
# The exact certificate identity binds the hosted release workflow and tag.
gh attestation verify "$tmp/install.sh" \
  --bundle "$tmp/attestations.jsonl" \
  --repo olafurns7/codator \
  --cert-identity "https://github.com/olafurns7/codator/.github/workflows/release.yml@refs/tags/$version" \
  --source-ref "refs/tags/$version" \
  --deny-self-hosted-runners \
  --hostname github.com || fail "attestation verification failed for install.sh"

CODATOR_VERSION="$version" sh "$tmp/install.sh"
)
~~~

The snippet is self-contained and resolves `latest` once, then verifies the complete installer and its bundle from the same tag before running it. Leave `CODATOR_VERSION` unset for the latest tag, or set it to a real signed `v...` release tag before running the snippet. Set `CODATOR_INSTALL_DIR` before the snippet to choose another destination, for example `export CODATOR_INSTALL_DIR="$HOME/bin"`. By default this installs codator in `~/.local/bin`. If that directory is not already in PATH, the installer prints the exact export to add to `~/.zshrc` or `~/.bashrc`; reload it, for example:

~~~sh
. ~/.zshrc
~~~

After a verified install, the installer runs `codator doctor` for each native CLI it finds, when the installed release supports it. Older releases get a short notice instead. A missing optional provider does not block installation. If a prerequisite needs attention, the binary is still installed and the installer prints the doctor result; fix it before that provider is launched. If neither native CLI is installed, it prints the native-install next step. The installer never uses `sudo` or changes shell, security, or profile settings. Releases without `attestations.jsonl` fail closed; this includes v0.3.2 and earlier, which predate provenance bundles. They require migration to a later signed release. Until one is available, the supported fallback is to build from a reviewed source checkout with the documented Go toolchain rather than downloading an unverified binary.

For Codex on Linux, install the distribution `bubblewrap` package so `bwrap` is on PATH. After Codator is installed, check the native CLI you plan to use before login or launch:

~~~sh
codator doctor codex
# or, for a Claude-only installation
codator doctor claude
~~~

`doctor` is read-only: it does not access accounts, usage, credentials, or profiles. It checks only the selected native CLI. On Linux, the Codex check also runs a short harmless `bwrap` user-and-network namespace command. On macOS, Codex uses Seatbelt and does not need `bwrap`.

If Ubuntu 24.04 still reports a blocked namespace after `bubblewrap` is installed, use the supported AppArmor profile procedure from the [Codex sandbox prerequisites](https://learn.chatgpt.com/docs/sandboxing#prerequisites):

~~~sh
sudo apt update
sudo apt install bubblewrap apparmor-profiles apparmor-utils
sudo install -m 0644 \
  /usr/share/apparmor/extra-profiles/bwrap-userns-restrict \
  /etc/apparmor.d/bwrap-userns-restrict
sudo apparmor_parser -r /etc/apparmor.d/bwrap-userns-restrict
~~~

Do not disable the sandbox or the system-wide user-namespace restriction. For other platforms, follow the upstream prerequisite guidance instead of applying guessed system changes.

## Use

Install and use either provider independently. The headless Codex example forwards its native --device-auth option:

~~~sh
# Codex only
codator doctor codex
codator login codex personal
codator login codex team -- --device-auth
codator codex

# Claude only
codator doctor claude
codator login claude personal
codator claude --account personal --model sonnet
~~~

Launch without an account to select automatically, or choose a label explicitly:

~~~sh
codator codex
codator claude --account personal --model sonnet
codator codex --account personal exec -- "explain this repository"
~~~

Codator consumes only a leading --account NAME; every other native argument is forwarded unchanged and in order, including --.

For an optional `cdx` shortcut that selects an account automatically and starts Codex with full access and no command approvals, install the helper from a source checkout:

> Warning: `cdx` deliberately requests full access (`danger-full-access`) and no command approvals. Use it only when you trust the repository, files, native CLI, plugins, and command inputs; use normal `codator codex` commands when you want the native sandbox and approval flow.

~~~sh
install -m 0755 scripts/cdx "$HOME/.local/bin/cdx"
unalias cdx 2>/dev/null || true
cdx
cdx --account personal --model gpt-5.6-luna
~~~

The helper keeps native flags available and forwards them unchanged. Remove any old `alias cdx='codator codex'` from your shell configuration, then reload it, for example with `. ~/.zshrc`.

For a `cdl` shortcut that starts Claude with `--dangerously-skip-permissions --permission-mode bypassPermissions`, install the helper from a source checkout:

> Warning: `cdl` deliberately bypasses Claude permission checks, so it can run with no approval prompts. Use it only when you trust the repository, files, native CLI, plugins, and command inputs; use normal `codator claude` commands for the ordinary permission flow.

~~~sh
install -m 0755 scripts/cdl "$HOME/.local/bin/cdl"
unalias cdl 2>/dev/null || true
cdl
cdl --account personal --model sonnet
~~~

Remove any old `alias cdl='codator claude'` from your shell configuration so it does not override the helper, then reload that file. The helper preserves leading `--account NAME` selection, native arguments, and the account-free help/version commands. Bypass mode skips Claude's permission checks; it is an explicit choice made by this shortcut. Normal `codator codex` and `codator claude` commands do not add either helper's bypass flags.

Each label has its own native settings, sessions, and history beneath ${XDG_DATA_HOME:-$HOME/.local/share}/codator/. Files and directories use private permissions, but filesystem permissions are not encryption. This is same-UID profile separation, not OS isolation: Codator trusts the user's home and environment, the native CLI, installed plugins, and PATH, and it cannot protect one same-UID process from another. Codator leaves provider credentials to their native CLIs. Credential-changing commands retain an exclusive profile lock; normal Codex and Claude sessions release it after selection, so concurrent normal sessions can share a profile.

### Claude setup recovery

Some native Claude versions reopen login-method setup for an already signed-in isolated profile when its safe private config has a saved `oauthAccount` but lacks `hasCompletedOnboarding`. Just before a selected Claude launch, Codator can atomically add only `hasCompletedOnboarding: true` when all of these conditions hold:

- the preferred existing private config (`.config.json`, or `.claude.json` when the preferred file is absent) is safe and well-formed;
- it has a saved OAuth account and the onboarding field is absent; and
- an isolated, bounded `claude auth status --json` confirms a logged-in Claude.ai first-party subscription.

Codator does not probe again when the field is already present, including `false` or `null`. It does not create configs, change credentials, tokens, endpoints, workspace trust, permissions, privacy choices, bypass acceptance, or model settings. A malformed, unsafe, unauthenticated, or timed-out status check leaves the config unchanged and allows native Claude setup or login to continue. Wrapper signal cancellation also leaves the config unchanged, stops the launch, and terminates the status probe. New or genuinely signed-out profiles still require `codator login claude NAME`.

## Status and selection

~~~sh
codator status
codator status codex
codator status claude
codator doctor codex
~~~

Claude selection treats session and shared weekly limits separately from model-specific weekly limits. An explicit recognized --model can filter that model's limit, while shared limits always apply; an omitted or ambiguous model remains conservative. In status, a depleted Fable limit therefore does not mean the all-model weekly capacity is depleted. Automatic selection skips unknown quota; an explicit account may launch when its subscription identity is verified even if quota is unknown, but a known-ineligible account is still refused. Codator does not set or enforce a provider spending cap. busy means another guarded operation holds the profile lock; unknown means Codator could not safely establish usable usage data; Claude cooldown output means it is temporarily avoiding another failed or unsafe usage probe.

## Update, uninstall, and build

Run the installer again to update (or set CODATOR_VERSION to pin it). To uninstall, remove only the installed binary, for example rm ~/.local/bin/codator. Neither action removes the profile data under ${XDG_DATA_HOME:-$HOME/.local/share}/codator/.

Build from source with Go 1.26.8 or newer, the minimum supported patched toolchain:

~~~sh
go build -trimpath -o codator .
~~~

Maintainers build four release assets and their SHA256SUMS manifest with:

~~~sh
scripts/release.sh
~~~

Pushing a v* tag runs checks, builds those assets, and publishes the GitHub release. Go 1.26 is the last Go series supported on macOS 12. Before Go 1.26 support ends, move macOS 12 users to a supported Go branch or raise the project's macOS minimum; do not treat an indefinitely frozen 1.26 toolchain as the update policy. Do not publish a tag until the release checks have passed.

The assets are `codator-linux-amd64`, `codator-linux-arm64`, `codator-darwin-amd64`, and `codator-darwin-arm64`. Releases also include optional `cdx` and `cdl` helper downloads; install those manually only if wanted. The binary installer does not install shortcuts. Native macOS CI uses fake native adapters; it does not verify a Claude or Codex account flow on macOS.

See the [security policy and reporting instructions](SECURITY.md).

## License

Codator is available under the [MIT License](LICENSE).
