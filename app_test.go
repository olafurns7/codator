package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodatorDispatchChild(t *testing.T) {
	if os.Getenv("CODATOR_DISPATCH_CHILD") == "1" {
		os.Exit(run([]string{"codex", "--", "--help"}))
	}
}

func TestCodatorPassthroughChild(t *testing.T) {
	if os.Getenv("CODATOR_PASSTHROUGH_CHILD") != "1" {
		return
	}
	var args []string
	if err := json.Unmarshal([]byte(os.Getenv("CODATOR_TEST_ARGS")), &args); err != nil {
		os.Exit(97)
	}
	os.Exit(run(args))
}

func TestNativeInfoArgsOnlyUsesProviderFlags(t *testing.T) {
	for provider, flags := range map[string][]string{
		"codex":  {"-h", "--help", "-V", "--version"},
		"claude": {"-h", "--help", "-v", "--version"},
	} {
		for _, flag := range flags {
			if !nativeInfoArgs(provider, []string{flag}) {
				t.Errorf("%s did not recognize native info flag %q", provider, flag)
			}
		}
		for _, word := range []string{"help", "version"} {
			if nativeInfoArgs(provider, []string{word}) {
				t.Errorf("%s bare word %q bypasses account selection", provider, word)
			}
		}
	}
}

func TestLaunchBestSelectsAccountAndPreservesContext(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"low", "high"} {
		if _, err := store.EnsureAccount("codex", name); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	launchFile := filepath.Join(root, "selected")
	argsFile := filepath.Join(root, "args")
	cwdFile := filepath.Join(root, "cwd")
	releaseFile := filepath.Join(root, "release")
	stub := filepath.Join(binDir, "codex")
	script := `#!/bin/sh
profile=${CODEX_HOME%/native}
profile=${profile##*/}
case "$*" in
  *app-server*)
    while IFS= read -r line; do
      case "$line" in
        *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
        *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","planType":"plus"}}}' ;;
        *'"method":"account/rateLimits/read"'*)
          if [ "$profile" = high ]; then primary=10; secondary=20; else primary=50; secondary=60; fi
          printf '%s\n' "{\"id\":3,\"result\":{\"ordinaryUsageAllowed\":true,\"rateLimitsByLimitId\":{\"codex\":{\"limitId\":\"codex\",\"primary\":{\"usedPercent\":$primary,\"windowDurationMins\":300,\"resetsAt\":4102444800},\"secondary\":{\"usedPercent\":$secondary,\"windowDurationMins\":10080,\"resetsAt\":4102444800},\"spendControlReached\":false}}}}"
          ;;
      esac
    done
    ;;
  *)
    printf '%s\n' "$profile" > "$CODATOR_LAUNCH_FILE"
    printf '%s\n' "$@" > "$CODATOR_LAUNCH_ARGS_FILE"
    pwd -P > "$CODATOR_LAUNCH_CWD_FILE"
    while [ ! -f "$CODATOR_RELEASE_FILE" ]; do :; done
    exit 23
    ;;
esac
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	launchDir := filepath.Join(root, "work")
	if err := os.Mkdir(launchDir, 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCodatorDispatchChild$")
	cmd.Dir = launchDir
	cmd.Env = buildEnv(os.Environ(), nil, map[string]string{
		"CODATOR_DISPATCH_CHILD":   "1",
		"CODATOR_LAUNCH_FILE":      launchFile,
		"CODATOR_LAUNCH_ARGS_FILE": argsFile,
		"CODATOR_LAUNCH_CWD_FILE":  cwdFile,
		"CODATOR_RELEASE_FILE":     releaseFile,
		"PATH":                     binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_DATA_HOME":            dataHome,
	})
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = os.WriteFile(releaseFile, nil, 0600)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitForFile(t, launchFile, cmd)

	selected, err := os.ReadFile(launchFile)
	if err != nil || strings.TrimSpace(string(selected)) != "high" {
		t.Fatalf("selected profile = %q, err %v", selected, err)
	}
	args, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(args), "--help") {
		t.Fatalf("native arguments = %q, err %v", args, err)
	}
	cwd, err := os.ReadFile(cwdFile)
	if err != nil || strings.TrimSpace(string(cwd)) != launchDir {
		t.Fatalf("native cwd = %q, err %v", cwd, err)
	}
	if _, err := store.Lock("codex", "high"); !errors.Is(err, ErrAccountBusy) {
		t.Fatalf("selected account lock error = %v, want busy", err)
	}
	loserLock, err := store.Lock("codex", "low")
	if err != nil {
		t.Fatalf("unselected account lock was retained: %v", err)
	}
	loserLock.Close()
	if err := os.WriteFile(releaseFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("native exit status was lost")
	} else if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
		t.Fatalf("native exit status = %v, want 23", err)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "codator: using codex account high") {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	selectedLock, err := store.Lock("codex", "high")
	if err != nil {
		t.Fatalf("selected account lock was not released: %v", err)
	}
	selectedLock.Close()
}

func TestLaunchPassesNativeArgsOpaque(t *testing.T) {
	tests := []struct {
		name       string
		provider   string
		invocation []string
		nativeArgs []string
	}{
		{
			name:     "Codex exec options, native account text, and separator",
			provider: "codex",
			invocation: []string{"codex", "--account", "work", "exec", "-m", "gpt-5.6-luna",
				"-c", "model_reasoning_effort=\"max\"", "-c", "service_tier=\"priority\"",
				"-p", "work profile", "--account", "native-value", "--", "prompt with spaces"},
			nativeArgs: []string{"exec", "-m", "gpt-5.6-luna",
				"-c", "model_reasoning_effort=\"max\"", "-c", "service_tier=\"priority\"",
				"-p", "work profile", "--account", "native-value", "--", "prompt with spaces"},
		},
		{
			name:     "Codex resume",
			provider: "codex",
			invocation: []string{"codex", "--account", "work", "resume", "session-id",
				"-m", "gpt-5.6-luna", "-p", "profile with spaces", "--", "continue this session"},
			nativeArgs: []string{"resume", "session-id", "-m", "gpt-5.6-luna", "-p",
				"profile with spaces", "--", "continue this session"},
		},
		{
			name:       "cdx leading native separator",
			provider:   "codex",
			invocation: []string{"codex", "--", "-m"},
			nativeArgs: []string{"--", "-m"},
		},
		{
			name:     "Claude settings MCP plugin and separator",
			provider: "claude",
			invocation: []string{"claude", "--account", "work", "--settings", "{\"model\":\"opus\"}",
				"--mcp-config", "/tmp/mcp config.json", "--plugin-dir", "/tmp/plugin dir",
				"--", "prompt with spaces and --account native"},
			nativeArgs: []string{"--settings", "{\"model\":\"opus\"}", "--mcp-config",
				"/tmp/mcp config.json", "--plugin-dir", "/tmp/plugin dir", "--",
				"prompt with spaces and --account native"},
		},
		{
			name:       "Codex bare version word remains a native command",
			provider:   "codex",
			invocation: []string{"codex", "--account", "work", "version"},
			nativeArgs: []string{"version"},
		},
		{
			name:       "Claude bare help word remains a native command",
			provider:   "claude",
			invocation: []string{"claude", "--account", "work", "help"},
			nativeArgs: []string{"help"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataHome := tempDataHome(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.EnsureAccount(test.provider, "work"); err != nil {
				t.Fatal(err)
			}

			root := t.TempDir()
			binDir := filepath.Join(root, "bin")
			if err := os.Mkdir(binDir, 0700); err != nil {
				t.Fatal(err)
			}
			captureFile := filepath.Join(root, "native-argv")
			var script string
			if test.provider == "codex" {
				script = `#!/bin/sh
probe=0
for arg in "$@"; do
  if [ "$arg" = app-server ]; then probe=1; fi
done
if [ "$probe" = 1 ]; then
  while IFS= read -r line; do
    case "$line" in
      *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
      *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","planType":"plus"}}}' ;;
      *'"method":"account/rateLimits/read"'*) printf '%s\n' '{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false}}}}' ;;
    esac
  done
  exit 0
