package main

import (
	"errors"
	"fmt"
	"strings"
)

const usage = `Usage:
  codator login codex NAME [-- native-login-options]
  codator login claude NAME [-- native-login-options]
  codator status [codex|claude]
  codator codex [--account NAME] [native arguments]
  codator claude [--account NAME] [native arguments]
  codator --help

For launch, Codator consumes only a leading --account NAME pair. Every
remaining argument, including --, is passed unchanged to the native CLI.

Examples:
  codator login codex personal
  codator status
  codator claude --account work --model sonnet`

var errUsage = errors.New("invalid command")

type invocation struct {
	verb     string
	provider string
	account  string
	args     []string
}

func parseInvocation(args []string) (invocation, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		return invocation{verb: "help"}, nil
	}
	switch args[0] {
	case "login":
		if len(args) < 3 || !validProvider(args[1]) || !validLabel(args[2]) {
			return invocation{}, fmt.Errorf("%w: login requires a provider and valid account name", errUsage)
		}
		if len(args) == 3 {
			return invocation{verb: "login", provider: args[1], account: args[2]}, nil
		}
		if args[3] != "--" {
			return invocation{}, fmt.Errorf("%w: native login options must follow --", errUsage)
		}
		return invocation{verb: "login", provider: args[1], account: args[2], args: args[4:]}, nil
	case "status":
		if len(args) == 1 {
			return invocation{verb: "status"}, nil
		}
		if len(args) == 2 && validProvider(args[1]) {
			return invocation{verb: "status", provider: args[1]}, nil
		}
		return invocation{}, fmt.Errorf("%w: status accepts only codex or claude", errUsage)
	case "codex", "claude":
		account, nativeArgs, err := parseLaunchArgs(args[1:])
		if err != nil {
			return invocation{}, fmt.Errorf("%w: %v", errUsage, err)
		}
		return invocation{verb: "launch", provider: args[0], account: account, args: nativeArgs}, nil
	default:
		return invocation{}, fmt.Errorf("%w: unknown command %q", errUsage, args[0])
	}
}

func parseLaunchArgs(args []string) (string, []string, error) {
	var account string
	start := 0
	if len(args) > 0 && args[0] == "--account" {
		if len(args) < 2 || !validLabel(args[1]) {
			return "", nil, errors.New("--account needs one valid name")
		}
		account, start = args[1], 2
	}
	return account, args[start:], nil
}

func claudeModelHint(args []string) claudeModelFamily {
	var model claudeModelFamily
	seen := false
	for i := 0; i < len(args); {
		arg := args[i]
		if arg == "--" {
			if seen && hasClaudeModelOption(args[i+1:]) {
				return ""
			}
			return model
		}
		if !strings.HasPrefix(arg, "-") {
			if seen && hasClaudeModelOption(args[i+1:]) {
				return ""
			}
			return model
		}
		switch {
		case arg == "--model" || strings.HasPrefix(arg, "--model="):
			if seen {
				return ""
			}
			name := strings.TrimPrefix(arg, "--model=")
			if arg == "--model" {
				if i+1 == len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
					return ""
				}
				name = args[i+1]
				i++
			}
			if name == "" {
				return ""
			}
			var ok bool
			model, ok = claudeModelFamilyForName(name)
			if !ok {
				return ""
			}
			seen = true
			i++
		case arg == "--fallback-model" || strings.HasPrefix(arg, "--fallback-model=") ||
			arg == "--agent" || arg == "--agents" || arg == "--resume" || arg == "-r" ||
			arg == "--continue" || arg == "-c" || arg == "--from-pr" || arg == "--teleport":
			return ""
		case claudeValueOption(arg):
			if i+1 == len(args) || args[i+1] == "--" {
				return ""
			}
			i += 2
		case claudeAttachedValueOption(arg):
			i++
		case claudeKnownFlag(arg):
			i++
		default:
			return ""
		}
	}
	return model
}

func hasClaudeModelOption(args []string) bool {
	for _, arg := range args {
		if i := strings.IndexByte(arg, '='); i >= 0 {
			arg = arg[:i]
		}
		if arg == "--model" || arg == "--fallback-model" ||
			arg == "--agent" || arg == "--agents" || arg == "--resume" || arg == "-r" ||
			arg == "--continue" || arg == "-c" || arg == "--from-pr" || arg == "--teleport" {
			return true
		}
	}
	return false
}

func claudeValueOption(arg string) bool {
	// Keep pre-parser option-looking values intact; unrecognized and variadic options fall back.
	switch arg {
	case "--settings", "--plugin-dir", "--plugin-dir-no-mcp", "--cwd",
		"--permission-mode", "--effort", "--output-format":
		return true
	default:
		return false
	}
}

func claudeAttachedValueOption(arg string) bool {
	for _, option := range []string{"--permission-mode", "--effort", "--output-format"} {
		if strings.HasPrefix(arg, option+"=") {
			return true
		}
	}
	return false
}

func claudeKnownFlag(arg string) bool {
	switch arg {
	case "--print", "-p", "--verbose", "--safe-mode", "--no-session-persistence", "--dangerously-skip-permissions":
		return true
	default:
		return false
	}
}
