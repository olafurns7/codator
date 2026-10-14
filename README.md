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
version=${CODATOR_VERSION:-v0.9.1}
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
codator status --refresh claude     # retry Claude usage despite its normal cooldown
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

## Optional shims: bare `codex` and `claude`

Set `CODATOR_SHIMS=1` to make the installer create `codator-shims/codex` and `codator-shims/claude` next to `codator` as symlinks to it. Put that directory first on `PATH`. Then a bare `codex` or `claude` selects an account like `codator codex` and `codator claude`, including when a terminal multiplexer or another tool relaunches or resumes a session with the plain native command. Codator skips its own shims when it looks for the native CLI. Every bare invocation selects an account, including one made from inside a running session. A leading `--account NAME` still pins the account.

Install them by running the step 2 snippet with one extra variable:

~~~sh
export CODATOR_SHIMS=1
# then paste the install snippet from step 2
~~~

## Commands

~~~text
codator login codex|claude NAME [-- native-login-options]
codator codex  [--account NAME] [native arguments]
codator claude [--account NAME] [native arguments]
codator status [--json] [--refresh] [codex|claude]
codator status --watch [codex|claude]
codator doctor [codex|claude]
codator mcp login SERVER [--account NAME] [--timeout 30m] [--scopes a,b]
codator mcp share
~~~

Codator reads only a leading `--account NAME`. All other arguments, including `--`, go to the native CLI unchanged and in the same order:

~~~sh
codator codex --account personal exec -- "explain this repository"
~~~

`doctor` is read-only. It checks that the native CLI and, on Linux, the Codex sandbox work. It never changes accounts or credentials.

Add `--json` to `codator status` for one versioned JSON document with UTC timestamps. The flag can appear before or after the optional provider.

Use `codator status --watch [codex|claude]` to keep the same boxed tables open. It requires terminal input/output and a nonempty `TERM` other than `dumb`; `NO_COLOR` keeps bars and dots without color. `--watch` and `--json` cannot be combined.

- `r` uses the shared refresh gate; repeated presses are discarded. The first usable view collects immediately, then waits at least 300 seconds after successful completion or 900 seconds after command-error cleanup. Scroll, resize and redraw do not probe.
- Arrows and `h`/`l` scroll the intrinsic tables vertically/horizontally; PgUp/PgDn and Home/End reach all accounts, columns and notes. A terminal smaller than 20 columns by 8 rows defers new collections until resized.
- `[`/`]` select an account; Enter opens/closes its full detail. At 132x18 or larger, table and detail share the body and Tab switches focus; smaller panes show detail in the full body. Selecting reveals the ACCOUNT column; manual scrolling stays available. Busy means profile-lock contention, not an active session.
- `s` cycles label/headroom/nearest-reset ordering within providers; `p` cycles loaded-provider filters and `f` state filters. `/` edits a case-insensitive account-name substring (128 UTF-8 bytes): Enter applies, Ctrl-G cancels, Ctrl-U clears and Backspace removes a grapheme. In name entry q/r/h/l are text; Esc/Ctrl-C still quit. Filters intersect, affect only the view and never change the unfiltered next-account estimate or collect other providers. Sort order updates on snapshots/control changes, while freshness labels keep updating between them.
- `t` opens the filtered reset timeline; `a` opens the unfiltered availability overview. Press the active view key to return to tables. In overview, `[`/`]` choose a provider/state bucket and Enter applies it, clears the name filter and returns to tables. Counts describe recorded rows; fresh launchable/headroom/reset aggregates require qualified provider/account observations. Window reset, denial-until and available-at stay distinct; passed times await fresh data and unknown timestamps stay unknown. These controls add no probes.
- `g` opens session sparklines of **used percent**; `c` opens the snapshot change feed. The active view key returns to tables. These are observations from completed polls, not live activity: intervening activity is unknown and busy means profile-lock contention. Codex points use collection-start UTC; Claude points use `ObservedAt`, so cached observations do not repeat. Spaces mark gaps and `|` starts a segment after reset changes, decreases or missing data; there is no interpolation or inferred replenishment. Filters only affect display.
- History is memory-only and ends on quit: at most 128 series with 72 slots each, 128 semantic summaries (including provider baselines), and 128 changes. Raw identity components over 256 bytes or ambiguous duplicate labels are explicitly untracked; retained display text is capped at 512 bytes. Oldest updated records are evicted deterministically. Incomplete/failed/unknown snapshots cannot prove absence; first, omitted and evicted returns establish baselines. After identity/summary omissions, addition events remain baseline-only for this run. Feed events ignore countdown/probe-time churn and unchanged quota observations. Narrow prose, headers and footers show an ellipsis when clipped; horizontal scrolling reveals full body data.
- Smaller panes keep table borders closed while interiors scroll; `…` marks clipped detail, whose full text remains reachable with horizontal scrolling. Identical displayed timestamps share their event labels and are marked `same UTC second`; differing displayed times remain separate.
- `q`, Esc or Ctrl-C quits and restores the terminal. Collection has a 120-second deadline; cancellation cleanup is bounded to five further seconds. Uncertain cleanup disables later refreshes and warns after terminal restoration; detached descendants are not guaranteed removed.

