# Codator

Linux wrapper for isolated Codex and Claude Code subscription profiles. It selects the eligible account with the most percentage headroom in its tightest applicable usage window. It skips busy, non-subscription, exhausted, spend-capped, and unknown profiles.

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
alias cl='codator claude'
```

Enroll each account separately. For headless Codex sign-in, pass the native `--device-auth` option:

```sh
./codator login codex personal
./codator login codex server -- --device-auth
./codator login claude work
./codator status
./codator codex                 # auto-select with the default model
./codator claude --account work # select one profile
```

Each account gets separate native settings, sessions, and history under `${XDG_DATA_HOME:-$HOME/.local/share}/codator/`. Directories and files use private permissions, but filesystem permissions are not encryption. One Codator process can use a profile at a time; its lock lasts until the native CLI exits.

An explicit `--account` launch requires verified subscription identity. If usage alone is unavailable, Codator can run that explicitly selected profile and labels usage as unknown; accounts known to be exhausted or spend-capped remain blocked.

Claude launches and usage checks both use `--safe-mode`: native OAuth, approval defaults, and managed policy remain active, while user customizations and hooks are disabled.

Validation used mocked provider responses and empty profiles only. Authenticated sign-in, token refresh, and live quota checks have not been tested.