fi
printf '%s\0' "$@" > "$CODATOR_CAPTURE_FILE"
exit 23
`
			} else {
				script = `#!/bin/sh
if [ "$1" = --print ]; then
  while IFS= read -r line; do
    case "$line" in
      *'"request_id":"init-1"'*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}' ;;
      *'"request_id":"usage-1"'*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"usage-1","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":20,"resets_at":"2099-01-01T00:00:00Z"}}}}}' ;;
    esac
  done
  exit 0
fi
printf '%s\0' "$@" > "$CODATOR_CAPTURE_FILE"
exit 23
`
			}
			if err := os.WriteFile(filepath.Join(binDir, test.provider), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			launchDir := filepath.Join(root, "work")
			if err := os.Mkdir(launchDir, 0700); err != nil {
				t.Fatal(err)
			}
			argsJSON, err := json.Marshal(test.invocation)
			if err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCodatorPassthroughChild$")
			cmd.Dir = launchDir
			cmd.Env = buildEnv(os.Environ(), nil, map[string]string{
				"CODATOR_PASSTHROUGH_CHILD": "1",
				"CODATOR_TEST_ARGS":         string(argsJSON),
				"CODATOR_CAPTURE_FILE":      captureFile,
				"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"XDG_DATA_HOME":             dataHome,
			})
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
				t.Fatalf("native exit status = %v, want 23; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			got, err := os.ReadFile(captureFile)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Join(test.nativeArgs, "\x00") + "\x00"
			if string(got) != want {
				t.Fatalf("native argv = %q, want %q", strings.Split(string(got), "\x00"), test.nativeArgs)
			}
			if !strings.Contains(stderr.String(), "codator: using "+test.provider+" account work") {
				t.Fatalf("native invocation bypassed selected profile: stderr=%q", stderr.String())
			}
			lock, err := store.Lock(test.provider, "work")
			if err != nil {
				t.Fatalf("selected profile lock was not released: %v", err)
			}
			lock.Close()
		})
	}
}
