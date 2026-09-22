package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// This fake native editor treats config bytes as JSON. The wrapper itself never
// parses TOML; a separate smoke check uses the installed Codex TOML editor.
func TestMCPConfigNativeChild(t *testing.T) {
	if os.Getenv("CODATOR_MCP_CONFIG_CHILD") != "1" {
		return
	}
	encoder := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	configPath := filepath.Join(os.Getenv("CODEX_HOME"), "config.toml")
	for scanner.Scan() {
		var request struct {
			ID     int
			Method string
			Params json.RawMessage
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(91)
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"codexHome": os.Getenv("CODEX_HOME")}
		case "initialized":
			continue
		case "config/read", "config/batchWrite":
			data, err := os.ReadFile(configPath)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				os.Exit(92)
			}
			values := map[string]any{}
			if len(data) > 0 && json.Unmarshal(data, &values) != nil {
				os.Exit(93)
			}
			digest := sha256.Sum256(data)
			version := hex.EncodeToString(digest[:])
			if request.Method == "config/read" {
				result = map[string]any{"layers": []any{
					map[string]any{"name": map[string]string{"type": "project", "dotCodexFolder": "/unrelated/.codex"}, "version": "project", "config": map[string]any{"mcp_servers": map[string]any{"project-only": map[string]string{"url": "https://project.example.test"}}}},
					map[string]any{"name": map[string]string{"type": "user", "file": configPath}, "version": version, "config": values},
				}}
			} else {
				var params struct {
					ExpectedVersion string
					Edits           []struct {
						KeyPath, MergeStrategy string
						Value                  any
					}
				}
				if json.Unmarshal(request.Params, &params) != nil {
					os.Exit(94)
				}
				_, reject := os.Stat(filepath.Join(os.Getenv("CODEX_HOME"), "reject-write"))
				if params.ExpectedVersion != version || reject == nil {
					_ = encoder.Encode(map[string]any{"id": request.ID, "error": map[string]any{"code": -32000, "message": "synthetic credential must not be printed"}})
					continue
				}
				if len(params.Edits) != 2 {
					os.Exit(95)
				}
				for _, edit := range params.Edits {
					if edit.MergeStrategy != "replace" || (edit.KeyPath != "mcp_servers" && edit.KeyPath != "mcp_oauth_credentials_store") {
						os.Exit(96)
					}
					values[edit.KeyPath] = edit.Value
				}
				updated, _ := json.Marshal(values)
				if os.WriteFile(configPath, updated, 0600) != nil {
					os.Exit(97)
				}
				result = map[string]string{"status": "ok"}
			}
		default:
			os.Exit(98)
		}
		_ = encoder.Encode(map[string]any{"id": request.ID, "result": result})
	}
	os.Exit(0)
}

func setupMCPSharingTest(t *testing.T) (*Store, []Account) {
	t.Helper()
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	bin := tempDataHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nCODATOR_MCP_CONFIG_CHILD=1 exec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestMCPConfigNativeChild$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var accounts []Account
	for _, name := range []string{"one", "two", "three"} {
		account, err := store.EnsureAccount("codex", name)
		if err != nil {
			t.Fatal(err)
		}
		writeMCPSharingFixture(t, account, map[string]any{"model": name, "mcp_oauth_credentials_store": "keyring"})
		if err := os.WriteFile(filepath.Join(account.NativeDir, "auth.json"), []byte("isolated subscription: "+name), 0600); err != nil {
			t.Fatal(err)
		}
		accounts = append(accounts, account)
	}
	return store, accounts
}

func writeMCPSharingFixture(t *testing.T, account Account, config map[string]any) {
	t.Helper()
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(account.NativeDir, "config.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func readMCPSharingFixture(t *testing.T, account Account) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(account.NativeDir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func runSharedMCPSync(t *testing.T, store *Store, enable bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if enabled, err := syncSharedMCP(ctx, store, enable, io.Discard); err != nil || !enabled {
		t.Fatalf("sync enabled=%v: %v", enabled, err)
	}
}