Watch retains the last successful snapshot after a command error and labels stale data. Source collection time, Claude observation age and successful completion remain separate; a completed collection does not make cached quota fresh. Next-account text is only a fresh-snapshot estimate, with Claude's all-model caveat; a later launch can choose differently. Unknown/busy rows remain unknown, and passing a reset does not invent fresh quota.

The gate covers one continuously running view. Restarts, other views/hosts and ordinary status calls can add probes; five minutes is not a vendor polling guarantee. Existing native probes may update authentication and local lock/usage-cache state. Watch adds no provider requests or account commands beyond status collection.

## How accounts are chosen

Automatic selection skips accounts that are busy, exhausted, spend-capped, not on a subscription, or have unknown quota. `codator status` draws one panel per provider with one row per account: a dot before the name, then a bar for every usage limit with the percent remaining and the time until it resets. `●` means the account can launch, `○` means it cannot (exhausted, unknown or skipped), and `◐` means it is busy. On a terminal the dot is green, yellow on credits or busy, red when exhausted and dim otherwise. A busy, unknown or skipped account with no usage to show says so in place of the bars. A Codex account whose included usage is exhausted but has a Codex credit balance stays eligible on credits; automatic selection uses it only when no other eligible account has included usage left, and status shows `on credits` and the balance. The Claude panel title says how long ago the oldest usage snapshot was taken. Status colors the output only on a terminal, and never when `NO_COLOR` is set or `TERM` is `dumb`.

Claude Code records when a login ends, and a new `codator login claude NAME` is the way to move that date. Where the date is in the profile (Linux), `codator status` shows the days left in a `LOGIN` column, `status --json` reports it as `login_expires_at`, and `codator doctor claude` prints one line per account. With 3 days or less left, status and doctor add the renewal command and a launch prints one extra line; none of this blocks a launch, changes which account is selected, or fails `doctor`. On macOS the login lives in the keychain, which Codator does not read, so the date is unknown there: status shows nothing and doctor says `login expiry unknown`.

~~~text
╭─ codex ─────────────────────────────────────────────────────────╮
│ ACCOUNT   EMAIL           WEEKLY                   CREDITS  RST │
│ ● codex1  a@example.com   ██████████ 100% 6d 21h  41366.80      │
│ ● codex2  bb@example.com  ████████▌░  85% 6d 21h   2974.40    1 │
│ ◐ codex3                  busy                                  │
╰─────────────────────────────────────────────────────────────────╯
~~~

A dim line under an account explains anything a cell cannot, such as why it is skipped:

- **busy:** another Codator command, such as a login, holds that profile's lock.
- **unknown:** Codator could not read reliable usage data. Automatic selection skips it. An explicit `--account` can still launch a previously verified subscription during a transport failure or probe cooldown, with unknown quota. Known login failures and exhausted quota remain blocked.
- **cooldown (Claude):** Codator is waiting before it retries a usage probe that failed.

Codator counts Claude's session and weekly limits separately from its model-specific weekly limits. It checks a recognized `--model`, or for a plain launch, each account's `ANTHROPIC_MODEL`, saved `settings.json` model, or `ANTHROPIC_DEFAULT_MODEL` in that order. With no model override, it uses a bounded local `claude --version` check to recognize the Opus default in Claude Code 2.1.280 and later 2.1.x releases. An exhausted Fable limit therefore does not block a known Opus launch. Older or unrecognized versions, unfamiliar models, conflicting settings, and ambiguous native arguments keep the conservative all-model check. Codator forwards native arguments unchanged and never sets or enforces spending caps.

Claude usage is cached on disk per account for 5 minutes after success. Network errors, timeouts, malformed or unknown answers, and interrupted probe reservations retry after 60 seconds. Only a provider rate-limit error (429 or equivalent) uses the 15-minute fallback; a supplied Retry-After sets the retry deadline instead. Login failures remain blocked until you run the exact `codator login claude NAME` command shown in status and launch errors, or a forced refresh verifies recovery. Codex shows `codator login codex NAME` for the same auth failures and remembers them without adding a usage cache.

Use `codator status --refresh [codex|claude]` (also with `--json`) to bypass Claude's normal cooldown or retry a remembered login failure. Forced retries have a 10-second minimum interval; profile locks and durable reservations prevent concurrent Claude probes. Codex usage probes already run live, so it needs that interval only when refreshing a remembered auth failure. Automatic selection continues to skip unknown usage. During a network outage, `codator claude --account NAME` can launch with unknown quota if that subscription was previously verified and has no known login or quota denial. Legacy failed Claude caches have their retry window shortened to 60 seconds without treating their ambiguous failures as evidence of a valid login. See [the provider contract](docs/provider-contract.md) for the failure and cache rules.

