package main

import (
	"reflect"
	"testing"
)

func TestInvocationArgs(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want []string
	}{
		{name: "codator command", argv: []string{"/x/codator", "status"}, want: []string{"status"}},
		{name: "claude shim", argv: []string{"/x/shims/claude", "--resume", "abc"}, want: []string{"claude", "--resume", "abc"}},
		{name: "bare codex", argv: []string{"codex"}, want: []string{"codex"}},
		{name: "codator without args", argv: []string{"/x/codator"}, want: []string{}},
		{name: "no args", argv: nil, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := invocationArgs(test.argv); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("invocationArgs(%q) = %q, want %q", test.argv, got, test.want)
			}
		})
	}
}
