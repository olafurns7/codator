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
  codator codex [--account NAME] [--] [native arguments]
  codator claude [--account NAME] [--] [native arguments]
  codator --help

Examples:
  codator login codex personal
  codator status
  codator claude --account work -- --model sonnet`

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
	var native []string
	seenAccount, afterSeparator := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !afterSeparator && arg == "--" {
			afterSeparator = true
			continue
		}
		if !afterSeparator && arg == "--account" {
			if seenAccount || i+1 == len(args) || !validLabel(args[i+1]) {
				return "", nil, errors.New("--account needs one valid name")
			}
			seenAccount = true
			account = args[i+1]
			i++
			continue
		}
		native = append(native, arg)
	}
	return account, native, nil
}

func modelFromArgs(args []string, short, long string) (string, bool, error) {
	model, explicit := "", false
	flags := []string{long}
	if short != "" {
		flags = append(flags, short)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		for _, flag := range flags {
			switch {
			case arg == flag:
				if i+1 == len(args) || args[i+1] == "--" || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
					return "", false, fmt.Errorf("%s needs a model name", flag)
				}
				model, explicit = args[i+1], true
				i++
			case strings.HasPrefix(arg, flag+"="):
				model, explicit = strings.TrimPrefix(arg, flag+"="), true
				if model == "" {
					return "", false, fmt.Errorf("%s needs a model name", flag)
				}
			case flag == short && short != "" && len(arg) > len(flag) && strings.HasPrefix(arg, flag) && arg[len(flag)] != '=':
				model, explicit = arg[len(flag):], true
			}
		}
	}
	return model, explicit, nil
}

func rejectOptions(args []string, forbidden map[string]bool) error {
	for _, arg := range args {
		if arg == "--" {
			break
		}
		flag := arg
		if i := strings.IndexByte(flag, '='); i >= 0 {
			flag = flag[:i]
		}
		if forbidden[flag] || compactForbiddenShort(arg, forbidden) {
			return fmt.Errorf("native option %s is disabled by account isolation", flag)
		}
	}
	return nil
}

func compactForbiddenShort(arg string, forbidden map[string]bool) bool {
	if len(arg) < 3 || arg[0] != '-' || arg[1] == '-' {
		return false
	}
	for flag := range forbidden {
		if len(flag) == 2 && strings.HasPrefix(arg, flag) {
			return true
		}
	}
	return false
}
