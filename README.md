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
./codator claude --account work --model claude-sonnet-5
```

Each account gets separate native settings, sessions, and history under `${XDG_DATA_HOME:-$HOME/.local/share}/codator/`. Directories and files use private permissions, but filesystem permissions are not encryption. One Codator process can use a profile at a time; its lock lasts until the native CLI exits.

Claude quota checks cache sanitized per-profile observations for up to five minutes; a completed subscription classification lasts through its original five- or fifteen-minute cooldown. Native or external activity may change either observation sooner, and unknown or failed probes suppress retries for fifteen minutes.

Codex uses its native default file credential store at `$CODEX_HOME/auth.json`; each account gets its own `CODEX_HOME`. Codator does not write a native config file. It sets `CODEX_SQLITE_HOME` to the same account directory and preserves native settings. Explicit native config, including `-c`/`--config` or a configured `sqlite_home`, remains authoritative and can redirect state, provider, or authentication. Credentials remain stored and managed by Codex. Codator filters inherited auth and endpoint variables before native launches and quota probes.

On launch, Codator removes only a leading `--account NAME` pair. Every other argument passes unchanged and in order, including every `--`. For example, `cdx -- '-m'` reaches Codex as `-- -m`. The separately documented `codator login ... -- ...` command keeps its own delimiter.

Codex global help/version flags are `-h`, `--help`, `-V`, and `--version`; Claude global flags are `-h`, `--help`, `-v`, and `--version`. These verified native flags run without selecting a profile. Bare words `help` and `version` go through normal profile selection and are forwarded to the native CLI.

An explicit `--account` launch requires verified subscription identity. For Codex, that check requires `account.type` to be `chatgpt` and `planType` to match a recognized paid plan; this is an identity filter, not a billing judgment. If usage alone is unavailable, Codator can run that explicitly selected profile; accounts known to be exhausted or spend-capped remain blocked. Native subcommands act on the selected profile too: `codator codex --account NAME logout` runs Codex logout for that profile, subject to the same eligibility checks.

Claude launch selection reads only an unambiguous pre-prompt `--model VALUE` or `--model=VALUE` hint. It recognizes the `fable`, `opus`, `sonnet`, and `haiku` aliases plus the exact IDs `claude-fable-5-1`, `claude-opus-5`, and `claude-sonnet-5`. Aliases can resolve to different versions by provider; use an exact ID when the version matters. Codator filters only exact matching `Fable`, `Opus`, `Sonnet`, or `Haiku` quota buckets. Shared and account-wide limits always apply; unknown bucket labels remain in the score. Missing hints, unknown models, repeated or mixed model choices, fallback/agent/resume modes, or option syntax Codator cannot safely observe retain conservative all-model scoring. Status remains model-free. Codator does not resolve settings, profiles, providers, or authentication. The Codex probe scores the tightest window across all `rateLimitsByLimitId` buckets when present, falling back to the legacy single `rateLimits` snapshot only when the map is absent. Spend status comes from the primary `codex` bucket; any reported true cap blocks selection, while null spend fields on additional buckets do not override that primary status. A missing or null primary status remains unknown. Native arguments remain unchanged and may change the effective provider, authentication, model, or state after selection, so the quota result ranks profiles; it does not guarantee the native invocation's provider or usage.

Background Claude quota probes use `--safe-mode`, set `DO_NOT_TRACK=1`, and disable auto-updates, error reporting, bug commands, and telemetry; ordinary launches receive the native argument vector unchanged. Native login and launch flags are validated by their CLI.

Live validation covered three Codex profiles and two Claude Max profiles; tiny `gpt-5.6-luna`, Sonnet 5, and Opus 5 prompts succeeded. No Fable inference, expired-token refresh, or distinct-account identity proof was tested.