func TestSharedMCPPropagatesEditsWithoutSharingAccountData(t *testing.T) {
	store, accounts := setupMCPSharingTest(t)
	first := readMCPSharingFixture(t, accounts[0])
	first["mcp_servers"] = map[string]any{"example": map[string]any{"url": "https://mcp.example.test/mcp", "http_headers": map[string]string{"X-Tenant": "same-tenant"}}}
	writeMCPSharingFixture(t, accounts[0], first)
	first = readMCPSharingFixture(t, accounts[0])
	runSharedMCPSync(t, store, true)
	for _, account := range accounts {
		config := readMCPSharingFixture(t, account)
		if !reflect.DeepEqual(config["mcp_servers"], first["mcp_servers"]) || config["model"] != account.Name {
			t.Fatalf("changed unrelated data or missed servers: %+v", config)
		}
		auth, _ := os.ReadFile(filepath.Join(account.NativeDir, "auth.json"))
		if string(auth) != "isolated subscription: "+account.Name {
			t.Fatal("modified subscription credentials")
		}
	}
	// The fast path must work without starting Codex at all.
	originalPath := os.Getenv("PATH")
	t.Setenv("PATH", tempDataHome(t))
	runSharedMCPSync(t, store, false)
	t.Setenv("PATH", originalPath)
	// Updates and deletions in any profile propagate on the next launch.
	second := readMCPSharingFixture(t, accounts[1])
	second["model"] = "only-two"
	second["mcp_servers"].(map[string]any)["example"].(map[string]any)["url"] = "https://changed.example.test/mcp"
	writeMCPSharingFixture(t, accounts[1], second)
	runSharedMCPSync(t, store, false)
	for _, account := range accounts {
		if !reflect.DeepEqual(readMCPSharingFixture(t, account)["mcp_servers"], second["mcp_servers"]) {
			t.Fatal("did not propagate edited endpoint")
		}
	}
	third := readMCPSharingFixture(t, accounts[2])
	delete(third["mcp_servers"].(map[string]any), "example")
	writeMCPSharingFixture(t, accounts[2], third)
	runSharedMCPSync(t, store, false)
	for _, account := range accounts {
		if len(readMCPSharingFixture(t, account)["mcp_servers"].(map[string]any)) != 0 {
			t.Fatal("resurrected deleted MCP entry")
		}
	}
	if readMCPSharingFixture(t, accounts[1])["model"] != "only-two" {
		t.Fatal("overwrote profile-specific model")
	}
	// A newly enrolled account has no config yet and inherits shared MCP setup.
	fourth, err := store.EnsureAccount("codex", "four")
	if err != nil {
		t.Fatal(err)
	}
	runSharedMCPSync(t, store, false)
	if readMCPSharingFixture(t, fourth)["mcp_oauth_credentials_store"] != "keyring" {
		t.Fatal("new account did not use shared keyring")
	}
}

func TestSharedMCPResumesInterruptedBroadcast(t *testing.T) {
	store, accounts := setupMCPSharingTest(t)
	config := readMCPSharingFixture(t, accounts[0])
	config["mcp_servers"] = map[string]any{"new": map[string]any{"url": "https://mcp.example.test/mcp"}}
	writeMCPSharingFixture(t, accounts[0], config)
	reject := filepath.Join(accounts[1].NativeDir, "reject-write")
	if err := os.WriteFile(reject, nil, 0600); err != nil {
		t.Fatal(err)
	}
	_, err := syncSharedMCP(context.Background(), store, true, io.Discard)
	if err == nil || strings.Contains(err.Error(), "synthetic credential") {
		t.Fatalf("unsafe or missing write failure: %v", err)
	}
	if err := os.Remove(reject); err != nil {
		t.Fatal(err)
	}
	runSharedMCPSync(t, store, false)
	for _, account := range accounts {
		if !reflect.DeepEqual(readMCPSharingFixture(t, account)["mcp_servers"], config["mcp_servers"]) {
			t.Fatal("partial broadcast lost accepted MCP entry")
		}
	}
}

