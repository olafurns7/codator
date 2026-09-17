# Codator

Codator is a Linux and macOS wrapper for separate [Codex](https://learn.chatgpt.com/docs/codex/cli) and [Claude Code](https://code.claude.com/docs/en/setup) profiles. Give each subscribed account a local label; Codator checks the native CLI's usage data and chooses the eligible label with the most percentage headroom. A label is only a local profile name, not an account ID.

It skips busy, unknown, exhausted, spend-capped, and non-subscription profiles. Codator is not affiliated with either provider and does not provide or alter account access.

## Requirements

Codator release binaries support Linux x86_64/amd64 and aarch64/arm64, plus macOS 12 or later on Intel (amd64) and Apple Silicon (arm64). They need no Go runtime. Install the native CLI you plan to use and ensure it is on PATH. Native CLI requirements still apply: current Claude Code requires macOS 13 or later; Codator's macOS 12 support does not make Claude Code available on macOS 12.

~~~sh
# Codex: https://learn.chatgpt.com/docs/codex/cli
curl -fsSL https://chatgpt.com/codex/install.sh | sh

# Claude Code: https://code.claude.com/docs/en/setup
curl -fsSL https://claude.ai/install.sh | bash
~~~

The Codator installer needs curl, standard POSIX shell utilities, and `sha256sum` (Linux) or `shasum -a 256` (macOS). It downloads only the selected GitHub release asset over HTTPS, verifies its SHA-256 checksum, and atomically replaces the binary only after verification.

## Install

~~~sh
curl -fsSL https://raw.githubusercontent.com/olafurns7/codator/main/install.sh | sh
~~~

By default this installs codator in ~/.local/bin. If that directory is not already in PATH, the installer prints the exact export to add to ~/.zshrc or ~/.bashrc; reload it, for example:

~~~sh
. ~/.zshrc
~~~

To choose a directory or pin a release:

~~~sh
curl -fsSL https://raw.githubusercontent.com/olafurns7/codator/main/install.sh | CODATOR_INSTALL_DIR="$HOME/bin" sh
curl -fsSL https://raw.githubusercontent.com/olafurns7/codator/main/install.sh | CODATOR_VERSION=v0.3.0 sh
~~~

After a verified install, the installer runs `codator doctor` for each native CLI it finds, when the installed release supports it. Older releases get a short notice instead. A missing optional provider does not block installation. If a prerequisite needs attention, the binary is still installed and the installer prints the doctor result; fix it before that provider is launched. If neither native CLI is installed, it prints the native-install next step. The installer never uses `sudo` or changes shell, security, or profile settings.

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

~~~sh
install -m 0755 scripts/cdx "$HOME/.local/bin/cdx"
unalias cdx 2>/dev/null || true
cdx
cdx --account personal --model gpt-5.6-luna
~~~

The helper keeps native flags available and forwards them unchanged. Remove any old `alias cdx='codator codex'` from your shell configuration, then reload it, for example with `. ~/.zshrc`.

For a `cdl` shortcut that starts Claude with `--dangerously-skip-permissions --permission-mode bypassPermissions`, install the helper from a source checkout:

~~~sh
install -m 0755 scripts/cdl "$HOME/.local/bin/cdl"
unalias cdl 2>/dev/null || true
cdl
cdl --account personal --model sonnet
~~~

Remove any old `alias cdl='codator claude'` from your shell configuration so it does not override the helper, then reload that file. The helper preserves leading `--account NAME` selection, native arguments, and the account-free help/version commands. Bypass mode skips Claude's permission checks; it is an explicit choice made by this shortcut.

Each label has its own native settings, sessions, and history beneath ${XDG_DATA_HOME:-$HOME/.local/share}/codator/. Files and directories use private permissions, but filesystem permissions are not encryption. Codator leaves provider credentials to their native CLIs. Codex credential-changing commands such as login and logout retain an exclusive profile lock; normal Codex sessions release it after selection, so concurrent normal Codex sessions can share a profile. Claude sessions retain the profile lock while they run.

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

Claude selection treats session and shared weekly limits separately from model-specific weekly limits. An explicit recognized --model can filter that model's limit, while shared limits always apply; an omitted or ambiguous model remains conservative. In status, a depleted Fable limit therefore does not mean the all-model weekly capacity is depleted. busy means another guarded operation holds the profile lock; unknown means Codator could not safely establish usable usage data; Claude cooldown output means it is temporarily avoiding another failed or unsafe usage probe.

## Update, uninstall, and build

Run the installer again to update (or set CODATOR_VERSION to pin it). To uninstall, remove only the installed binary, for example rm ~/.local/bin/codator. Neither action removes the profile data under ${XDG_DATA_HOME:-$HOME/.local/share}/codator/.

Build from source with Go 1.25 or newer:

~~~sh
go build -trimpath -o codator .
~~~

Maintainers build four release assets and their SHA256SUMS manifest with:

~~~sh
scripts/release.sh
~~~

Pushing a v* tag runs checks, builds those assets, and publishes the GitHub release. Do not publish a tag until the release checks have passed.

The assets are `codator-linux-amd64`, `codator-linux-arm64`, `codator-darwin-amd64`, and `codator-darwin-arm64`. Releases also include optional `cdx` and `cdl` helper downloads; install those manually only if wanted. The binary installer does not install shortcuts. Native macOS CI uses fake native adapters; it does not verify a Claude or Codex account flow on macOS.

## License

Codator is available under the [MIT License](LICENSE).
