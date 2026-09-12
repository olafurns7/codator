package main

import (
	"bytes"
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
        *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt"}}}' ;;
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
