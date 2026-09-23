package main

import (
	"reflect"
	"testing"
)

func TestParseLaunchArgsConsumesOnlyPrefixOptions(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		account string
		args    []string
	}{
		{
			name:  "leading native separator without pinned account",
			input: []string{"--", "-m"},
			args:  []string{"--", "-m"},
		},
		{
			name:    "leading native separator with pinned account",
			input:   []string{"--account", "work", "--", "-m"},
			account: "work",
			args:    []string{"--", "-m"},
		},
		{
			name:    "account and all following arguments stay native",
			input:   []string{"--account", "work", "exec", "--account", "native-value", "--", "prompt with spaces"},
			account: "work",
			args:    []string{"exec", "--account", "native-value", "--", "prompt with spaces"},
		},
		{
			name:  "native account option after native separator",
			input: []string{"--", "--account", "native-value", "--", "prompt"},
			args:  []string{"--", "--account", "native-value", "--", "prompt"},
		},
		{
			name:  "account text in native arguments",
			input: []string{"--model", "--account", "native-value", "--", "prompt"},
			args:  []string{"--model", "--account", "native-value", "--", "prompt"},
		},
		{
			name:  "late wrapper-looking option stays native",
			input: []string{"exec", "--account", "work", "--", "prompt"},
			args:  []string{"exec", "--account", "work", "--", "prompt"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			account, args, err := parseLaunchArgs(test.input)
			if err != nil {
				t.Fatal(err)
			}
			if account != test.account || !reflect.DeepEqual(args, test.args) {
				t.Fatalf("account=%q args=%q", account, args)
			}
		})
	}
	if _, _, err := parseLaunchArgs([]string{"--account", "../unsafe"}); err == nil {
		t.Fatal("accepted an invalid Codator account name in the prefix")
	}
}

func TestParseInvocation(t *testing.T) {
	got, err := parseInvocation([]string{"codex", "--", "-m"})
	if err != nil || got.verb != "launch" || got.provider != "codex" || !reflect.DeepEqual(got.args, []string{"--", "-m"}) {
		t.Fatalf("native leading separator invocation=%+v err=%v", got, err)
	}
	got, err = parseInvocation([]string{"login", "claude", "personal", "--", "auth", "login"})
	if err != nil || got.verb != "login" || got.provider != "claude" || got.account != "personal" || !reflect.DeepEqual(got.args, []string{"auth", "login"}) {
		t.Fatalf("invocation=%+v err=%v", got, err)
	}
	if _, err := parseInvocation([]string{"login", "codex", "../unsafe"}); err == nil {
		t.Fatal("accepted invalid login label")
	}
	if _, err := parseInvocation([]string{"login", "codex", "personal", "--with-api-key"}); err == nil {
		t.Fatal("accepted native login options without separator")
	}
	if got, err := parseInvocation([]string{"doctor", "claude"}); err != nil || got.verb != "doctor" || got.provider != "claude" {
		t.Fatalf("doctor invocation=%+v err=%v", got, err)
	}
	if _, err := parseInvocation([]string{"doctor", "both"}); err == nil {
		t.Fatal("accepted an invalid doctor provider")
	}
}

