# Codator

Linux wrapper for isolated Codex and Claude Code profiles. It selects an eligible profile with the most percentage headroom in the usage data returned by each native CLI. It skips busy, non-subscription, exhausted, spend-capped, and unknown profiles.

Requires the native `codex` and `claude` CLIs in `PATH`; verified versions are Codex `0.154.0` and Claude Code `2.1.269`. Claude's `get_usage` control request is experimental and may change in future releases.

Build a standalone binary (no Go runtime needed to run it):

```sh
go build -trimpath -o codator .
mkdir -p ~/.local/bin
install -m 0755 codator ~/.local/bin/codator
```

Run `./codator` from this directory, or `codator` after installing it on `PATH`. Optional shell aliases:

```sh
alias cx='codator codex'
alias cdx='codator codex'
alias cl='codator claude'
```

Enroll each account separately. For headless Codex sign-in, pass the native `--device-auth` option:

```sh
./codator login codex personal
./codator login codex server -- --device-auth
./codator login claude work
./codator status
./codator codex                 # auto-select with the default model
./codator claude --account work --model sonnet
```

Each account gets separate native settings, sessions, and history under `${XDG_DATA_HOME:-$HOME/.local/share}/codator/`. Directories and files use private permissions, but filesystem permissions are not encryption. One Codator process can use a profile at a time; its lock lasts until the native CLI exits.

Codex uses its native default file credential store at `$CODEX_HOME/auth.json`; each account gets its own `CODEX_HOME`. Codator does not write a native config file. It sets `CODEX_SQLITE_HOME` to the same account directory and preserves native settings. Explicit native config, including `-c`/`--config` or a configured `sqlite_home`, remains authoritative and can redirect state, provider, or authentication. Credentials remain stored and managed by Codex. Codator filters inherited auth and endpoint variables before native launches and quota probes.

On launch, Codator removes only a leading `--account NAME` pair. Every other argument passes unchanged and in order, including every `--`. For example, `cdx -- '-m'` reaches Codex as `-- -m`. The separately documented `codator login ... -- ...` command keeps its own delimiter.

Codex global help/version flags are `-h`, `--help`, `-V`, and `--version`; Claude global flags are `-h`, `--help`, `-v`, and `--version`. These verified native flags run without selecting a profile. Bare words `help` and `version` go through normal profile selection and are forwarded to the native CLI.

An explicit `--account` launch requires verified subscription identity. For Codex, that check requires `account.type` to be `chatgpt` and `planType` to match a recognized paid plan; this is an identity filter, not a billing judgment. If usage alone is unavailable, Codator can run that explicitly selected profile; accounts known to be exhausted or spend-capped remain blocked. Native subcommands act on the selected profile too: `codator codex --account NAME logout` runs Codex logout for that profile, subject to the same eligibility checks.

Codator does not parse native model, config, profile, provider, or auth flags for quota selection. The Codex probe scores the tightest window across all `rateLimitsByLimitId` buckets when present, falling back to the legacy single `rateLimits` snapshot only when the map is absent. Spend status comes from the primary `codex` bucket; any reported true cap blocks selection, while null spend fields on additional buckets do not override that primary status. A missing or null primary status remains unknown. Claude's probe scores every returned model-scoped window. This conservative ranking can skip a profile because a different model's bucket is exhausted. Native settings and CLI options pass through unchanged and may change the effective provider, authentication, model, or state after selection, so the quota result ranks profiles; it does not guarantee the native invocation's provider or usage.

Only background Claude quota probes use `--safe-mode`; ordinary launches receive the native argument vector unchanged. Native login and launch flags are validated by their CLI.

Validation used mocked provider responses and empty profiles only. Authenticated sign-in, token refresh, and live quota checks have not been tested.
