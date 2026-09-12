package main

import (
	"errors"
	"fmt"
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
