package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// A model hint only narrows quota checks. Any unobserved override must leave
// the hint unknown so that all model limits still apply.
func launchClaudeModel(provider string, args []string, account Account) claudeModelFamily {
	if provider != "claude" || claudeModelRemapped() {
		return ""
	}
	if model := claudeModelHint(args); model != "" {
		return model
	}
	if claudeModelHintWithFallback(args, "implicit") != "implicit" {
		return ""
	}
	if claudeProjectOrManagedSettings() {
		return ""
	}
	name, ok := claudeSavedModel(account.NativeDir)
	if !ok {
		return ""
	}
	if envModel := os.Getenv("ANTHROPIC_MODEL"); envModel != "" {
		name = envModel
	}
	if name == "" {
		name = os.Getenv("ANTHROPIC_DEFAULT_MODEL")
	}
	model, _ := claudeModelFamilyForName(name)
	return model
}

func claudeModelRemapped() bool {
	for _, item := range os.Environ() {
		key, _, ok := cutEnv(item)
		if !ok {
			continue
		}
		if (strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_")) &&
			strings.Contains(key, "MODEL") && key != "ANTHROPIC_MODEL" && key != "ANTHROPIC_DEFAULT_MODEL" {
			return true
		}
	}
	return false
}

func claudeSavedModel(nativeDir string) (string, bool) {
	data, _, ok := readClaudeModelSettings(filepath.Join(nativeDir, "settings.json"))
	if !ok {
		return "", false
	}
	if data == nil {
		return "", true
	}
	var settings map[string]json.RawMessage
	if json.Unmarshal(data, &settings) != nil || settings == nil {
		return "", false
	}
	raw := settings["model"]
	delete(settings, "model")
	if claudeSettingsChangeModel(settings) {
		return "", false
	}
	var name string
	if len(raw) != 0 && json.Unmarshal(raw, &name) != nil {
		return "", false
	}
	return name, true
}

func claudeProjectOrManagedSettings() bool {
	cwd, err := os.Getwd()
	if err != nil {
		return true
	}
	paths, err := exec.Command("git", "rev-parse", "--show-toplevel", "--git-common-dir").Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(paths)), "\n")
		if len(lines) != 2 || !filepath.IsAbs(lines[0]) {
			return true
		}
		if claudeSettingsAt(lines[0]) {
			return true
		}
		// A linked worktree can also read the main checkout's local settings.
		gitDir := lines[1]
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(cwd, gitDir)
		}
		if claudeSettingsAt(filepath.Dir(filepath.Clean(gitDir))) {
			return true
		}
	} else {
		// Without a git root, stop before the user's global ~/.claude scope.
		home, _ := os.UserHomeDir()
		for dir := cwd; ; dir = filepath.Dir(dir) {
			if dir == home {
				break
			}
			if claudeSettingsAt(dir) {
				return true
			}
			if _, statErr := os.Lstat(filepath.Join(dir, ".git")); statErr == nil {
				return true
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	for _, path := range []string{"/etc/claude-code/managed-settings.json", "/Library/Application Support/ClaudeCode/managed-settings.json"} {
		if claudeSettingsOverride(path) {
			return true
		}
	}
	return false
}

func claudeSettingsAt(dir string) bool {
	for _, name := range []string{"settings.json", "settings.local.json"} {
		if claudeSettingsOverride(filepath.Join(dir, ".claude", name)) {
			return true
		}
	}
	return false
}

func claudeSettingsOverride(path string) bool {
	data, present, ok := readClaudeModelSettings(path)
	if !present {
		return false
	}
	if !ok {
		return true
	}
	var settings map[string]json.RawMessage
	return json.Unmarshal(data, &settings) != nil || settings == nil || claudeSettingsChangeModel(settings)
}

func claudeSettingsChangeModel(settings map[string]json.RawMessage) bool {
	for key, raw := range settings {
		if strings.Contains(strings.ToLower(key), "model") {
			return true
		}
		if key == "env" {
			var env map[string]json.RawMessage
			if json.Unmarshal(raw, &env) != nil {
				return true
			}
			for name := range env {
				if strings.Contains(strings.ToUpper(name), "MODEL") {
					return true
				}
			}
		}
	}
	return false
}

func readClaudeModelSettings(path string) ([]byte, bool, bool) {
	info, err := os.Stat(path) // follow profile symlinks, but inspect type before opening
	if os.IsNotExist(err) {
		if _, linkErr := os.Lstat(path); !os.IsNotExist(linkErr) {
			return nil, true, false // broken symlink is unreadable, not absent
		}
		return nil, false, true
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > claudeConfigMaxBytes {
		return nil, true, false
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, true, false
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		return nil, true, false
	}
	data, err := io.ReadAll(io.LimitReader(file, claudeConfigMaxBytes+1))
	return data, true, err == nil && len(data) <= claudeConfigMaxBytes
}
