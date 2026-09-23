package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

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
	if got := launchClaudeModel("claude", nil, account); got != claudeModelOpus {
		t.Fatalf("home global + neutral project model=%q, want Opus", got)
	}
	if err := os.WriteFile(project, []byte(`{"model":"fable"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, account); got != "" {
		t.Fatalf("project model override narrowed to %q", got)
	}
	if got := launchClaudeModel("claude", []string{"--model", "opus"}, account); got != claudeModelOpus {
		t.Fatalf("explicit CLI model=%q, want Opus", got)
	}
	if err := os.WriteFile(project, []byte(`{"permissions":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".claude", "settings.local.json"), []byte(`{"env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"fable"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, account); got != "" {
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
	if got := launchClaudeModel("claude", nil, alpha); got != claudeModelOpus {
		t.Fatalf("alpha model=%q", got)
	}
	if got := launchClaudeModel("claude", nil, beta); got != claudeModelSonnet {
		t.Fatalf("beta model=%q", got)
	}
	if err := os.Remove(filepath.Join(beta.NativeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_DEFAULT_MODEL", "sonnet[1m]")
	if got := launchClaudeModel("claude", nil, beta); got != claudeModelSonnet {
		t.Fatalf("ANTHROPIC_DEFAULT_MODEL fallback=%q", got)
	}
	t.Setenv("ANTHROPIC_MODEL", "fable")
	if got := launchClaudeModel("claude", nil, alpha); got != claudeModelFable {
		t.Fatalf("ANTHROPIC_MODEL precedence=%q", got)
	}
	if got := launchClaudeModel("claude", []string{"--model", "opus"}, alpha); got != claudeModelOpus {
		t.Fatalf("explicit CLI precedence=%q", got)
	}
	if err := os.WriteFile(alphaFile, []byte(`{"model":"opus","env":{"ANTHROPIC_DEFAULT_OPUS_MODEL":"fable"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, alpha); got != "" {
		t.Fatalf("saved env remap narrowed ANTHROPIC_MODEL to %q", got)
	}
	if err := os.WriteFile(alphaFile, []byte(`{"model":"opus"`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, alpha); got != "" {
		t.Fatalf("malformed settings narrowed to %q", got)
	}
	if err := os.Remove(alphaFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(alphaFile, 0600); err != nil {
		t.Fatal(err)
	}
	if got := launchClaudeModel("claude", nil, alpha); got != "" {
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
