package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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
	root := tempDataHome(t)
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	launchFile := filepath.Join(root, "selected")
	argsFile := filepath.Join(root, "args")
	cwdFile := filepath.Join(root, "cwd")
	readyFile := filepath.Join(root, "native-ready")
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
    printf ready > "$CODATOR_LAUNCH_READY_FILE"
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
		"CODATOR_DISPATCH_CHILD":    "1",
		"CODATOR_LAUNCH_FILE":       launchFile,
		"CODATOR_LAUNCH_ARGS_FILE":  argsFile,
		"CODATOR_LAUNCH_CWD_FILE":   cwdFile,
		"CODATOR_LAUNCH_READY_FILE": readyFile,
		"CODATOR_RELEASE_FILE":      releaseFile,
		"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"XDG_DATA_HOME":             dataHome,
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
	waitForFile(t, readyFile, cmd)

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
	activeLock, err := store.Lock("codex", "high")
	if err != nil {
		t.Fatalf("ordinary Codex session retained the selected account lock: %v", err)
	}
	activeLock.Close()
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

func TestCodexConcurrentResumeReleasesOnlySessionLock(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAccount("codex", "work"); err != nil {
		t.Fatal(err)
	}
	root := tempDataHome(t)
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	writeBlockingCodexStub(t, filepath.Join(binDir, "codex"))
	launches, release := filepath.Join(root, "launches"), filepath.Join(root, "release")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(session string) *exec.Cmd {
		args, err := json.Marshal([]string{"codex", "--account", "work", "resume", session})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestCodatorPassthroughChild$")
		cmd.Env = buildEnv(os.Environ(), nil, map[string]string{
			"CODATOR_PASSTHROUGH_CHILD": "1",
			"CODATOR_TEST_ARGS":         string(args),
			"CODATOR_LAUNCHES":          launches,
			"CODATOR_RELEASE_FILE":      release,
			"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
			"XDG_DATA_HOME":             dataHome,
		})
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		return cmd
	}
	first := start("first")
	t.Cleanup(func() {
		_ = os.WriteFile(release, nil, 0600)
		if first.ProcessState == nil {
			_ = first.Process.Kill()
			_ = first.Wait()
		}
	})
	waitForFile(t, launches, first)
	if err := first.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("first resume stopped before second launch: %v", err)
	}
	second := start("second")
	t.Cleanup(func() {
		if second.ProcessState == nil {
			_ = second.Process.Kill()
			_ = second.Wait()
		}
	})
	waitForLaunchCount(t, launches, 2, first, second)
	lock, err := store.Lock("codex", "work")
	if err != nil {
		t.Fatalf("active Codex resume retained the session lock: %v", err)
	}
	lock.Close()
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := first.Wait(); err != nil {
		t.Fatalf("first resume: %v", err)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second resume: %v", err)
	}
}

func TestCodexCredentialMutationsKeepProfileLock(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "Codator login", args: []string{"login", "codex", "work"}},
		{name: "native login", args: []string{"codex", "--account", "work", "login"}},
		{name: "native logout", args: []string{"codex", "--account", "work", "logout"}},
		{name: "native MCP login", args: []string{"codex", "--account", "work", "mcp", "login", "synthetic"}},
		{name: "native MCP logout", args: []string{"codex", "--account", "work", "mcp", "logout", "synthetic"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome := tempDataHome(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.EnsureAccount("codex", "work"); err != nil {
				t.Fatal(err)
			}
			root := tempDataHome(t)
			binDir := filepath.Join(root, "bin")
			if err := os.Mkdir(binDir, 0700); err != nil {
				t.Fatal(err)
			}
			writeBlockingCodexStub(t, filepath.Join(binDir, "codex"))
			launches, release := filepath.Join(root, "launches"), filepath.Join(root, "release")
			args, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCodatorPassthroughChild$")
			cmd.Env = buildEnv(os.Environ(), nil, map[string]string{
				"CODATOR_PASSTHROUGH_CHILD": "1",
				"CODATOR_TEST_ARGS":         string(args),
				"CODATOR_LAUNCHES":          launches,
				"CODATOR_RELEASE_FILE":      release,
				"PATH":                      binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"XDG_DATA_HOME":             dataHome,
			})
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = os.WriteFile(release, nil, 0600)
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			waitForFile(t, launches, cmd)
			if lock, err := store.Lock("codex", "work"); !errors.Is(err, ErrAccountBusy) {
				if lock != nil {
					lock.Close()
				}
				t.Fatalf("credential mutation lock error=%v, want busy", err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatalf("credential mutation: %v", err)
			}
		})
	}
}

