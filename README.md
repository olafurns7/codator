# Codator

Codator is a Linux wrapper for separate [Codex](https://learn.chatgpt.com/docs/codex/cli) and [Claude Code](https://code.claude.com/docs/en/setup) profiles. Give each subscribed account a local label; Codator checks the native CLI's usage data and chooses the eligible label with the most percentage headroom. A label is only a local profile name, not an account ID.

It skips busy, unknown, exhausted, spend-capped, and non-subscription profiles. Codator is not affiliated with either provider and does not provide or alter account access.

## Requirements

Codator release binaries support Linux x86_64/amd64 and aarch64/arm64. They need no Go runtime. Install the native CLI you plan to use and ensure it is on PATH:

~~~sh
# Codex: https://learn.chatgpt.com/docs/codex/cli
curl -fsSL https://chatgpt.com/codex/install.sh | sh

# Claude Code: https://code.claude.com/docs/en/setup
curl -fsSL https://claude.ai/install.sh | bash
~~~

The Codator installer needs curl, sha256sum, and standard Linux shell utilities. It downloads only the selected GitHub release asset over HTTPS, verifies its SHA-256 checksum, and atomically replaces the binary only after verification.

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
curl -fsSL https://raw.githubusercontent.com/olafurns7/codator/main/install.sh | CODATOR_VERSION=v0.1.0 sh
~~~

## Use

Enroll each Codex and Claude account separately. The headless Codex example forwards its native --device-auth option:

~~~sh
codator login codex personal
codator login codex team -- --device-auth
codator login claude personal
codator login claude team
~~~

Launch without an account to select automatically, or choose a label explicitly:

~~~sh
codator codex
codator claude --account team --model sonnet
codator codex --account personal exec -- "explain this repository"
~~~

Codator consumes only a leading --account NAME; every other native argument is forwarded unchanged and in order, including --. Optional aliases:

~~~sh
alias cdx='codator codex'
alias cdl='codator claude'
~~~

Put those in ~/.zshrc or ~/.bashrc and reload that file to activate them.

Each label has its own native settings, sessions, and history beneath ${XDG_DATA_HOME:-$HOME/.local/share}/codator/. Files and directories use private permissions, but filesystem permissions are not encryption. Codator leaves provider credentials to their native CLIs. Codex credential-changing commands such as login and logout retain an exclusive profile lock; normal Codex sessions release it after selection, so concurrent normal Codex sessions can share a profile. Claude sessions retain the profile lock while they run.

## Status and selection

~~~sh
codator status
codator status codex
codator status claude
~~~

Claude selection treats session and shared weekly limits separately from model-specific weekly limits. An explicit recognized --model can filter that model's limit, while shared limits always apply; an omitted or ambiguous model remains conservative. In status, a depleted Fable limit therefore does not mean the all-model weekly capacity is depleted. busy means another guarded operation holds the profile lock; unknown means Codator could not safely establish usable usage data; Claude cooldown output means it is temporarily avoiding another failed or unsafe usage probe.

## Update, uninstall, and build

Run the installer again to update (or set CODATOR_VERSION to pin it). To uninstall, remove only the installed binary, for example rm ~/.local/bin/codator. Neither action removes the profile data under ${XDG_DATA_HOME:-$HOME/.local/share}/codator/.

Build from source with Go 1.24 or newer:

~~~sh
go build -trimpath -o codator .
~~~

Maintainers build the two release assets and their SHA256SUMS manifest with:

~~~sh
scripts/release.sh
~~~

Pushing a v* tag runs checks, builds those assets, and publishes the GitHub release. Do not publish a tag until the release checks have passed.
