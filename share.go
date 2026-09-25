package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// User configuration that every profile shares through the native default
// directory, ~/.claude or ~/.codex. Credentials, account state, sessions, and
// history stay in each profile.
// ponytail: plugin installs stay per profile because their manifests record
// absolute profile paths; sharing them needs a path-aware migration.
var sharedConfigItems = map[string][]string{
	"claude": {"settings.json", "CLAUDE.md", "keybindings.json", "agents", "commands", "output-styles", "routines", "rules", "skills", "themes", "workflows"},
	"codex":  {"config.toml", "AGENTS.md", "AGENTS.override.md", "hooks.json", "prompts", "rules", "skills", "themes"},
}

const configShareLock = ".config-share.lock"

// Sharing never blocks a session: an item that cannot be shared stays in the
// profile and the next launch retries it.
func shareConfigForSession(ctx context.Context, store *Store, provider string, out io.Writer) error {
	err := shareConfig(ctx, store, provider, out)
	if err == nil {
		return nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	fmt.Fprintf(out, "codator: warning: some %s configuration is not shared (%v)\n", provider, err)
	return nil
}

// shareConfig links each profile's configuration items to the shared
// directory. The native CLIs write through these links. A profile's own copy
// is moved there when nothing is shared yet (the newest copy wins), merged into
// a shared directory, or saved beside the profile when it differs, so no
// configuration is discarded.
func shareConfig(ctx context.Context, store *Store, provider string, out io.Writer) error {
	home := os.Getenv("HOME")
	if !filepath.IsAbs(home) {
		return errors.New("HOME must be an absolute path")
	}
	shared := filepath.Join(home, "."+provider)
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return err
	}
	defer root.Close()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	lock, err := lockStoreFile(ctx, root, configShareLock)
	if err != nil {
		return err
	}
	defer lock.Close()
	accounts, err := store.Accounts(provider)
	if err != nil {
		return err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	var errs []error
	for _, name := range sharedConfigItems[provider] {
		if err := shareConfigItem(ctx, provider, accounts, name, filepath.Join(shared, name), stamp, out); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func shareConfigItem(ctx context.Context, provider string, accounts []Account, name, target, stamp string, out io.Writer) error {
	codexConfig := provider == "codex" && name == "config.toml"
	if codexConfig && restrictsCodexLogin(target) {
		return fmt.Errorf("not shared because %s restricts login and Codex signs out accounts that do not match", target)
	}
	type profileCopy struct {
		account  Account
		path     string
		modified time.Time
	}
	var copies []profileCopy
	for _, account := range accounts {
		if account.Err != nil {
			continue
		}
		path := filepath.Join(account.NativeDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, fs.ErrNotExist) || err == nil && info.Mode()&fs.ModeSymlink != 0 {
			continue // missing, already shared, or linked elsewhere on purpose
		}
		if err != nil {
			return err
		}
		if codexConfig && restrictsCodexLogin(path) {
			continue // belongs to this account
		}
		copies = append(copies, profileCopy{account, path, info.ModTime()})
	}
	sort.SliceStable(copies, func(i, j int) bool { return copies[i].modified.After(copies[j].modified) })
	for _, c := range copies {
		_, err := os.Lstat(target)
		seed := errors.Is(err, fs.ErrNotExist)
		if seed {
			err = os.MkdirAll(filepath.Dir(target), 0700)
		}
		if err != nil {
			return err
		}
		kept, err := absorb(c.path, target)
		if err != nil {
			return err
		}
		if seed {
			fmt.Fprintf(out, "codator: moved %s account %s's %s to %s to share it with every account\n", provider, c.account.Name, name, target)
		}
		if !kept {
			continue
		}
		backup := c.path + ".before-sharing-" + stamp
		if _, err := os.Lstat(backup); err == nil {
			return fmt.Errorf("%s already exists", backup)
		}
		note := ""
		if codexConfig {
			mergeErr, err := keepCodexConfig(ctx, c.account, backup, target)
			if err != nil {
				return err
			}
			if mergeErr == nil {
				fmt.Fprintf(out, "codator: merged %s account %s's %s into %s; its original is saved as %s\n", provider, c.account.Name, name, target, backup)
				continue
			}
			note = fmt.Sprintf(" (not merged: %v)", mergeErr)
		} else if err := os.Rename(c.path, backup); err != nil {
			return err
		}
		fmt.Fprintf(out, "codator: %s account %s now uses the shared %s; its differing copy is saved as %s%s\n", provider, c.account.Name, target, backup, note)
	}
	if _, err := os.Lstat(target); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, account := range accounts {
		if account.Err != nil {
			continue
		}
		path := filepath.Join(account.NativeDir, name)
		if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
			if err := os.Symlink(target, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// absorb moves what dst lacks from src into dst and drops what dst already
// holds identically. It reports whether src still holds anything that differs.
func absorb(src, dst string) (bool, error) {
	info, err := os.Lstat(src)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(dst); errors.Is(err, fs.ErrNotExist) {
		return false, os.Rename(src, dst)
	} else if err != nil {
		return false, err
	}
	shared, err := os.Stat(dst) // the shared directory may itself be a link
	if err != nil {
		return true, nil
	}
	switch {
	case info.Mode().IsRegular() && shared.Mode().IsRegular():
		if info.Size() != shared.Size() {
			return true, nil
		}
		left, err := os.ReadFile(src)
		if err != nil {
			return false, err
		}
		right, err := os.ReadFile(dst)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(left, right) {
			return true, nil
		}
		return false, os.Remove(src)
	case info.IsDir() && shared.IsDir():
		entries, err := os.ReadDir(src)
		if err != nil {
			return false, err
		}
		kept := false
		for _, entry := range entries {
			left, err := absorb(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name()))
			if err != nil {
				return false, err
			}
			kept = kept || left
		}
		if kept {
			return true, nil
		}
		return false, os.Remove(src)
	default:
		return true, nil
	}
}

// Codex signs out, and revokes, credentials that do not match a login
// restriction, so a config.toml that sets one is never shared.
func restrictsCodexLogin(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && (bytes.Contains(data, []byte("forced_login_method")) || bytes.Contains(data, []byte("forced_chatgpt_workspace_id")))
}

// keepCodexConfig saves a profile's own config.toml, links the shared one, and
// adds what the shared config lacks: trusted folders, MCP servers, and the hook
// trust Codex records under each profile's path. The native editor parses and
// writes the TOML. It returns why the merge failed; the saved copy still holds
// every setting.
func keepCodexConfig(ctx context.Context, account Account, backup, target string) (mergeErr, err error) {
	var client *mcpConfigClient
	var own map[string]any
	native, mergeErr := findNative("codex")
	if mergeErr == nil {
		client, mergeErr = startMCPConfigClient(ctx, native, account)
	}
	if mergeErr == nil {
		defer client.close()
		own, _, mergeErr = codexUserConfig(client, account)
	}
	path := filepath.Join(account.NativeDir, "config.toml")
	if err := os.Rename(path, backup); err != nil {
		return nil, err
	}
	if err := os.Symlink(target, path); err != nil {
		return nil, err
	}
	if mergeErr != nil {
		return mergeErr, nil
	}
	shared, version, mergeErr := codexUserConfig(client, account)
	if mergeErr != nil {
		return mergeErr, nil
	}
	var edits []map[string]any
	for key, value := range own {
		if merged := mergeMissing(shared[key], value); !sameJSON(merged, shared[key]) {
			edits = append(edits, map[string]any{"keyPath": key, "value": merged, "mergeStrategy": "replace"})
		}
	}
	if len(edits) == 0 {
		return nil, nil
	}
	var response json.RawMessage
	return client.request("config/batchWrite", map[string]any{"expectedVersion": version, "edits": edits}, &response), nil
}

func codexUserConfig(client *mcpConfigClient, account Account) (map[string]any, string, error) {
	raw, version, err := client.userLayer(account)
	if err != nil {
		return nil, "", err
	}
	config := map[string]any{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber() // keep integers exact
	if len(raw) != 0 && string(raw) != "null" && decoder.Decode(&config) != nil {
		return nil, "", errors.New("Codex returned an invalid configuration")
	}
	return config, version, nil
}

// mergeMissing adds what shared lacks from own, recursing into tables. A value
// present in both keeps the shared one.
func mergeMissing(shared, own any) any {
	if shared == nil {
		return own
	}
	sharedTable, ok := shared.(map[string]any)
	ownTable, ownOK := own.(map[string]any)
	if !ok || !ownOK {
		return shared
	}
	merged := make(map[string]any, len(sharedTable)+len(ownTable))
	for key, value := range sharedTable {
		merged[key] = value
	}
	for key, value := range ownTable {
		merged[key] = mergeMissing(sharedTable[key], value)
	}
	return merged
}

func sameJSON(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
