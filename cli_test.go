package main

import (
	"reflect"
	"testing"
)

func TestParseLaunchArgs(t *testing.T) {
	account, args, err := parseLaunchArgs([]string{"--model", "gpt-5", "--account", "work", "--", "--account", "native-value"})
	if err != nil {
		t.Fatal(err)
	}
	if account != "work" || !reflect.DeepEqual(args, []string{"--model", "gpt-5", "--account", "native-value"}) {
		t.Fatalf("account=%q args=%q", account, args)
	}
}

func TestParseInvocation(t *testing.T) {
	got, err := parseInvocation([]string{"login", "claude", "personal", "--", "auth", "login"})
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