## Shared configuration

A setting you change under one account applies to every account, and to the plain `claude` and `codex` commands. Codator links these items in each profile to the native default folder:

- **Claude**, from `~/.claude`: `settings.json`, `CLAUDE.md`, `keybindings.json`, and the `agents`, `commands`, `output-styles`, `routines`, `rules`, `skills`, `themes`, and `workflows` folders.
- **Codex**, from `~/.codex`: `config.toml`, `AGENTS.md`, `AGENTS.override.md`, `hooks.json`, and the `prompts`, `rules`, `skills`, and `themes` folders. Codator also links `.tmp/bundled-marketplaces`, `plugins/cache/openai-bundled`, and `computer-use` from `~/.codex` when present, so profiles can access the bundled marketplace, its installed plugins such as computer use and browser, and the computer use client app that the Codex desktop app installed there. Codex accepts that marketplace only from the profile's own folder, but the shared `config.toml` names the copy in `~/.codex`. So when a profile has `.tmp/bundled-marketplaces/openai-bundled`, Codator starts Codex in that account with `-c marketplaces.openai-bundled.source=<profile>/.tmp/bundled-marketplaces/openai-bundled`. It does not change `config.toml` or turn any plugin on, and a `-c` you pass for the same setting wins.

Codator checks the links before each launch and login, and adds any that are missing. When you linked a folder that contains an item, such as a profile's `plugins` folder for `plugins/cache/openai-bundled`, Codator leaves that item alone for that profile, wherever the link points. When a profile has its own copy of an item, Codator moves it into the shared folder if nothing is there yet. If several accounts have a copy, the most recently changed one wins. Files from a profile's folder that the shared folder lacks are moved into it, and identical files are dropped. A copy that differs is saved inside the profile as `NAME.before-sharing-TIMESTAMP`, and Codator prints its path, so no configuration is lost. Differing settings files, such as `settings.json`, `hooks.json`, and `config.toml`, are also merged key by key. Codator adds the settings the shared file lacks, and a setting present in both keeps the shared value. Codex's own editor merges `config.toml`, which carries over trusted folders, MCP servers, and hook trust, which Codex records separately for each profile. To keep an item separate for one account, replace its link with your own symlink. Codator leaves symlinks alone.

Codator never shares a Codex `config.toml` that sets `forced_login_method` or `forced_chatgpt_workspace_id`. Codex signs out, and revokes, any account that does not match those settings.

Session transcripts are shared so any account can resume a session; Claude's per-project memory lives with the transcripts and is shared too, with each account's memory index lines added to the shared index while its original is saved beside the profile. Credentials, prompt history, and other plugin installs stay per account. So does Claude's `.claude.json`, which holds the login along with folder-trust answers, per-project allowed tools, and user-scope MCP servers. For Codex, each plugin's writable data in `plugins/data`, the plugins installed for a ChatGPT account, which Codex syncs and prunes for each account, and Codex's cache of that account's apps also stay per account. Provider or API-key settings in a shared file, such as `apiKeyHelper` or `model_provider`, apply to every account. Start a new session after sharing bundled plugin files. Availability also depends on Codex loading the plugin, the desktop app, and OS permissions.

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

Each label has its own credentials, account state, and prompt history in `${XDG_DATA_HOME:-~/.local/share}/codator/`, created with private permissions; session transcripts are shared so any account can resume a session. Configuration is shared as described in [Shared configuration](#shared-configuration). This separates profiles for the same Unix user; it is not OS-level isolation or encryption. Codator trusts your home directory, environment, `PATH`, the native CLIs, and their plugins. Credentials stay with the native CLIs.

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

To update, run the install snippet again with a newer `CODATOR_VERSION`. To uninstall, delete `~/.local/bin/codator`, plus `cdx` and `cdl` if you installed them, and `~/.local/bin/codator-shims` if you installed the shims. Neither step removes profile data in `~/.local/share/codator/`; delete that directory yourself if you want it gone.

## Building from source

Building requires Go 1.26.8 or newer. The project has no third-party modules.

~~~sh
go build -trimpath -o codator .
go test ./...
~~~

Maintainers: `scripts/release.sh` builds the four binaries, `cdx`, `cdl`, `install.sh`, `LICENSE`, and `SHA256SUMS`. [docs/RELEASING.md](docs/RELEASING.md) covers tagging, the draft release, and the signing workflow. Go 1.26 is the last Go series that supports macOS 12. Before 1.26 support ends, either raise the macOS minimum or move macOS 12 users to a supported Go branch.

## License

[MIT](LICENSE)
