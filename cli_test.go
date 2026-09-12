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
}