func TestSharedMCPRejectsConflictsAndCredentialMigration(t *testing.T) {
	t.Run("conflicting simultaneous edits", func(t *testing.T) {
		store, accounts := setupMCPSharingTest(t)
		runSharedMCPSync(t, store, true)
		for i, account := range accounts[:2] {
			config := readMCPSharingFixture(t, account)
			config["mcp_servers"] = map[string]any{"same-name": map[string]string{"url": fmt.Sprintf("https://server%d.example.test", i)}}
			writeMCPSharingFixture(t, account, config)
		}
		before, _ := os.ReadFile(filepath.Join(accounts[2].NativeDir, "config.toml"))
		if _, err := syncSharedMCP(context.Background(), store, false, io.Discard); err == nil || !strings.Contains(err.Error(), "conflicting MCP") {
			t.Fatalf("conflict: %v", err)
		}
		after, _ := os.ReadFile(filepath.Join(accounts[2].NativeDir, "config.toml"))
		if string(before) != string(after) {
			t.Fatal("changed other profile despite conflict")
		}
	})
	t.Run("file credentials are not silently abandoned", func(t *testing.T) {
		store, accounts := setupMCPSharingTest(t)
		config := readMCPSharingFixture(t, accounts[0])
		config["mcp_oauth_credentials_store"] = "file"
		writeMCPSharingFixture(t, accounts[0], config)
		if err := os.WriteFile(filepath.Join(accounts[0].NativeDir, ".credentials.json"), []byte("synthetic-private-credentials"), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := syncSharedMCP(context.Background(), store, true, io.Discard); err == nil || !strings.Contains(err.Error(), "file-based") {
			t.Fatalf("migration: %v", err)
		}
		if _, err := os.Stat(filepath.Join(store.root, mcpSharedFile)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("enabled sharing after rejected migration")
		}
		if readMCPSharingFixture(t, accounts[0])["mcp_oauth_credentials_store"] != "file" {
			t.Fatal("changed credential store")
		}
	})
	t.Run("opt-in and private config required", func(t *testing.T) {
		store, accounts := setupMCPSharingTest(t)
		t.Setenv("PATH", tempDataHome(t))
		if enabled, err := syncSharedMCP(context.Background(), store, false, io.Discard); enabled || err != nil {
			t.Fatalf("sharing enabled implicitly: %v, %v", enabled, err)
		}
		if err := os.Chmod(filepath.Join(accounts[0].NativeDir, "config.toml"), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := syncSharedMCP(context.Background(), store, true, io.Discard); err == nil || !strings.Contains(err.Error(), "private regular file") {
			t.Fatalf("unsafe config: %v", err)
		}
	})
}

func TestSharedMCPFailureWarnsOrdinarySessionsOnly(t *testing.T) {
	conditions := map[string]func(t *testing.T, store *Store, accounts []Account){
		"busy": func(t *testing.T, store *Store, accounts []Account) {
			config := readMCPSharingFixture(t, accounts[0])
			config["mcp_servers"] = map[string]any{"new": map[string]string{"url": "https://mcp.example.test"}}
			writeMCPSharingFixture(t, accounts[0], config)
			lock, err := store.Lock("codex", accounts[1].Name)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { lock.Close() })
		},
		"unsafe": func(t *testing.T, store *Store, accounts []Account) {
			if err := os.Chmod(accounts[1].NativeDir, 0755); err != nil {
				t.Fatal(err)
			}
		},
		"conflicting": func(t *testing.T, store *Store, accounts []Account) {
			for i, account := range accounts[:2] {
				config := readMCPSharingFixture(t, account)
				config["mcp_servers"] = map[string]any{"same-name": map[string]string{"url": fmt.Sprintf("https://server%d.example.test", i)}}
				writeMCPSharingFixture(t, account, config)
			}
		},
	}
	for name, setup := range conditions {
		t.Run(name, func(t *testing.T) {
			store, accounts := setupMCPSharingTest(t)
			runSharedMCPSync(t, store, true)
			setup(t, store, accounts)
			var out strings.Builder
			if err := syncSharedMCPForSession(context.Background(), store, []string{"-c", `sandbox_mode="danger-full-access"`, "resume", "--last"}, &out); err != nil {
				t.Fatalf("ordinary session stopped: %v", err)
			}
			if !strings.Contains(out.String(), "warning: MCP settings were not shared") {
				t.Fatalf("missing warning: %q", out.String())
			}
			if err := syncSharedMCPForSession(context.Background(), store, []string{"-c", `sandbox_mode="danger-full-access"`, "mcp", "logout", "posthog"}, io.Discard); err == nil {
				t.Fatal("native MCP command proceeded without shared settings")
			}
			if _, err := syncSharedMCP(context.Background(), store, true, io.Discard); err == nil {
				t.Fatal("mcp share accepted a failed synchronization")
			}
			err := loginMCPContext(context.Background(), store, invocation{account: accounts[0].Name, mcp: &mcpLoginOptions{}}, strings.NewReader(""), io.Discard)
			if err == nil {
				t.Fatal("mcp login proceeded without shared settings")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := syncSharedMCPForSession(ctx, store, nil, io.Discard); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation did not stop the session: %v", err)
			}
		})
	}
}
