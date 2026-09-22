# Codator

Codator lets you keep several [Codex](https://learn.chatgpt.com/docs/codex/cli) and [Claude Code](https://code.claude.com/docs/en/setup) subscription accounts side by side and start whichever one has the most quota left. Each account gets a local label (such as `personal` or `work`). Before a launch, Codator reads the native CLI's usage data and picks the eligible label with the most headroom. You can also choose one yourself.

It runs on Linux and macOS (amd64 and arm64). Codator is not affiliated with OpenAI or Anthropic and does not provide or alter account access.

## Quick start

**1. Install the native CLI you want to use.** You can use one or both.

~~~sh
# Codex
curl -fsSL https://chatgpt.com/codex/install.sh | sh

# Claude Code (needs macOS 13+ on a Mac)
curl -fsSL https://claude.ai/install.sh | bash
~~~

On Linux, Codex also needs `bubblewrap` for its sandbox: `sudo apt install bubblewrap`, or your distribution's equivalent.

**2. Install Codator.**

~~~sh
(
set -eu
version=${CODATOR_VERSION:-v0.5.2}
installer=$(mktemp)
trap 'rm -f "$installer"' EXIT
curl -fsSL -o "$installer" "https://github.com/olafurns7/codator/releases/download/$version/install.sh"
CODATOR_VERSION="$version" sh "$installer"
)
~~~

This installs `codator` into `~/.local/bin`, verifies its checksum, and then runs `codator doctor` for each native CLI it finds. If `~/.local/bin` is not on your `PATH`, the installer prints the line to add to `~/.zshrc` or `~/.bashrc`. To install somewhere else, set `CODATOR_INSTALL_DIR` first.

**3. Sign in to each account under a label.**

~~~sh
codator login codex personal
codator login codex work -- --device-auth   # headless or SSH machine
codator login claude personal
~~~

Everything after `--` goes to the native login command unchanged.

**4. Run it.**

~~~sh
codator codex                       # picks the Codex account with the most quota left
codator claude --account personal   # or choose a label yourself
codator status                      # show every account's remaining quota
~~~

That's all you need. The rest of this page covers optional shortcuts, MCP servers, and reference details.

## Optional shortcuts: `cdx` and `cdl`

`cdx` and `cdl` are small shell wrappers that start Codex or Claude through Codator **with approvals turned off**:

- `cdx` runs `codator codex` with `approval_policy="never"` and `sandbox_mode="danger-full-access"`.
- `cdl` runs `codator claude` with `--dangerously-skip-permissions --permission-mode bypassPermissions`.

> Only use them in repositories, tools, and plugins you trust. For the normal sandbox and approval prompts, run `codator codex` or `codator claude` directly.

Install them by running the step 2 snippet with one extra variable:

~~~sh
export CODATOR_SHORTCUTS=1
# then paste the install snippet from step 2
~~~

The installer verifies both wrappers against the release checksums and puts them next to `codator`. It replaces an existing `cdx` or `cdl` only if that file already calls Codator; any other command with the same name is left alone and reported.

~~~sh
cdx
cdl --account work --model sonnet
~~~

Both helpers accept a leading `--account NAME` and pass everything else through. If you previously defined `alias cdx=...` or `alias cdl=...` in your shell config, remove it so the alias does not shadow the helper.

## Commands

~~~text
codator login codex|claude NAME [-- native-login-options]
codator codex  [--account NAME] [native arguments]
codator claude [--account NAME] [native arguments]
codator status [codex|claude]
codator doctor [codex|claude]
codator mcp login SERVER [--account NAME] [--timeout 30m] [--scopes a,b]
codator mcp share
~~~

Codator reads only a leading `--account NAME`. All other arguments, including `--`, go to the native CLI unchanged and in the same order:

~~~sh
codator codex --account personal exec -- "explain this repository"
~~~

`doctor` is read-only. It checks that the native CLI and, on Linux, the Codex sandbox work. It never touches accounts or credentials.

## How accounts are chosen

Automatic selection skips accounts that are busy, exhausted, spend-capped, not on a subscription, or have unknown quota. `codator status` shows the reason for each account:

- **busy:** another Codator command, such as a login, holds that profile's lock.
- **unknown:** Codator could not read reliable usage data. Automatic selection skips it. An explicit `--account` still launches if the subscription is verified.
- **cooldown (Claude):** Codator is waiting before it retries a usage probe that failed.

Codator counts Claude's session and weekly limits separately from its model-specific weekly limits. If you pass a recognized `--model`, Codator also checks that model's limit. An exhausted Fable limit therefore does not block other models. Codator never sets or enforces spending caps.

## Codex MCP servers

### Logging in to an MCP server over SSH

When Codex runs on a remote machine, the OAuth redirect to `127.0.0.1` cannot reach it from your browser. `codator mcp login` handles this case:

~~~sh
codator mcp login posthog --account personal   # or: cdx mcp login posthog
~~~

1. Open the printed URL in your browser and approve.
2. If the browser then shows an error page for a `127.0.0.1` address, copy that page's full URL from the address bar.
3. Paste the URL into the waiting terminal and press Enter.

If the browser runs on the same machine, or the callback port is forwarded over SSH, the login finishes without pasting. The command waits 30 minutes; `--timeout` changes this. If it expires, run the command again and use the new URL, because old callback URLs will not work. After logging in, reconnect the MCP server or restart Codex.

`--account` is optional inside a Codator-launched Codex session, when only one profile exists, or when MCP sharing is enabled. Scopes come from the server unless you override them with `--scopes`; the server defaults are usually the right choice. It needs a recent Codex (tested with 0.155.1). `codator codex mcp login ...` still runs the plain native command.

*For agents that drive this flow:* keep the `codator mcp login` process running in a persistent PTY and write the callback URL plus a newline to its stdin. Do not start a second login to submit the URL.

### Sharing MCP servers across Codex accounts

~~~sh
codator mcp share          # once
codator mcp login posthog  # now available in every Codex profile
~~~

Sharing merges the `[mcp_servers]` entries from every Codex profile and switches all profiles to keyring-stored OAuth credentials (`mcp_oauth_credentials_store = "keyring"`). One login then works for every account, and so does one logout. Before each Codator launch, additions, edits, and removals made in any profile are copied to the others. Codator reports conflicting edits to the same server so you can resolve them. If sync fails during a normal launch, Codator prints a warning and starts with each profile's existing settings. `mcp` commands stop with the error instead.

Sharing requires a working OS keyring. Codator does not copy token files: if a profile uses file-based MCP credentials, switch it to the keyring and sign in again. Codator saves each profile's original config as `config.toml.before-mcp-sharing`. Project configs, named profiles, plugins, sessions, and models are not shared.

## Profiles and security

Each label has its own native settings, sessions, and history in `${XDG_DATA_HOME:-~/.local/share}/codator/`, created with private permissions. This separates profiles for the same Unix user; it is not OS-level isolation or encryption. Codator trusts your home directory, environment, `PATH`, the native CLIs, and their plugins. Credentials stay with the native CLIs.

Commands that change credentials hold an exclusive lock on the profile. Normal sessions release the lock once an account is selected, so several sessions can use the same profile at once.

See [SECURITY.md](SECURITY.md) for the release trust model, the optional attestation-verified install (`CODATOR_VERIFY_ATTESTATION=1`, requires `gh` 2.86+), and how to report a vulnerability.

## Troubleshooting

**`doctor codex` reports a blocked user namespace on Ubuntu 24.04.** Install the AppArmor profile for bubblewrap, as described in the [Codex sandbox prerequisites](https://learn.chatgpt.com/docs/sandboxing#prerequisites):

~~~sh
sudo apt install bubblewrap apparmor-profiles apparmor-utils
sudo install -m 0644 /usr/share/apparmor/extra-profiles/bwrap-userns-restrict /etc/apparmor.d/
sudo apparmor_parser -r /etc/apparmor.d/bwrap-userns-restrict
~~~

Do not disable the sandbox or the system-wide namespace restriction.

**Claude asks you to choose a login method again for a profile that is already signed in.** Some Claude versions show this when a profile's config has no `hasCompletedOnboarding`. Before launch, Codator adds only that field, and only when `claude auth status` confirms a logged-in Claude.ai subscription. It does not change anything else. A profile that is actually signed out still needs `codator login claude NAME`.

## Update and uninstall

To update, run the install snippet again with a newer `CODATOR_VERSION`. To uninstall, delete `~/.local/bin/codator`, plus `cdx` and `cdl` if you installed them. Neither step removes profile data in `~/.local/share/codator/`; delete that directory yourself if you want it gone.

## Building from source

Building requires Go 1.26.8 or newer. The project has no third-party modules.

~~~sh
go build -trimpath -o codator .
go test ./...
~~~

Maintainers: `scripts/release.sh` builds the four binaries, `cdx`, `cdl`, `install.sh`, `LICENSE`, and `SHA256SUMS`. [docs/RELEASING.md](docs/RELEASING.md) covers tagging, the draft release, and the signing workflow. Go 1.26 is the last Go series that supports macOS 12. Before 1.26 support ends, either raise the macOS minimum or move macOS 12 users to a supported Go branch.

## License

[MIT](LICENSE)
