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

The default Codator installer needs curl, standard POSIX shell utilities, and `sha256sum` (Linux) or `shasum -a 256` (macOS). It resolves `latest` once when requested, downloads the selected GitHub release asset and `SHA256SUMS` over HTTPS, verifies the binary checksum, and atomically replaces the binary only after verification. It does not require GitHub CLI `gh`. A checksum manifest provides release consistency, not independent authenticity: this path explicitly trusts GitHub and the maintainer's release assets.

Set `CODATOR_VERIFY_ATTESTATION=1` when you want the stronger optional provenance check. That mode requires GitHub CLI `gh` 2.86.0 or newer, downloads `attestations.jsonl`, and verifies the downloaded `SHA256SUMS` against this repository, the exact release workflow and tag, the matching source ref, GitHub's issuer, and hosted-runner policy before applying its unique binary checksum and staging it. `CODATOR_VERIFY_ATTESTATION` accepts only `0` or `1`; the default is `0`. The bundled `gh attestation verify --bundle ...` check does not need `gh login`; see the [GitHub attestation verification documentation](https://cli.github.com/manual/gh_attestation_verify).

## Install

~~~sh
(
set -eu
umask 077
repo=https://github.com/olafurns7/codator
version=${CODATOR_VERSION:-v0.5.0}

fail() {
  printf '%s\n' "codator bootstrap: $*" >&2
  exit 1
}

tmp=$(mktemp -d "${TMPDIR:-/tmp}/codator-bootstrap.XXXXXX") || fail "cannot create temporary directory"
cleanup() {
  rm -rf "$tmp"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

if ! curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --output "$tmp/install.sh" "$repo/releases/download/$version/install.sh"; then
  fail "cannot download install.sh"
fi
[ -s "$tmp/install.sh" ] || fail "downloaded install.sh is empty"

CODATOR_VERSION="$version" sh "$tmp/install.sh"
)
~~~

This bootstrap downloads the complete versioned installer into a private temporary directory, then executes that downloaded file; it never pipes mutable `main` into a shell. It uses `v0.5.0` unless `CODATOR_VERSION` is set to another concrete release tag. Set `CODATOR_INSTALL_DIR` before the snippet to choose another destination, for example `export CODATOR_INSTALL_DIR="$HOME/bin"`. By default this installs codator in `~/.local/bin`. If that directory is not already in PATH, the installer prints the exact export to add to `~/.zshrc` or `~/.bashrc`; reload it, for example:

~~~sh
. ~/.zshrc
~~~

After installation, the installer runs `codator doctor` for each native CLI it finds, when the installed release supports it. Older releases get a short notice instead. A missing optional provider does not block installation. If a prerequisite needs attention, the binary is still installed and the installer prints the doctor result; fix it before that provider is launched. If neither native CLI is installed, it prints the native-install next step. The installer never uses `sudo` or changes shell, security, or profile settings. In opt-in mode, a missing bundle, unsupported verifier, checksum mismatch, or any attestation error fails closed before staging; checksum-only mode does not require a bundle.

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

The helper keeps native flags available and forwards them unchanged, except that `cdx mcp login SERVER` uses Codator's MCP login command so it can accept a pasted browser callback. It supports the same `--timeout` and `--scopes` options, with an optional leading `--account NAME`. Remove any old `alias cdx='codator codex'` from your shell configuration, then reload it, for example with `. ~/.zshrc`.

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

### Codex MCP OAuth login on a remote machine

For an MCP server already configured in a Codator profile, run:

~~~sh
codator mcp login posthog --account personal
~~~

The installed shortcut also supports `cdx mcp login posthog`, or `cdx --account personal mcp login posthog` for a specific profile. Both use the same callback input described below.

Open the printed authorization URL in your browser and leave this command running. If your browser is on another computer, its `127.0.0.1` callback page may fail to load. Copy that complete callback URL from the browser address bar, paste it into the waiting Codator terminal, and press Enter. Codator forwards it to the matching Codex listener on the remote machine. A browser on the same machine, or an SSH-forwarded callback, can finish without pasting anything.

The command waits up to 30 minutes; `--timeout 10m` changes that window. Closing it or pressing Ctrl+C ends the login. A callback from a stopped or expired login cannot finish a new one: start the command again and use its new authorization URL. The callback contains a one-use code, while the original native Codex process holds the PKCE verifier needed to exchange it.

Inside a Codator-launched Codex session, `codator mcp login posthog` keeps the profile identified by the inherited `CODEX_HOME`. Outside such a session, use `--account` when multiple profiles exist, or enable MCP sharing below. OAuth does not require model quota, so this command does not rotate accounts or run usage checks. It retains the profile lock, preserves the current working directory for project configuration, and lets Codex handle the token exchange and credential storage.

Scopes come from native Codex configuration and server discovery unless you explicitly pass `--scopes openid,user:read,...`. Prefer the server defaults: a custom list can omit a permission the server needs even when consent succeeds. OAuth success confirms credential storage; it does not guarantee tool access. Reconnect the server or restart the Codex session after login. A later MCP HTTP 401/403 requires checking the server's scopes, account access, and configuration.

This uses the native [Codex app-server OAuth protocol](https://learn.chatgpt.com/docs/app-server). It requires a Codex version that reports `codexHome` during initialization and supports `mcpServer/oauth/login` with `timeoutSecs` (verified with 0.155.1). The existing `codator codex --account personal mcp login posthog` remains a direct native CLI invocation with native behavior and timeout.

For an agent handling the browser step, keep `codator mcp login` alive in a persistent terminal or tool PTY. Send the returned callback URL to that same process's stdin followed by a newline. Do not start another login to submit a callback, put callback URLs in command-line arguments, or report success before the native completion result.

### Share MCPs across Codex accounts

Enable sharing once so every account selected by `cdx` can use the same user-configured MCP servers and OAuth logins:

~~~sh
codator mcp share
codator mcp login posthog
~~~

Sharing combines the base `[mcp_servers]` definitions in each enrolled Codex account and sets `mcp_oauth_credentials_store = "keyring"`. Native Codex uses the same OS keyring entry for the same server name and URL across profiles, so one successful login is available to all of them. `codator mcp login` then works without `--account`, including outside a Codex session. Logging out of that shared MCP also affects the other profiles. Already-running sessions may need to reconnect the server or restart.

Before subsequent Codator Codex launches and logins, additions, edits, and removals in any profile's base user MCP settings propagate to the others. Newly enrolled accounts inherit them. Conflicting edits to the same server are reported for resolution. If synchronization fails, for example because of a conflict, a busy or unsafe profile, or a failed native write, an ordinary Codex launch or `codator login codex` prints a warning and continues normal account selection with each profile's existing MCP settings. `codator mcp share`, `codator mcp login`, `cdx mcp login`, and native `mcp` subcommands run through Codator stop with the error instead. Interrupting Codator still stops the command. Project settings, named configuration profiles, plugin installations, ChatGPT logins, sessions, models, and hooks retain their existing scope.

Sharing is opt-in and requires a working native OS keyring. If a profile has file-based MCP credentials, the command asks you to configure and sign in with the native keyring first; it does not copy token files. Codator stores the shared definitions and synchronization snapshots in its private `codex-mcp-shared.json` file and saves each changed profile's original config as `config.toml.before-mcp-sharing`. It uses the native Codex configuration API to preserve unrelated settings. Unchanged configurations need only a local file check on launch.

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

Maintainers build the four binaries, `cdx`, `cdl`, `install.sh`, `LICENSE`, and their `SHA256SUMS` manifest locally with:

~~~sh
scripts/release.sh
~~~

The maintainer release sequence is documented in [RELEASING.md](docs/RELEASING.md). It uses local gates and builds at the exact annotated `v0.5.0` tag, uploads a draft GitHub release, then manually invokes the one-job signing workflow with the local `SHA256SUMS` hash. The workflow signs only `SHA256SUMS`, which authenticates the eight payloads through their hashes, and adds only `attestations.jsonl`; it never builds, tests, or publishes. Verify the returned manifest attestation locally before publishing the draft. Go 1.26 is the last Go series supported on macOS 12. Before Go 1.26 support ends, move macOS 12 users to a supported Go branch or raise the project's macOS minimum; do not treat an indefinitely frozen 1.26 toolchain as the update policy.

The release payloads are `codator-linux-amd64`, `codator-linux-arm64`, `codator-darwin-amd64`, `codator-darwin-arm64`, `cdx`, `cdl`, `install.sh`, `LICENSE`, and `SHA256SUMS`. The binary installer does not install shortcuts. Native macOS CI uses fake native adapters; it does not verify a Claude or Codex account flow on macOS.

See the [security policy and reporting instructions](SECURITY.md).

## License

Codator is available under the [MIT License](LICENSE).
