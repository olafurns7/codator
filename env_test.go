package main

import (
	"reflect"
	"testing"
)

func TestBuildEnvRemovesAuthAndForcesProfile(t *testing.T) {
	got := buildEnv([]string{"PATH=/bin", "TOKEN=secret", "CODEX_HOME=/wrong", "EMPTY="}, []string{"TOKEN"}, map[string]string{"CODEX_HOME": "/profile"})
	want := []string{"CODEX_HOME=/profile", "EMPTY=", "PATH=/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("env=%q want %q", got, want)
	}
}

func envMap(env []string) map[string]string {
	values := make(map[string]string, len(env))
	for _, item := range env {
		key, value, ok := cutEnv(item)
		if ok {
			values[key] = value
		}
	}
	return values
}
