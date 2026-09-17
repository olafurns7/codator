package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestCdxHelper(t *testing.T) {
	defaults := []string{"codex", "-c", `approval_policy="never"`, "-c", `sandbox_mode="danger-full-access"`}
	pinned := append([]string{"codex", "--account", "team"}, defaults[1:]...)
	tests := []struct {
		name string
		args []string
		want []string
		exit int
	}{
		{"plain launch defaults", nil, defaults, 0},
		{"leading account", []string{"--account", "team", "--model", "gpt-5.6-luna"}, append(pinned, "--model", "gpt-5.6-luna"), 0},
		{"missing account is delegated", []string{"--account"}, []string{"codex", "--account"}, 0},
		{"empty account is delegated for validation", []string{"--account", ""}, append([]string{"codex", "--account", ""}, defaults[1:]...), 0},
		{"opaque separator spaces newline and empty prompt", []string{"--", "prompt with spaces", "line one\nline two", ""}, append(defaults, "--", "prompt with spaces", "line one\nline two", ""), 0},
		{"resume is opaque", []string{"resume", "session id with spaces", "--last"}, append(defaults, "resume", "session id with spaces", "--last"), 0},
		{"explicit permissions and yolo remain native", []string{"-c", `approval_policy="on-request"`, "-c", `sandbox_mode="workspace-write"`, "--yolo"}, append(defaults, "-c", `approval_policy="on-request"`, "-c", `sandbox_mode="workspace-write"`, "--yolo"), 0},
		{"plain info -h", []string{"-h"}, []string{"codex", "-h"}, 0},
		{"plain info --help", []string{"--help"}, []string{"codex", "--help"}, 0},
		{"plain info -V", []string{"-V"}, []string{"codex", "-V"}, 0},
		{"plain info --version", []string{"--version"}, []string{"codex", "--version"}, 0},
		{"pinned info -h", []string{"--account", "team", "-h"}, []string{"codex", "--account", "team", "-h"}, 0},
		{"pinned info --help", []string{"--account", "team", "--help"}, []string{"codex", "--account", "team", "--help"}, 0},
		{"pinned info -V", []string{"--account", "team", "-V"}, []string{"codex", "--account", "team", "-V"}, 0},
		{"pinned info --version", []string{"--account", "team", "--version"}, []string{"codex", "--account", "team", "--version"}, 0},
		{"exit code propagates", nil, defaults, 37},
	}

	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	writeInstallFixture(t, filepath.Join(bin, "codator"), `#!/bin/sh
for arg in "$@"; do
    printf '%s\0' "$arg"
done
exit "${CDX_EXIT_CODE:-0}"
`)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := exec.Command("./scripts/cdx", test.args...)
			cmd.Env = buildEnv(os.Environ(), nil, map[string]string{
				"CDX_EXIT_CODE": strconv.Itoa(test.exit),
				"PATH":          bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			})
			output, err := cmd.Output()
			if test.exit == 0 {
				if err != nil {
					t.Fatalf("cdx returned error: %v", err)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != test.exit {
					t.Fatalf("cdx error = %v, want exit status %d", err, test.exit)
				}
			}
			if !strings.HasSuffix(string(output), "\x00") {
				t.Fatalf("captured argv is not NUL terminated: %q", output)
			}
			got := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("captured argv = %#v, want %#v", got, test.want)
			}
		})
	}
}
