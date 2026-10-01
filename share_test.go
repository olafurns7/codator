package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Launches share configuration through $HOME; keep every test away from the
// developer's real ~/.claude and ~/.codex.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "codator-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func writeTestFile(t *testing.T, path, content string, modified time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestShareConfigLinksProfilesWithoutLosingConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shared := filepath.Join(home, ".claude")
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	accounts := map[string]Account{}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		account, err := store.EnsureAccount("claude", name)
		if err != nil {
			t.Fatal(err)
		}
		accounts[name] = account
		writeTestFile(t, filepath.Join(account.NativeDir, ".credentials.json"), "secret "+name, time.Now())
		writeTestFile(t, filepath.Join(account.NativeDir, ".claude.json"), `{"oauthAccount":"`+name+`"}`, time.Now())
	}
	old, recent := time.Now().Add(-time.Hour), time.Now()
	alpha, beta, gamma := accounts["alpha"].NativeDir, accounts["beta"].NativeDir, accounts["gamma"].NativeDir
	// Nothing is shared yet: the newest settings seed the shared file, and the
	// older copy is merged into it (shared values win) and kept.
	writeTestFile(t, filepath.Join(alpha, "settings.json"), `{"model":"opus","env":{"A":"&&"}}`, old)
	writeTestFile(t, filepath.Join(beta, "settings.json"), `{"model":"sonnet"}`, recent)
	// Directories merge; identical entries collapse, conflicting ones are kept.
	writeTestFile(t, filepath.Join(alpha, "skills", "a", "SKILL.md"), "a", old)
	writeTestFile(t, filepath.Join(alpha, "skills", "same", "SKILL.md"), "same", old)
	writeTestFile(t, filepath.Join(beta, "skills", "b", "SKILL.md"), "b", recent)
	writeTestFile(t, filepath.Join(beta, "skills", "same", "SKILL.md"), "same", recent)
	writeTestFile(t, filepath.Join(shared, "agents", "x.md"), "shared", old)
	writeTestFile(t, filepath.Join(alpha, "agents", "x.md"), "profile", old)
	writeTestFile(t, filepath.Join(alpha, "agents", "y.md"), "only alpha", old)
	writeTestFile(t, filepath.Join(shared, "CLAUDE.md"), "rules", old)
	writeTestFile(t, filepath.Join(gamma, "CLAUDE.md"), "rules", old)
	// A link the user made on purpose is left alone.
	custom := filepath.Join(home, "custom-keybindings.json")
	writeTestFile(t, custom, "[]", old)
	if err := os.Symlink(custom, filepath.Join(beta, "keybindings.json")); err != nil {
		t.Fatal(err)
	}

	var out strings.Builder
	if err := shareConfig(context.Background(), store, "claude", &out); err != nil {
		t.Fatal(err)
	}
	for _, item := range []string{"settings.json", "skills", "agents", "CLAUDE.md"} {
		for name, account := range accounts {
			if target, err := os.Readlink(filepath.Join(account.NativeDir, item)); err != nil || target != filepath.Join(shared, item) {
				t.Fatalf("%s %s link = %q, %v", name, item, target, err)
			}
		}
	}
	if got := readTestFile(t, filepath.Join(gamma, "settings.json")); got != "{\n  \"env\": {\n    \"A\": \"&&\"\n  },\n  \"model\": \"sonnet\"\n}\n" {
		t.Fatalf("shared settings = %q, want the newest copy plus the older one's other keys", got)
	}
	for name, want := range map[string]string{"a": "a", "b": "b", "same": "same"} {
		if got := readTestFile(t, filepath.Join(shared, "skills", name, "SKILL.md")); got != want {
			t.Fatalf("shared skill %s = %q", name, got)
		}
	}
	if got := readTestFile(t, filepath.Join(shared, "agents", "y.md")); got != "only alpha" {
		t.Fatalf("profile-only agent was not shared: %q", got)
	}
	backups := func(dir string) []string {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.before-sharing-*"))
		return matches
	}
	alphaBackups, gammaBackups := backups(alpha), backups(gamma)
	if len(alphaBackups) != 2 || len(backups(beta)) != 0 || len(gammaBackups) != 0 {
		t.Fatalf("backups alpha=%v beta=%v gamma=%v", alphaBackups, backups(beta), gammaBackups)
	}
	for _, backup := range alphaBackups {
		switch filepath.Base(backup)[:strings.Index(filepath.Base(backup), ".before-sharing-")] {
		case "settings.json":
			if readTestFile(t, backup) != `{"model":"opus","env":{"A":"&&"}}` {
				t.Fatal("lost the older settings")
			}
		case "agents":
			if readTestFile(t, filepath.Join(backup, "x.md")) != "profile" || readTestFile(t, filepath.Join(shared, "agents", "x.md")) != "shared" {
				t.Fatal("conflicting agent was overwritten or lost")
			}
		default:
			t.Fatalf("unexpected backup %s", backup)
		}
	}
	if target, _ := os.Readlink(filepath.Join(beta, "keybindings.json")); target != custom {
		t.Fatalf("replaced a user link: %q", target)
	}
	for name, account := range accounts {
		for _, item := range []string{".credentials.json", ".claude.json"} {
			if info, err := os.Lstat(filepath.Join(account.NativeDir, item)); err != nil || !info.Mode().IsRegular() {
				t.Fatalf("%s %s is no longer private to the account", name, item)
			}
		}
	}
	for _, want := range []string{
		"moved claude account beta's settings.json to " + filepath.Join(shared, "settings.json"),
		"merged claude account alpha's settings.json into " + filepath.Join(shared, "settings.json") + ";",
		"claude account alpha now uses the shared " + filepath.Join(shared, "agents") + ";",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output %q lacks %q", out.String(), want)
		}
	}

	// Native writes go through the link, and a later run changes nothing.
	if err := os.WriteFile(filepath.Join(alpha, "settings.json"), []byte(`{"model":"haiku"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(gamma, "settings.json")); got != `{"model":"haiku"}` {
		t.Fatalf("edit under alpha is not visible to gamma: %q", got)
	}
	out.Reset()
	if err := shareConfig(context.Background(), store, "claude", &out); err != nil || out.Len() != 0 {
		t.Fatalf("second run err=%v output=%q", err, out.String())
	}
	// A new account starts with the shared configuration.
	delta, err := store.EnsureAccount("claude", "delta")
	if err != nil {
		t.Fatal(err)
	}
	if err := shareConfig(context.Background(), store, "claude", &out); err != nil {
		t.Fatal(err)
	}
	if got := readTestFile(t, filepath.Join(delta.NativeDir, "settings.json")); got != `{"model":"haiku"}` {
		t.Fatalf("new account settings = %q", got)
	}
}

func TestShareConfigMergesCodexStateKeyedByProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, accounts := setupMCPSharingTest(t)
	shared := filepath.Join(home, ".codex", "config.toml")
	trust := func(account string) map[string]any {
		return map[string]any{account + "/hooks.json:stop:0:0": map[string]any{"trusted_hash": "sha256:same"}}
	}
	sharedConfig := map[string]any{"model": "plain", "mcp_oauth_credentials_store": "keyring", "hooks": map[string]any{"state": trust(home + "/.codex")}}
	data, _ := json.Marshal(sharedConfig)
	writeTestFile(t, shared, string(data), time.Now())
	// Codex keys hook trust by each profile's own path, so replacing a profile's
	// config with the shared one would make its hooks need review again.
	for _, account := range accounts[:2] {
		config := readMCPSharingFixture(t, account)
		config["hooks"] = map[string]any{"state": trust(account.NativeDir)}
		config["projects"] = map[string]any{"/work/" + account.Name: map[string]any{"trust_level": "trusted"}}
		writeMCPSharingFixture(t, account, config)
	}
	// A login restriction belongs to its account and is never shared.
	restricted := `{"model":"three","forced_chatgpt_workspace_id":"workspace"}`
	writeTestFile(t, filepath.Join(accounts[2].NativeDir, "config.toml"), restricted, time.Now())

	var out strings.Builder
	if err := shareConfig(context.Background(), store, "codex", &out); err != nil {
		t.Fatal(err)
	}
	var config map[string]any
	if err := json.Unmarshal([]byte(readTestFile(t, shared)), &config); err != nil {
		t.Fatal(err)
	}
	state := config["hooks"].(map[string]any)["state"].(map[string]any)
	projects, _ := config["projects"].(map[string]any)
	if config["model"] != "plain" || len(state) != 3 || len(projects) != 2 {
		t.Fatalf("merged config = %+v", config)
	}
	for _, account := range accounts[:2] {
		if target, err := os.Readlink(filepath.Join(account.NativeDir, "config.toml")); err != nil || target != shared {
			t.Fatalf("%s config link = %q, %v", account.Name, target, err)
		}
		backups, _ := filepath.Glob(filepath.Join(account.NativeDir, "config.toml.before-sharing-*"))
		if len(backups) != 1 || !strings.Contains(readTestFile(t, backups[0]), `"model":"`+account.Name+`"`) {
			t.Fatalf("%s original config was not saved: %v", account.Name, backups)
		}
		if !strings.Contains(out.String(), "merged codex account "+account.Name+"'s config.toml") {
			t.Fatalf("output %q does not report the merge", out.String())
		}
	}
	if got := readTestFile(t, filepath.Join(accounts[2].NativeDir, "config.toml")); got != restricted {
		t.Fatalf("restricted config was shared: %q", got)
	}
	// Nor is a shared config that restricts login linked into a profile.
	for _, account := range accounts[:2] {
		if err := os.Remove(filepath.Join(account.NativeDir, "config.toml")); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, shared, restricted, time.Now())
	if err := shareConfig(context.Background(), store, "codex", io.Discard); err == nil || !strings.Contains(err.Error(), "restricts login") {
		t.Fatalf("restricted shared config: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(accounts[0].NativeDir, "config.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("linked a restricted shared config: %v", err)
	}
}

func TestShareConfigSharesCodexBundledPluginsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	shared := filepath.Join(home, ".codex")
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	var accounts []Account
	for _, name := range []string{"alpha", "beta", "gamma"} {
		account, err := store.EnsureAccount("codex", name)
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
	}
	alpha, beta, gamma := accounts[0].NativeDir, accounts[1].NativeDir, accounts[2].NativeDir
	// The desktop app writes the bundled marketplace and installs its plugins
	// only under ~/.codex.
	manifest := filepath.Join("computer-use", "1.0.0", ".codex-plugin", "plugin.json")
	source := filepath.Join(".tmp", "bundled-marketplaces", "openai-bundled", ".agents", "plugins", "marketplace.json")
	writeTestFile(t, filepath.Join(shared, "plugins", "cache", "openai-bundled", manifest), `{"mcpServers":"./.mcp.json"}`, time.Now())
	writeTestFile(t, filepath.Join(shared, source), `{"name":"openai-bundled"}`, time.Now())
	// The computer-use plugin runs its client app from CODEX_HOME/computer-use.
	writeTestFile(t, filepath.Join(shared, "computer-use", "config.json"), `{"strings":{}}`, time.Now())
	// Account-scoped plugin state stays in the profile.
	writeTestFile(t, filepath.Join(alpha, "plugins", "cache", "openai-curated-remote", "gmail", ".codex-remote-plugin-install.json"), "alpha remote", time.Now())
	writeTestFile(t, filepath.Join(alpha, "plugins", "data", "computer-use-openai-bundled", "state"), "alpha data", time.Now())
	// A profile whose whole plugins folder the user linked to ~/.codex keeps
	// that link, and the shared files it reaches are not absorbed into themselves.
	if err := os.Symlink(filepath.Join(shared, "plugins"), filepath.Join(beta, "plugins")); err != nil {
		t.Fatal(err)
	}
	// gamma has no plugins folder yet.

	var out strings.Builder
	if err := shareConfig(context.Background(), store, "codex", &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(shared, "plugins", "cache", "openai-bundled", manifest)); err != nil {
		t.Fatalf("shared bundled plugin was lost: %v", err)
	}
	for _, dir := range []string{alpha, gamma} {
		for _, item := range []string{filepath.Join("plugins", "cache", "openai-bundled"), filepath.Join(".tmp", "bundled-marketplaces"), "computer-use"} {
			if target, err := os.Readlink(filepath.Join(dir, item)); err != nil || target != filepath.Join(shared, item) {
				t.Fatalf("%s %s link = %q, %v", dir, item, target, err)
			}
		}
	}
	for _, dir := range accounts {
		if got := readTestFile(t, filepath.Join(dir.NativeDir, "plugins", "cache", "openai-bundled", manifest)); got != `{"mcpServers":"./.mcp.json"}` {
			t.Fatalf("%s bundled plugin manifest = %q", dir.Name, got)
		}
		if got := readTestFile(t, filepath.Join(dir.NativeDir, "computer-use", "config.json")); got != `{"strings":{}}` {
			t.Fatalf("%s computer-use config = %q", dir.Name, got)
		}
	}
	if target, err := os.Readlink(filepath.Join(beta, "plugins")); err != nil || target != filepath.Join(shared, "plugins") {
		t.Fatalf("replaced the user's plugins link: %q, %v", target, err)
	}
	for path, want := range map[string]string{
		filepath.Join(alpha, "plugins", "cache", "openai-curated-remote", "gmail", ".codex-remote-plugin-install.json"): "alpha remote",
		filepath.Join(alpha, "plugins", "data", "computer-use-openai-bundled", "state"):                                 "alpha data",
	} {
		if info, err := os.Lstat(filepath.Dir(path)); err != nil || info.Mode()&os.ModeSymlink != 0 || readTestFile(t, path) != want {
			t.Fatalf("%s is no longer private to the account", path)
		}
	}
	for _, path := range []string{filepath.Join(shared, "plugins", "cache", "openai-curated-remote"), filepath.Join(shared, "plugins", "data")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("account plugin state reached %s: %v", path, err)
		}
	}
	if matches, _ := filepath.Glob(filepath.Join(beta, "plugins", "cache", "*.before-sharing-*")); len(matches) != 0 || out.Len() != 0 {
		t.Fatalf("backups %v output %q", matches, out.String())
	}
}

func TestShareConfigSharesSessionTranscripts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	var claudeAccounts, codexAccounts []Account
	for _, name := range []string{"alpha", "beta"} {
		account, err := store.EnsureAccount("claude", name)
		if err != nil {
			t.Fatal(err)
		}
		claudeAccounts = append(claudeAccounts, account)
		account, err = store.EnsureAccount("codex", name)
		if err != nil {
			t.Fatal(err)
		}
		codexAccounts = append(codexAccounts, account)
	}

	old, recent := time.Now().Add(-2*time.Hour), time.Now()
	claudeShared := filepath.Join(home, ".claude", "projects")
	claudeSlug := "project-slug"
	claudeIDs := []string{
		"00000000-0000-4000-8000-000000000001",
		"00000000-0000-4000-8000-000000000002",
	}
	claudeOtherID := "00000000-0000-4000-8000-000000000003"
	claudeConflictID := "00000000-0000-4000-8000-000000000004"
	claudeTranscripts := map[string]string{
		claudeIDs[0] + ".jsonl":  "alpha Claude transcript",
		claudeIDs[1] + ".jsonl":  "beta Claude transcript",
		claudeOtherID + ".jsonl": "existing Claude transcript",
	}
	writeTestFile(t, filepath.Join(claudeShared, claudeSlug, claudeOtherID+".jsonl"), claudeTranscripts[claudeOtherID+".jsonl"], recent)
	for i, account := range claudeAccounts {
		writeTestFile(t, filepath.Join(account.NativeDir, "projects", claudeSlug, claudeIDs[i]+".jsonl"), claudeTranscripts[claudeIDs[i]+".jsonl"], recent)
	}
	writeTestFile(t, filepath.Join(claudeAccounts[0].NativeDir, "projects", claudeSlug, claudeConflictID+".jsonl"), "older Claude transcript", old)
	writeTestFile(t, filepath.Join(claudeAccounts[1].NativeDir, "projects", claudeSlug, claudeConflictID+".jsonl"), "newer Claude transcript", recent)
	if err := os.Chtimes(filepath.Join(claudeAccounts[0].NativeDir, "projects"), old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(claudeAccounts[1].NativeDir, "projects"), recent, recent); err != nil {
		t.Fatal(err)
	}
	if err := shareConfig(context.Background(), store, "claude", io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, account := range claudeAccounts {
		link := filepath.Join(account.NativeDir, "projects")
		if target, err := os.Readlink(link); err != nil || target != claudeShared {
			t.Fatalf("%s projects link = %q, %v", account.Name, target, err)
		}
		for file, want := range claudeTranscripts {
			if got := readTestFile(t, filepath.Join(link, claudeSlug, file)); got != want {
				t.Fatalf("%s Claude transcript %s = %q, want %q", account.Name, file, got, want)
			}
		}
	}
	for file, want := range claudeTranscripts {
		if got := readTestFile(t, filepath.Join(claudeShared, claudeSlug, file)); got != want {
			t.Fatalf("shared Claude transcript %s = %q, want %q", file, got, want)
		}
	}
	if got := readTestFile(t, filepath.Join(claudeShared, claudeSlug, claudeConflictID+".jsonl")); got != "newer Claude transcript" {
		t.Fatalf("shared conflicting Claude transcript = %q", got)
	}
	backups, err := filepath.Glob(filepath.Join(claudeAccounts[0].NativeDir, "projects.before-sharing-*"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("older Claude projects backups = %v, %v", backups, err)
	}
	if got := readTestFile(t, filepath.Join(backups[0], claudeSlug, claudeConflictID+".jsonl")); got != "older Claude transcript" {
		t.Fatalf("older Claude transcript backup = %q", got)
	}

	codexShared := filepath.Join(home, ".codex")
	codexSessionPaths := []string{
		filepath.Join("2026", "09", "30", "rollout-2026-09-30T10-00-00-00000000-0000-4000-8000-000000000005.jsonl"),
		filepath.Join("2026", "09", "30", "rollout-2026-09-30T11-00-00-00000000-0000-4000-8000-000000000006.jsonl"),
	}
	archivedPath := "rollout-2026-09-29T09-00-00-00000000-0000-4000-8000-000000000007.jsonl"
	type transcript struct {
		item, path, content string
	}
	codexTranscripts := []transcript{
		{"sessions", codexSessionPaths[0], "alpha Codex transcript"},
		{"sessions", codexSessionPaths[1], "beta Codex transcript"},
		{"archived_sessions", archivedPath, "archived Codex transcript"},
	}
	for i, account := range codexAccounts {
		transcript := codexTranscripts[i]
		writeTestFile(t, filepath.Join(account.NativeDir, transcript.item, transcript.path), transcript.content, recent)
	}
	archived := codexTranscripts[2]
	writeTestFile(t, filepath.Join(codexAccounts[0].NativeDir, archived.item, archived.path), archived.content, recent)
	if err := shareConfig(context.Background(), store, "codex", io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, account := range codexAccounts {
		for _, item := range []string{"sessions", "archived_sessions"} {
			if target, err := os.Readlink(filepath.Join(account.NativeDir, item)); err != nil || target != filepath.Join(codexShared, item) {
				t.Fatalf("%s %s link = %q, %v", account.Name, item, target, err)
			}
		}
		for _, transcript := range codexTranscripts {
			if got := readTestFile(t, filepath.Join(account.NativeDir, transcript.item, transcript.path)); got != transcript.content {
				t.Fatalf("%s %s transcript %s = %q, want %q", account.Name, transcript.item, transcript.path, got, transcript.content)
			}
		}
	}
	for _, transcript := range codexTranscripts {
		if got := readTestFile(t, filepath.Join(codexShared, transcript.item, transcript.path)); got != transcript.content {
			t.Fatalf("shared %s transcript %s = %q, want %q", transcript.item, transcript.path, got, transcript.content)
		}
	}
}

// Claude appends to its transcript by path without holding it open. Lines it
// appends while the profile folder is shared all reach the shared transcript.
func TestShareConfigKeepsTranscriptAppendedDuringSharing(t *testing.T) {
	for _, seed := range []bool{false, true} {
		t.Run(fmt.Sprintf("seed=%v", seed), func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			store, err := newStore(tempDataHome(t))
			if err != nil {
				t.Fatal(err)
			}
			account, err := store.EnsureAccount("claude", "alpha")
			if err != nil {
				t.Fatal(err)
			}
			shared := filepath.Join(home, ".claude", "projects")
			if !seed {
				writeTestFile(t, filepath.Join(shared, "slug", "other.jsonl"), "other", time.Now())
			}
			profile := filepath.Join(account.NativeDir, "projects")
			for i := 0; i < 2000; i++ {
				writeTestFile(t, filepath.Join(profile, "slug", fmt.Sprintf("z%04d.jsonl", i)), "old", time.Now())
			}
			live := filepath.Join(profile, "slug", "live.jsonl")
			writeTestFile(t, live, "line-0\n", time.Now())
			done := make(chan struct{})
			var wg sync.WaitGroup
			want := []string{"line-0"}
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 1; ; i++ {
					select {
					case <-done:
						return
					default:
					}
					file, err := os.OpenFile(live, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
					if err != nil {
						continue // the folder is being swapped
					}
					line := fmt.Sprintf("line-%d", i)
					if _, err := fmt.Fprintln(file, line); err == nil {
						want = append(want, line)
					}
					file.Close()
				}
			}()
			time.Sleep(10 * time.Millisecond)
			for launch := 0; launch < 2; launch++ {
				if err := shareConfig(context.Background(), store, "claude", io.Discard); err != nil {
					close(done)
					wg.Wait()
					t.Fatalf("launch %d: %v", launch, err)
				}
			}
			close(done)
			wg.Wait()
			if target, err := os.Readlink(profile); err != nil || target != shared {
				t.Fatalf("projects link = %q, %v", target, err)
			}
			if got := strings.Fields(readTestFile(t, filepath.Join(shared, "slug", "live.jsonl"))); strings.Join(got, " ") != strings.Join(want, " ") {
				t.Fatalf("shared transcript has %d of %d lines", len(got), len(want))
			}
			if backups, _ := filepath.Glob(profile + ".before-sharing-*"); len(backups) != 0 {
				t.Fatalf("backups = %v", backups)
			}
		})
	}
}

// Each profile's Claude memory index lines are added to the shared index.
func TestShareConfigMergesClaudeMemoryIndex(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(home, ".claude", "projects", "slug", "memory")
	writeTestFile(t, filepath.Join(shared, "MEMORY.md"), "- [Common](common.md) — common", time.Now())
	writeTestFile(t, filepath.Join(shared, "common.md"), "common", time.Now())
	var accounts []Account
	for _, name := range []string{"alpha", "beta"} {
		account, err := store.EnsureAccount("claude", name)
		if err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
		dir := filepath.Join(account.NativeDir, "projects", "slug", "memory")
		writeTestFile(t, filepath.Join(dir, "MEMORY.md"), "- [Common](common.md) — common\n- ["+name+"]("+name+".md) — "+name+"\n", time.Now())
		writeTestFile(t, filepath.Join(dir, name+".md"), name, time.Now())
	}
	if err := shareConfig(context.Background(), store, "claude", io.Discard); err != nil {
		t.Fatal(err)
	}
	index := readTestFile(t, filepath.Join(shared, "MEMORY.md"))
	for _, line := range []string{"- [Common](common.md) — common", "- [alpha](alpha.md) — alpha", "- [beta](beta.md) — beta"} {
		if strings.Count(index, line+"\n") != 1 {
			t.Fatalf("shared index %q lacks one %q", index, line)
		}
	}
	for _, account := range accounts {
		if got := readTestFile(t, filepath.Join(shared, account.Name+".md")); got != account.Name {
			t.Fatalf("%s memory = %q", account.Name, got)
		}
		backups, _ := filepath.Glob(filepath.Join(account.NativeDir, "projects.before-sharing-*", "slug", "memory", "MEMORY.md"))
		if len(backups) != 1 || !strings.Contains(readTestFile(t, backups[0]), account.Name) {
			t.Fatalf("%s index backup = %v", account.Name, backups)
		}
	}
}

// A folder the user linked between a profile and a nested shared item is left
// as it is, whatever it points to, and nothing is shared through it.
func TestShareConfigLeavesLinkedParentsOfNestedItems(t *testing.T) {
	for _, link := range []struct{ item, parent string }{
		{"plugins/cache/openai-bundled", "plugins"},
		{"plugins/cache/openai-bundled", "plugins/cache"},
		{".tmp/bundled-marketplaces", ".tmp"},
	} {
		for _, tc := range []struct {
			name, parent, own string // parent: shared, custom, or a regular folder
			shared            bool
		}{
			{"shared parent", "shared", "", true},
			{"custom parent, different", "custom", "custom", true},
			{"custom parent, identical", "custom", "shared", true},
			{"custom parent, missing child", "custom", "", true},
			{"custom parent, no shared target", "custom", "custom", false},
			{"regular copy, different", "regular", "custom", true},
			{"nothing anywhere", "regular", "", false},
		} {
			t.Run(link.parent+": "+tc.name, func(t *testing.T) {
				home := t.TempDir()
				native := filepath.Join(home, "profile")
				target := filepath.Join(home, ".codex", link.item)
				rel, _ := filepath.Rel(link.parent, link.item)
				custom := filepath.Join(home, "custom")
				if tc.shared {
					writeTestFile(t, filepath.Join(target, "manifest"), "shared", time.Now())
				}
				switch tc.parent {
				case "shared", "custom":
					dest := filepath.Join(home, ".codex", link.parent)
					if tc.parent == "custom" {
						dest = custom
					}
					if err := os.MkdirAll(dest, 0700); err != nil {
						t.Fatal(err)
					}
					if tc.own != "" {
						writeTestFile(t, filepath.Join(dest, rel, "manifest"), tc.own, time.Now())
					}
					if err := os.MkdirAll(filepath.Dir(filepath.Join(native, link.parent)), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(dest, filepath.Join(native, link.parent)); err != nil {
						t.Fatal(err)
					}
				default:
					if tc.own != "" {
						writeTestFile(t, filepath.Join(native, link.item, "manifest"), tc.own, time.Now())
					}
				}
				account := Account{Provider: "codex", Name: "alpha", NativeDir: native}
				for range 2 {
					if err := shareConfigItem(context.Background(), "codex", []Account{account}, link.item, target, "stamp", io.Discard); err != nil {
						t.Fatal(err)
					}
				}
				if tc.shared && readTestFile(t, filepath.Join(target, "manifest")) != "shared" {
					t.Fatal("shared content changed")
				}
				if !tc.shared {
					if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("created the shared target: %v", err)
					}
				}
				switch tc.parent {
				case "custom":
					child := filepath.Join(custom, rel)
					info, err := os.Lstat(child)
					if tc.own == "" {
						if !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("added %s inside the linked folder: %v", child, err)
						}
						break
					}
					if err != nil || !info.IsDir() || readTestFile(t, filepath.Join(child, "manifest")) != tc.own {
						t.Fatalf("linked folder's %s was changed: %v, %v", child, info, err)
					}
					if matches, _ := filepath.Glob(child + ".before-sharing-*"); len(matches) != 0 {
						t.Fatalf("backups inside the linked folder: %v", matches)
					}
				case "regular":
					if tc.own == "" {
						if _, err := os.Lstat(filepath.Join(native, link.item)); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("created %s with nothing to share: %v", link.item, err)
						}
						break
					}
					if got, err := os.Readlink(filepath.Join(native, link.item)); err != nil || got != target {
						t.Fatalf("regular copy link = %q, %v", got, err)
					}
					if readTestFile(t, filepath.Join(native, link.item+".before-sharing-stamp", "manifest")) != "custom" {
						t.Fatal("lost the differing copy")
					}
				}
			})
		}
	}
}

// A shared folder on another file system cannot take a profile's files, so
// the profile keeps its own folder, as before sharing moved anything.
func TestShareConfigKeepsProfileFolderAcrossFileSystems(t *testing.T) {
	home, err := os.MkdirTemp("/dev/shm", "codator-home-")
	if err != nil {
		t.Skip("no second file system:", err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	dataHome := tempDataHome(t)
	homeInfo, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	dataInfo, err := os.Stat(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if homeInfo.Sys().(*syscall.Stat_t).Dev == dataInfo.Sys().(*syscall.Stat_t).Dev {
		t.Skip("no second file system")
	}
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("claude", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	own := filepath.Join(account.NativeDir, "skills", "own", "SKILL.md")
	writeTestFile(t, own, "own", time.Now())
	for launch := 0; launch < 2; launch++ {
		if err := shareConfig(context.Background(), store, "claude", io.Discard); err == nil {
			t.Fatalf("launch %d shared skills across file systems", launch)
		}
		if got := readTestFile(t, own); got != "own" {
			t.Fatalf("launch %d: profile skill = %q", launch, got)
		}
		if backups, _ := filepath.Glob(filepath.Join(account.NativeDir, "skills.before-sharing-*")); len(backups) != 0 {
			t.Fatalf("launch %d: backups = %v", launch, backups)
		}
	}
}
