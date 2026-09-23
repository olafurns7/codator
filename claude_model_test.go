package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runtimeDefaultQuotaPayload(shared, opus int) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":10,"resets_at":"2099-01-01T00:00:00Z"},"seven_day":{"utilization":%d,"resets_at":"2099-01-01T00:00:00Z"},"model_scoped":[{"display_name":"Fable","utilization":100,"resets_at":"2099-01-01T00:00:00Z"},{"display_name":"Opus","utilization":%d,"resets_at":"2099-01-01T00:00:00Z"}]}}`, shared, opus))
}

func TestClaudeImplicitRuntimeDefaultLaunch(t *testing.T) {
	for _, settings := range []struct{ name, content string }{
		{"missing settings", ""},
		{"empty settings", `{}`},
		{"null model", `{"model":null}`},
	} {
		t.Run(settings.name, func(t *testing.T) {
			dataHome, home, binDir, account := newFakeClaudeProfile(t)
			if settings.content != "" {
				if err := os.WriteFile(filepath.Join(account.NativeDir, "settings.json"), []byte(settings.content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			installFakeClaude(t, binDir, runtimeDefaultQuotaPayload(20, 30), false, 0, false)
			if err := os.WriteFile(filepath.Join(binDir, ".version"), []byte("2.1.280 (Claude Code)\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "claude"}); code != 0 {
				t.Fatalf("warm cache code=%d stderr=%q", code, stderr)
			}
			args := []string{"--dangerously-skip-permissions", "--permission-mode", "bypassPermissions"}
			for _, command := range [][]string{append([]string{"claude"}, args...), append([]string{"claude", "--account", "work"}, args...)} {
				if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, command); code != 0 {
					t.Fatalf("plain launch %q code=%d stderr=%q", command, code, stderr)
				}
				got, err := os.ReadFile(filepath.Join(filepath.Dir(account.NativeDir), ".launch-argv"))
				if err != nil || string(got) != strings.Join(args, "\x00")+"\x00" {
					t.Fatalf("native args=%q err=%v", got, err)
				}
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("usage probes=%d, want warm-cache probe only", got)
			}
			if got := fakeClaudeCount(t, account, ".launch-count"); got != 2 {
				t.Fatalf("native launches=%d, want 2", got)
			}
			if got := fakeClaudeCount(t, Account{NativeDir: filepath.Join(binDir, "native")}, ".version-count"); got != 2 {
				t.Fatalf("local version checks=%d, want one per launch", got)
			}
		})
	}
}

func TestClaudeRuntimeDefaultChecksVersionOnceAcrossAccounts(t *testing.T) {
	dataHome, home, binDir, first := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.EnsureAccount("claude", "second")
	if err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, runtimeDefaultQuotaPayload(20, 30), false, 0, false)
	if err := os.WriteFile(filepath.Join(binDir, ".version"), []byte("2.1.280 (Claude Code)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"claude"}); code != 0 {
		t.Fatalf("automatic launch code=%d stderr=%q", code, stderr)
	}
	if got := fakeClaudeCount(t, Account{NativeDir: filepath.Join(binDir, "native")}, ".version-count"); got != 1 {
		t.Fatalf("version checks=%d, want one across both accounts", got)
	}
	for _, account := range []Account{first, second} {
		if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
			t.Fatalf("%s usage probes=%d, want 1", account.Name, got)
		}
	}
}

func TestClaudeKnownModelDoesNotNeedVersion(t *testing.T) {
	for _, test := range []struct {
		name, settings string
		args           []string
	}{
		{"saved Opus", `{"model":"opus[1m]"}`, nil},
		{"explicit Opus", "", []string{"--model", "opus"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, home, binDir, account := newFakeClaudeProfile(t)
			if test.settings != "" {
				if err := os.WriteFile(filepath.Join(account.NativeDir, "settings.json"), []byte(test.settings), 0600); err != nil {
					t.Fatal(err)
				}
			}
			installFakeClaude(t, binDir, runtimeDefaultQuotaPayload(20, 30), false, 0, false)
			if err := os.WriteFile(filepath.Join(binDir, ".version-fail"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			command := append([]string{"claude", "--account", "work"}, test.args...)
			if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, command); code != 0 {
				t.Fatalf("known model launch code=%d stderr=%q", code, stderr)
			}
			if got := fakeClaudeCount(t, Account{NativeDir: filepath.Join(binDir, "native")}, ".version-count"); got != 0 {
				t.Fatalf("known model checked unavailable version %d times", got)
			}
		})
	}
}

func TestClaudeRuntimeDefaultConservativeCases(t *testing.T) {
	for _, test := range []struct {
		name, version, settings string
		shared, opus            int
		args                    []string
		wantVersion             int
	}{
		{"shared cap", "2.1.280 (Claude Code)\n", "", 100, 20, nil, 1},
		{"Opus cap", "2.1.280 (Claude Code)\n", "", 20, 100, nil, 1},
		{"saved Fable", "2.1.280 (Claude Code)\n", `{"model":"fable"}`, 20, 20, nil, 0},
		{"explicit Fable", "2.1.280 (Claude Code)\n", "", 20, 20, []string{"--model", "fable"}, 0},
		{"older release", "2.1.279 (Claude Code)\n", "", 20, 20, nil, 1},
		{"unsupported release", "2.2.0 (Claude Code)\n", "", 20, 20, nil, 1},
		{"non-numeric patch", "2.1.+280 (Claude Code)\n", "", 20, 20, nil, 1},
		{"malformed version", "Claude Code 2.1.280\n", "", 20, 20, nil, 1},
		{"version failure", "", "", 20, 20, nil, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, home, binDir, account := newFakeClaudeProfile(t)
			if test.settings != "" {
				if err := os.WriteFile(filepath.Join(account.NativeDir, "settings.json"), []byte(test.settings), 0600); err != nil {
					t.Fatal(err)
				}
			}
			installFakeClaude(t, binDir, runtimeDefaultQuotaPayload(test.shared, test.opus), false, 0, false)
			if test.version == "" {
				if err := os.WriteFile(filepath.Join(binDir, ".version-fail"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(filepath.Join(binDir, ".version"), []byte(test.version), 0600); err != nil {
				t.Fatal(err)
			}
			if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "claude"}); code != 0 {
				t.Fatalf("warm cache code=%d stderr=%q", code, stderr)
			}
			command := append([]string{"claude", "--account", "work"}, test.args...)
			if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, command); code != 1 || !strings.Contains(stderr, "selected account is ineligible") {
				t.Fatalf("denied launch code=%d stderr=%q", code, stderr)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("usage probes=%d, want warm-cache probe only", got)
			}
			if got := fakeClaudeCount(t, account, ".launch-count"); got != 0 {
				t.Fatalf("denied launch reached native CLI %d times", got)
			}
			if got := fakeClaudeCount(t, Account{NativeDir: filepath.Join(binDir, "native")}, ".version-count"); got != test.wantVersion {
				t.Fatalf("version checks=%d, want %d", got, test.wantVersion)
			}
		})
	}
}

func TestClaudeRuntimeDefaultProbeIsBoundedAndCancellable(t *testing.T) {
	binDir := t.TempDir()
	path := filepath.Join(binDir, "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nsleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if got := claudeRuntimeDefault(ctx, path); got != "" || time.Since(start) > 2*time.Second {
		t.Fatalf("cancelled version probe model=%q duration=%s", got, time.Since(start))
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '2.1.280 (Claude Code)'\nprintf '%0256d' 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := claudeRuntimeDefault(context.Background(), path); got != "" {
		t.Fatalf("oversized version output narrowed to %q", got)
	}
}

func TestClaudeSavedModelInHomeRepoAndNeutralProjectSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_DEFAULT_MODEL", "")
	global := filepath.Join(home, ".claude")
	if err := os.Mkdir(global, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(global, "settings.json"), []byte(`{"model":"opus[1m]"}`), 0600); err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(home, "projects", "trip")
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	nested := filepath.Join(repo, "packages", "api")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("claude", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(global, "settings.json"), filepath.Join(account.NativeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(repo, ".claude", "settings.json")
	if err := os.WriteFile(project, []byte(`{"permissions":{"allow":["Read"]},"hooks":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, account, nil); got != claudeModelOpus {
		t.Fatalf("home global + neutral project model=%q, want Opus", got)
	}
	if err := os.WriteFile(project, []byte(`{"model":"fable"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, account, nil); got != "" {
		t.Fatalf("project model override narrowed to %q", got)
	}
	if got := launchClaudeModel("claude", []string{"--model", "opus"}, account, nil); got != claudeModelOpus {
		t.Fatalf("explicit CLI model=%q, want Opus", got)
	}
	if err := os.WriteFile(project, []byte(`{"permissions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"), []byte(`{"env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"fable"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, account, nil); got != "" {
		t.Fatalf("local model remap narrowed to %q", got)
	}
}

func TestClaudeSavedModelPrecedenceAndUnsafeFiles(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	alpha, err := store.EnsureAccount("claude", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	beta, err := store.EnsureAccount("claude", "beta")
	if err != nil {
		t.Fatal(err)
	}
	alphaFile := filepath.Join(alpha.NativeDir, "settings.json")
	if err := os.WriteFile(alphaFile, []byte(`{"model":"opus[1m]"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beta.NativeDir, "settings.json"), []byte(`{"model":"sonnet[1m]"}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("ANTHROPIC_DEFAULT_MODEL", "")
	if got := launchClaudeModel("claude", nil, alpha, nil); got != claudeModelOpus {
		t.Fatalf("alpha model=%q", got)
	}
	if got := launchClaudeModel("claude", nil, beta, nil); got != claudeModelSonnet {
		t.Fatalf("beta model=%q", got)
	}
	if err := os.Remove(filepath.Join(beta.NativeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_DEFAULT_MODEL", "sonnet[1m]")
	if got := launchClaudeModel("claude", nil, beta, nil); got != claudeModelSonnet {
		t.Fatalf("ANTHROPIC_DEFAULT_MODEL fallback=%q", got)
	}
	t.Setenv("ANTHROPIC_MODEL", "fable")
	if got := launchClaudeModel("claude", nil, alpha, nil); got != claudeModelFable {
		t.Fatalf("ANTHROPIC_MODEL precedence=%q", got)
	}
	if got := launchClaudeModel("claude", []string{"--model", "opus"}, alpha, nil); got != claudeModelOpus {
		t.Fatalf("explicit CLI precedence=%q", got)
	}
	if err := os.WriteFile(alphaFile, []byte(`{"model":"opus","env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"fable"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, alpha, nil); got != "" {
		t.Fatalf("saved env remap narrowed ANTHROPIC_MODEL to %q", got)
	}
	if err := os.WriteFile(alphaFile, []byte(`{"model":"opus"`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, alpha, nil); got != "" {
		t.Fatalf("malformed settings narrowed to %q", got)
	}
	if err := os.Remove(alphaFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(alphaFile, 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, alpha, nil); got != "" {
		t.Fatalf("FIFO settings narrowed to %q", got)
	}
}

func TestClaudeLinkedMainCheckoutLocalModelSettings(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "main")
	linked := filepath.Join(root, "linked")
	if output, err := exec.Command("git", "init", "-q", main).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	commit := exec.Command("git", "-C", main, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-q", "--allow-empty", "-m", "init")
	if output, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v %s", err, output)
	}
	if output, err := exec.Command("git", "-C", main, "worktree", "add", "-q", "-b", "linked", linked).CombinedOutput(); err != nil {
		t.Fatalf("git worktree: %v %s", err, output)
	}
	if err := os.Mkdir(filepath.Join(main, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(main, ".claude", "settings.local.json"), []byte(`{"model":"fable"}`), 0600); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(linked); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	if !claudeProjectOrManagedSettings() {
		t.Fatal("linked main checkout local model override was missed")
	}
}