func writeBlockingCodexStub(t *testing.T, path string) {
	t.Helper()
	script := `#!/bin/sh
case "$*" in
  *app-server*)
    while IFS= read -r line; do
      case "$line" in
        *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
        *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","planType":"plus"}}}' ;;
        *'"method":"account/rateLimits/read"'*) printf '%s\n' '{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":10,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":20,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false}}}}' ;;
      esac
    done
    exit 0
    ;;
esac
printf '%s\n' "$*" >> "$CODATOR_LAUNCHES"
while [ ! -f "$CODATOR_RELEASE_FILE" ]; do :; done
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func waitForLaunchCount(t *testing.T, path string, want int, commands ...*exec.Cmd) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			text := strings.TrimSuffix(string(data), "\n")
			if text != "" && strings.Count(text, "\n")+1 >= want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, command := range commands {
		_ = command.Process.Kill()
		_ = command.Wait()
	}
	t.Fatalf("native launch count did not reach %d", want)
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
			name:       "unmapped older Claude model ID stays opaque",
			provider:   "claude",
			invocation: []string{"claude", "--account", "work", "--model", "claude-fable-5", "--", "prompt"},
			nativeArgs: []string{"--model", "claude-fable-5", "--", "prompt"},
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

			root := tempDataHome(t)
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

func TestLaunchClaudeSelectsByModelAndPreservesArgs(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"alpha", "beta"} {
		if _, err := store.EnsureAccount("claude", name); err != nil {
			t.Fatal(err)
		}
	}

	root := tempDataHome(t)
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	launchDir := filepath.Join(root, "work")
	if err := os.Mkdir(launchDir, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
profile=${CLAUDE_CONFIG_DIR%/native}
profile=${profile##*/}
if [ "$1" = --print ]; then
  case "$profile" in
    alpha) fable=100; sonnet=10; opus=90 ;;
    beta) fable=25; sonnet=80; opus=20 ;;
    *) exit 90 ;;
  esac
  while IFS= read -r line; do
    case "$line" in
      *'"request_id":"init-1"'*)
        printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}'
        ;;
      *'"request_id":"usage-1"'*)
        printf '%s\n' "{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"usage-1\",\"response\":{\"subscription_type\":\"max\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":10,\"resets_at\":\"2099-01-01T00:00:00Z\"},\"seven_day\":{\"utilization\":20,\"resets_at\":\"2099-01-01T00:00:00Z\"},\"seven_day_opus\":{\"utilization\":$opus,\"resets_at\":\"2099-01-01T00:00:00Z\"},\"seven_day_sonnet\":{\"utilization\":$sonnet,\"resets_at\":\"2099-01-01T00:00:00Z\"},\"model_scoped\":[{\"display_name\":\"Fable\",\"utilization\":$fable,\"resets_at\":\"2099-01-01T00:00:00Z\"},{\"display_name\":\"Sonnet\",\"utilization\":$sonnet,\"resets_at\":\"2099-01-01T00:00:00Z\"},{\"display_name\":\"Opus\",\"utilization\":$opus,\"resets_at\":\"2099-01-01T00:00:00Z\"}]}}}}"
        exit 0
        ;;
    esac
  done
  exit 91
fi
printf '%s\n' "$profile" > "$CODATOR_SELECTED_FILE"
printf '%s\0' "$@" > "$CODATOR_CAPTURE_FILE"
exit 23
`
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		want       string
		wantNative []string
		blocked    bool
	}{
		{
			name: "automatic Fable 5.1 uses a different eligible account",
			args: []string{"claude", "--model=claude-fable-5-1", "--", "run this Fable prompt"},
			want: "beta", wantNative: []string{"--model=claude-fable-5-1", "--", "run this Fable prompt"},
		},
		{
			name: "automatic Sonnet 5 prefers the Fable-exhausted account",
			args: []string{"claude", "--model", "claude-sonnet-5", "--dangerously-skip-permissions",
				"--permission-mode", "bypassPermissions", "--effort=high", "--output-format", "json", "-p",
				"run this Sonnet prompt"},
			want: "alpha", wantNative: []string{"--model", "claude-sonnet-5", "--dangerously-skip-permissions",
				"--permission-mode", "bypassPermissions", "--effort=high", "--output-format", "json", "-p",
				"run this Sonnet prompt"},
		},
		{
			name: "automatic Opus 5 uses its own weekly bucket",
			args: []string{"claude", "--model=claude-opus-5", "--", "run this Opus prompt"},
			want: "beta", wantNative: []string{"--model=claude-opus-5", "--", "run this Opus prompt"},
		},
		{
			name: "explicit Sonnet 5 account remains eligible",
			args: []string{"claude", "--account", "alpha", "--model", "claude-sonnet-5", "--", "explicit Sonnet"},
			want: "alpha", wantNative: []string{"--model", "claude-sonnet-5", "--", "explicit Sonnet"},
		},
		{
			name:    "explicit Fable 5.1 account is blocked",
			args:    []string{"claude", "--account", "alpha", "--model=claude-fable-5-1", "--", "explicit Fable"},
			blocked: true,
		},
	}
	for i, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selectedFile := filepath.Join(root, "selected-"+strconv.Itoa(i))
			captureFile := filepath.Join(root, "argv-"+strconv.Itoa(i))
			argsJSON, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCodatorPassthroughChild$")
			cmd.Dir = launchDir
			cmd.Env = []string{
				"HOME=" + root,
				"PATH=" + binDir + string(os.PathListSeparator) + "/usr/bin:/bin",
				"XDG_DATA_HOME=" + dataHome,
				"CODATOR_PASSTHROUGH_CHILD=1",
				"CODATOR_TEST_ARGS=" + string(argsJSON),
				"CODATOR_SELECTED_FILE=" + selectedFile,
				"CODATOR_CAPTURE_FILE=" + captureFile,
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if test.blocked {
				if !ok || exit.ExitCode() != 1 || !strings.Contains(stderr.String(), "selected account is ineligible") {
					t.Fatalf("explicit exhausted launch = %v, stderr=%q", err, stderr.String())
				}
				if _, err := os.Stat(captureFile); !os.IsNotExist(err) {
					t.Fatalf("blocked launch reached native Claude, stat err=%v", err)
				}
				lock, err := store.Lock("claude", "alpha")
				if err != nil {
					t.Fatalf("blocked account lock was not released: %v", err)
				}
				lock.Close()
				return
			}
			if !ok || exit.ExitCode() != 23 {
				t.Fatalf("native exit status = %v, want 23; stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			selected, err := os.ReadFile(selectedFile)
			if err != nil || strings.TrimSpace(string(selected)) != test.want {
				t.Fatalf("selected account = %q, want %q; err=%v", selected, test.want, err)
			}
			got, err := os.ReadFile(captureFile)
			if err != nil {
				t.Fatal(err)
			}
			want := strings.Join(test.wantNative, "\x00") + "\x00"
			if string(got) != want {
				t.Fatalf("native argv = %q, want %q", strings.Split(string(got), "\x00"), test.wantNative)
			}
			if !strings.Contains(stderr.String(), "codator: using claude account "+test.want) {
				t.Fatalf("launch did not report selected profile: stderr=%q", stderr.String())
			}
			lock, err := store.Lock("claude", test.want)
			if err != nil {
				t.Fatalf("selected account lock was not released: %v", err)
			}
			lock.Close()
		})
	}
}