func TestClaudeModelHintUsesOnlyUnambiguousNativeModelFlags(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want claudeModelFamily
	}{
		{"Sonnet alias", []string{"--model", "sonnet"}, claudeModelSonnet},
		{"Opus alias", []string{"--model", "opus"}, claudeModelOpus},
		{"Opus 1m alias", []string{"--model", "opus[1m]"}, claudeModelOpus},
		{"Sonnet 1m alias", []string{"--model", "sonnet[1m]"}, claudeModelSonnet},
		{"Fable alias", []string{"--model", "fable"}, claudeModelFable},
		{"Haiku alias", []string{"--model", "haiku"}, claudeModelHaiku},
		{"current Opus full ID", []string{"--model", "claude-opus-5"}, claudeModelOpus},
		{"current Opus 5.5 full ID", []string{"--model", "claude-opus-5-5"}, claudeModelOpus},
		{"current Sonnet full ID", []string{"--model", "claude-sonnet-5"}, claudeModelSonnet},
		{"current Fable full ID", []string{"--model", "claude-fable-5-1"}, claudeModelFable},
		{"older Fable ID is conservative", []string{"--model", "claude-fable-5"}, ""},
		{"other model version is conservative", []string{"--model", "claude-sonnet-4-5"}, ""},
		{"equals model option", []string{"--model=sonnet"}, claudeModelSonnet},
		{"settings value is not a model hint", []string{"--settings", `{"model":"fable"}`, "--model", "sonnet"}, claudeModelSonnet},
		{"option-looking settings value is skipped whole", []string{"--settings", "--model", "--model", "sonnet"}, claudeModelSonnet},
		{"plugin directory value is skipped whole", []string{"--plugin-dir-no-mcp", "--model=fable", "--model", "sonnet"}, claudeModelSonnet},
		{"cwd option value is skipped whole", []string{"--cwd", "--model", "--model=fable"}, claudeModelFable},
		{"stops at delimiter", []string{"--model", "sonnet", "--", "prompt", "-m", "fable"}, claudeModelSonnet},
		{"delimiter tail with model option is ambiguous", []string{"--model", "sonnet", "--", "--model=fable"}, ""},
		{"prompt tail attached agent override clears hint", []string{"--model", "sonnet", "prompt", "--agent=fable-agent"}, ""},
		{"delimiter tail attached agent override clears hint", []string{"--model", "sonnet", "--", "--agent=fable-agent"}, ""},
		{"prompt tail attached fallback override clears hint", []string{"--model", "sonnet", "prompt", "--fallback-model=claude-opus-5"}, ""},
		{"delimiter tail attached resume override clears hint", []string{"--model", "sonnet", "--", "--resume=session-id"}, ""},
		{"prompt tail attached from-pr override clears hint", []string{"--model", "sonnet", "prompt", "--from-pr=123"}, ""},
		{"delimiter tail attached teleport override clears hint", []string{"--model", "sonnet", "--", "--teleport=session-id"}, ""},
		{"prompt model text is not parsed", []string{"-p", "say --model fable"}, ""},
		{"prompt after a model with later model-looking tokens is ambiguous", []string{"--model", "sonnet", "prompt", "--model", "fable"}, ""},
		{"prompt after a model with equals model tail is ambiguous", []string{"--model", "sonnet", "prompt", "--model=fable"}, ""},
		{"model after a positional is implicit to this observer", []string{"exec", "--model", "sonnet"}, ""},
		{"common model-neutral flags use both value forms", []string{"--dangerously-skip-permissions", "--permission-mode", "bypassPermissions", "--effort=high", "--output-format", "json", "--model", "claude-sonnet-5", "-p", "prompt"}, claudeModelSonnet},
		{"common attached values do not consume following options", []string{"--permission-mode=bypassPermissions", "--effort=high", "--output-format=json", "--model", "claude-sonnet-5"}, claudeModelSonnet},
		{"repeated model values are conservative", []string{"--model", "sonnet", "--model", "fable"}, ""},
		{"fallback model is mixed", []string{"--model", "sonnet", "--fallback-model", "fable"}, ""},
		{"agent override is conservative", []string{"--model", "sonnet", "--agent", "fable-agent"}, ""},
		{"agents override is conservative", []string{"--model", "sonnet", "--agents", `{"agent":{}}`}, ""},
		{"resume mode is conservative", []string{"--model", "sonnet", "--resume"}, ""},
		{"variadic option is conservative", []string{"--mcp-config", "servers.json", "--model", "sonnet"}, ""},
		{"unknown model is conservative", []string{"--model", "future-model"}, ""},
		{"default alias is conservative", []string{"--model", "default"}, ""},
		{"opusplan alias is conservative", []string{"--model", "opusplan"}, ""},
		{"unknown option grammar is conservative", []string{"--future-option", "value", "--model", "sonnet"}, ""},
		{"unverified short spelling is not inferred", []string{"-m", "sonnet"}, ""},
		{"missing model value is conservative", []string{"--model"}, ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := claudeModelHint(test.args); got != test.want {
				t.Fatalf("claudeModelHint(%q) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

func TestClaudeImplicitModelOnlyWithPlainNativeArgs(t *testing.T) {
	for _, test := range []struct {
		args []string
		want claudeModelFamily
	}{
		{nil, claudeModelOpus},
		{[]string{"--dangerously-skip-permissions", "--permission-mode", "bypassPermissions"}, claudeModelOpus},
		{[]string{"--model", "fable"}, claudeModelFable},
		{[]string{"--model", "future"}, ""},
		{[]string{"--model", "fable", "--model", "opus"}, ""},
		{[]string{"--settings", `{}`}, ""},
		{[]string{"--cwd", "/tmp"}, ""},
		{[]string{"--setting-sources", "user"}, ""},
		{[]string{"--plugin-dir", "/tmp/plugin"}, ""},
		{[]string{"--fallback-model", "fable"}, ""},
		{[]string{"--agents", `{}`}, ""},
		{[]string{"--resume", "abc"}, ""},
		{[]string{"--continue"}, ""},
		{[]string{"resume"}, ""},
		{[]string{"--future-flag"}, ""},
		{[]string{"prompt", "--model", "fable"}, ""},
		{[]string{"--", "prompt", "--model=fable"}, ""},
	} {
		if got := claudeModelHintWithFallback(test.args, claudeModelOpus); got != test.want {
			t.Errorf("args %q: got %q, want %q", test.args, got, test.want)
		}
	}
}
