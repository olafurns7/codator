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
