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
version=${CODATOR_VERSION:-v0.6.0}
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

That's all you need. `codator codex` and `codator claude` keep the native CLI's normal sandbox and approval prompts. The rest of this page covers optional shortcuts (which turn all of those off), MCP servers, and reference details.

## Optional shortcuts: `cdx` and `cdl`

> [!WARNING]
> **`cdx` and `cdl` turn off every safety check.** With no sandbox and no approval prompts, the agent can run any command, read or change any file your user account can reach, use your credentials and SSH keys, and reach the network without asking you first. A malicious repository, prompt injection, MCP server, or plugin gets the same access. Use them only on machines and in repositories you would trust with a shell logged in as you. If you are not sure, use `codator codex` or `codator claude`, which keep the native sandbox and approval prompts.

`cdx` and `cdl` are small shell wrappers that start Codex or Claude through Codator **with every permission bypassed**:

- `cdx` runs `codator codex` with `approval_policy="never"` and `sandbox_mode="danger-full-access"`.
- `cdl` runs `codator claude` with `--dangerously-skip-permissions --permission-mode bypassPermissions`.

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

Codator counts Claude's session and weekly limits separately from its model-specific weekly limits. It checks a recognized `--model`, or for a plain launch, each account's `ANTHROPIC_MODEL`, saved `settings.json` model, or `ANTHROPIC_DEFAULT_MODEL` in that order. With no model override, it uses a bounded local `claude --version` check to recognize the Opus default in Claude Code 2.1.280 and later 2.1.x releases. An exhausted Fable limit therefore does not block a known Opus launch. Older or unrecognized versions, unfamiliar models, conflicting settings, and ambiguous native arguments keep the conservative all-model check. Codator forwards native arguments unchanged and never sets or enforces spending caps.

Claude usage is cached on disk per account for 5 minutes after success and 15 minutes after failed or unknown probes; account locks and probe reservations prevent duplicate concurrent probes.

## Shared configuration

A setting you change under one account applies to every account, and to the plain `claude` and `codex` commands. Codator links these items in each profile to the native default folder:

- **Claude**, from `~/.claude`: `settings.json`, `CLAUDE.md`, `keybindings.json`, and the `agents`, `commands`, `output-styles`, `routines`, `rules`, `skills`, `themes`, and `workflows` folders.
- **Codex**, from `~/.codex`: `config.toml`, `AGENTS.md`, `AGENTS.override.md`, `hooks.json`, and the `prompts`, `rules`, `skills`, and `themes` folders.

Codator checks the links before each launch and login, and adds any that are missing. When a profile has its own copy of an item, Codator moves it into the shared folder if nothing is there yet. If several accounts have a copy, the most recently changed one wins. Files from a profile's folder that the shared folder lacks are moved into it, and identical files are dropped. A copy that differs is saved inside the profile as `NAME.before-sharing-TIMESTAMP`, and Codator prints its path, so no configuration is lost. A differing Codex `config.toml` is also merged: Codex adds the entries the shared file lacks, such as trusted folders, MCP servers, and hook trust, which Codex records separately for each profile. A setting present in both keeps the shared value. To keep an item separate for one account, replace its link with your own symlink. Codator leaves symlinks alone.

Codator never shares a Codex `config.toml` that sets `forced_login_method` or `forced_chatgpt_workspace_id`. Codex signs out, and revokes, any account that does not match those settings.

Credentials, sessions, history, and plugin installs stay per account. So does Claude's `.claude.json`, which holds the login along with folder-trust answers, per-project allowed tools, and user-scope MCP servers. Provider or API-key settings in a shared file, such as `apiKeyHelper` or `model_provider`, apply to every account.

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

### Sharing MCP logins across Codex accounts

~~~sh
codator mcp share          # once
codator mcp login posthog  # now signed in for every Codex account
~~~

Every account reads the same `[mcp_servers]` entries from the shared `~/.codex/config.toml`. `codator mcp share` also stores MCP OAuth credentials in the OS keyring (`mcp_oauth_credentials_store = "keyring"`), so one login works for every account, and so does one logout. A profile that keeps its own `config.toml` still has server additions, edits, and removals copied to and from the others before each launch. Codator reports conflicting edits to the same server so you can resolve them. If sync fails during a normal launch, Codator prints a warning and starts with each profile's existing settings. `mcp` commands stop with the error instead.

Sharing requires a working OS keyring. Codator does not copy token files: if a profile uses file-based MCP credentials, switch it to the keyring and sign in again. Codator saves the original config as `config.toml.before-mcp-sharing`.

## Profiles and security

Each label has its own credentials, account state, sessions, and history in `${XDG_DATA_HOME:-~/.local/share}/codator/`, created with private permissions. Configuration is shared as described in [Shared configuration](#shared-configuration). This separates profiles for the same Unix user; it is not OS-level isolation or encryption. Codator trusts your home directory, environment, `PATH`, the native CLIs, and their plugins. Credentials stay with the native CLIs.

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
