package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestMCPLoginOptions(t *testing.T) {
	if inv, err := parseInvocation([]string{"mcp", "share"}); err != nil || inv.verb != "mcp-share" || inv.provider != "codex" {
		t.Fatalf("sharing = %+v, %v", inv, err)
	}
	inv, err := parseInvocation([]string{"mcp", "login", "posthog"})
	if err != nil || inv.mcp == nil || inv.mcp.timeout != 30*time.Minute || inv.mcp.scopes != nil || inv.account != "" {
		t.Fatalf("defaults = %+v, %v", inv, err)
	}
	inv, err = parseInvocation([]string{"mcp", "login", "posthog", "--account", "work", "--timeout", "10m", "--scopes", "openid,user:read"})
	if err != nil || inv.account != "work" || inv.mcp.timeout != 10*time.Minute || !reflect.DeepEqual(inv.mcp.scopes, []string{"openid", "user:read"}) {
		t.Fatalf("options = %+v, %v", inv, err)
	}
	for _, args := range [][]string{
		{"mcp"}, {"mcp", "login"}, {"mcp", "logout", "posthog"},
		{"mcp", "share", "unexpected"},
		{"mcp", "login", "posthog", "--account", "../outside"},
		{"mcp", "login", "posthog", "--account", ""},
		{"mcp", "login", "posthog", "--timeout", "0s"},
		{"mcp", "login", "posthog", "--timeout", "1500ms"},
		{"mcp", "login", "posthog", "--timeout", "25h"},
		{"mcp", "login", "posthog", "--scopes", "user:read,,openid"},
		{"mcp", "login", "posthog", "--scopes", "invalid scope"},
		{"mcp", "login", "posthog", "unexpected"},
	} {
		if _, err := parseInvocation(args); !errors.Is(err, errUsage) {
			t.Errorf("accepted invalid arguments %q: %v", args, err)
		}
	}
	for _, args := range [][]string{{"mcp", "--help"}, {"mcp", "share", "--help"}, {"mcp", "login", "--help"}, {"mcp", "login", "posthog", "--help"}} {
		if inv, err := parseInvocation(args); err != nil || inv.verb != "help" {
			t.Errorf("help %q: %+v, %v", args, inv, err)
		}
	}
}

func TestMCPLoginAccountDoesNotRotateProfiles(t *testing.T) {
	store, err := newStore(tempDataHome(t))
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.EnsureAccount("codex", "first")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := mcpLoginAccount(store, "", ""); err != nil || got.Name != first.Name {
		t.Fatalf("single account: %+v, %v", got, err)
	}
	second, err := store.EnsureAccount("codex", "second")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, home, want string }{
		{"", second.NativeDir, "second"},
		{"first", second.NativeDir, "first"},
		{"", "", ""},
		{"", filepath.Join(store.root, "unrelated"), ""},
		{"missing", first.NativeDir, ""},
	} {
		got, err := mcpLoginAccount(store, test.name, test.home)
		if test.want == "" && err == nil || test.want != "" && (err != nil || got.Name != test.want) {
			t.Errorf("selection %+v: %+v, %v", test, got, err)
		}
	}
	if err := os.Chmod(second.NativeDir, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpLoginAccount(store, "second", ""); err == nil {
		t.Fatal("accepted unsafe profile")
	}
}

func TestMCPCallbackRelayChecksDestinationAndState(t *testing.T) {
	var redirects atomic.Int32
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirects.Add(1)
	}))
	defer elsewhere.Close()
	var received atomic.Int32
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received.Add(1)
		if r.URL.Query().Get("code") != "synthetic+code&value" || r.URL.Query().Get("iss") != "https://issuer.example.test/" {
			t.Error("OAuth query was changed before reaching Codex")
		}
		http.Redirect(w, r, elsewhere.URL+"/?code=synthetic", http.StatusFound)
	}))
	defer listener.Close()
	t.Setenv("HTTP_PROXY", elsewhere.URL)
	t.Setenv("ALL_PROXY", elsewhere.URL)
	t.Setenv("NO_PROXY", "")
	redirect := listener.URL + "/callback/server?fixed=value"
	auth := "https://issuer.example.test/authorize?" + url.Values{"redirect_uri": {redirect}, "state": {"state+with&chars"}}.Encode()
	callback := listener.URL + "/callback/server?" + url.Values{
		"fixed": {"value"}, "code": {"synthetic+code&value"}, "state": {"state+with&chars"}, "iss": {"https://issuer.example.test/"},
	}.Encode()
	if err := relayMCPCallback(context.Background(), auth, callback); err != nil {
		t.Fatal(err)
	}
	if received.Load() != 1 || redirects.Load() != 0 {
		t.Fatalf("callback deliveries=%d, followed redirects/proxies=%d", received.Load(), redirects.Load())
	}
	for _, invalid := range []string{
		strings.Replace(callback, "state%2Bwith%26chars", "stale-state", 1),
		strings.Replace(callback, "/callback/server", "/callback/another", 1),
		strings.Replace(callback, listener.URL, elsewhere.URL, 1),
		strings.Replace(callback, "http://", "https://", 1),
		strings.Replace(callback, "fixed=value", "fixed=changed", 1),
		callback + "&state=duplicate", callback + "#fragment",
		"http://example.test/callback?code=synthetic&state=state",
	} {
		if err := relayMCPCallback(context.Background(), auth, invalid); err == nil || strings.Contains(err.Error(), "synthetic") {
			t.Errorf("invalid callback accepted or echoed: %v", err)
		}
	}
	if received.Load() != 1 || redirects.Load() != 0 {
		t.Fatal("sent an invalid callback")
	}
}

// A fake native app-server runs as a separate process and owns the callback
// listener and credential write, just as Codex does. It rejects quota/model RPCs.
func TestMCPNativeChild(t *testing.T) {
	if os.Getenv("CODATOR_MCP_STUB_CHILD") != "1" {
		return
	}
	os.Exit(runMCPNativeStub())
}

func runMCPNativeStub() int {
	if len(os.Args) < 5 || !reflect.DeepEqual(os.Args[len(os.Args)-3:], []string{"app-server", "--listen", "stdio://"}) {
		return 91
	}
	encoder := json.NewEncoder(os.Stdout)
	scenario := os.Getenv("CODATOR_MCP_SCENARIO")
	var outputMu sync.Mutex
	write := func(value any) {
		outputMu.Lock()
		defer outputMu.Unlock()
		_ = encoder.Encode(value)
	}
	complete := func(success bool) {
		write(map[string]any{"method": "mcpServer/oauthLogin/completed", "params": map[string]any{
			"name": "synthetic", "success": success, "error": "provider detail with synthetic-callback-code",
		}})
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var request struct {
			Method string
			ID     int
			Params map[string]any
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return 92
		}
		switch request.Method {
		case "initialize":
			home := os.Getenv("CODEX_HOME")
			if scenario == "wrong-profile" {
				home += "/wrong"
			}
			write(map[string]any{"id": request.ID, "result": map[string]any{"codexHome": home}})
		case "initialized":
		case "mcpServer/oauth/login":
			cwd, _ := os.Getwd()
			record, _ := json.Marshal(map[string]any{
				"params": request.Params, "cwd": cwd, "home": os.Getenv("CODEX_HOME"),
				"sqliteHome": os.Getenv("CODEX_SQLITE_HOME"), "ambientAPIKey": os.Getenv("OPENAI_API_KEY"), "pid": os.Getpid(),
			})
			if os.WriteFile(os.Getenv("CODATOR_MCP_RECORD"), record, 0600) != nil {
				return 93
			}
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/callback/synthetic" || r.URL.Query().Get("state") != "synthetic-state" || r.URL.Query().Get("code") != "synthetic-callback-code" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if scenario != "oauth-failure" {
					_ = os.WriteFile(filepath.Join(os.Getenv("CODEX_HOME"), "synthetic-credential"), []byte("saved by native stub"), 0600)
				}
				complete(scenario != "oauth-failure")
				fmt.Fprintln(w, "Callback received")
			}))
			defer listener.Close()
			if scenario == "early-completion" {
				complete(true)
			}
			write(map[string]any{"id": request.ID, "result": map[string]any{"authorizationUrl": "https://oauth.example.test/authorize?" + url.Values{
				"redirect_uri": {listener.URL + "/callback/synthetic"}, "state": {"synthetic-state"},
			}.Encode()}})
		default:
			return 94
		}
	}
	return 0
}

type mcpTestOutput struct {
	mu   sync.Mutex
	text bytes.Buffer
	urls chan string
}

func (o *mcpTestOutput) Write(data []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	n, err := o.text.Write(data)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "https://oauth.example.test/") {
			select {
			case o.urls <- line:
			default:
			}
		}
	}
	return n, err
}

func (o *mcpTestOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.text.String()
}

func TestMCPLoginEndToEnd(t *testing.T) {
	for _, scenario := range []string{"success", "shared-no-account", "explicit-scopes", "oauth-failure", "wrong-profile", "cancel", "early-completion"} {
		t.Run(scenario, func(t *testing.T) {
			store, err := newStore(tempDataHome(t))
			if err != nil {
				t.Fatal(err)
			}
			account, err := store.EnsureAccount("codex", "work")
			if err != nil {
				t.Fatal(err)
			}
			bin := tempDataHome(t)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			script := "#!/bin/sh\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestMCPNativeChild$ -- \"$@\"\n"
			if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			record := filepath.Join(bin, "request.json")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("CODEX_HOME", account.NativeDir)
			t.Setenv("OPENAI_API_KEY", "synthetic-do-not-forward")
			t.Setenv("CODATOR_MCP_RECORD", record)
			t.Setenv("CODATOR_MCP_STUB_CHILD", "1")
			t.Setenv("CODATOR_MCP_SCENARIO", scenario)
			if scenario == "shared-no-account" {
				second, err := store.EnsureAccount("codex", "z-other")
				if err != nil {
					t.Fatal(err)
				}
				state := mcpSharedState{Version: 1, Servers: mcpServers{}, Profiles: map[string]mcpSharedProfile{}}
				for _, profile := range []Account{account, second} {
					if err := os.WriteFile(filepath.Join(profile.NativeDir, "config.toml"), []byte("mcp_oauth_credentials_store = \"keyring\"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					dir, err := os.OpenRoot(profile.NativeDir)
					if err != nil {
						t.Fatal(err)
					}
					_, fingerprint, err := readMCPConfigFile(dir)
					dir.Close()
					if err != nil {
						t.Fatal(err)
					}
					state.Profiles[profile.Name] = mcpSharedProfile{Fingerprint: fingerprint, Servers: mcpServers{}, Store: "keyring"}
				}
				root, err := os.OpenRoot(store.root)
				if err != nil {
					t.Fatal(err)
				}
				err = writeSharedMCP(root, state)
				root.Close()
				if err != nil {
					t.Fatal(err)
				}
				t.Setenv("CODEX_HOME", "")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			input, callbackWriter := io.Pipe()
			defer input.Close()
			defer callbackWriter.Close()
			out := &mcpTestOutput{urls: make(chan string, 1)}
			args := []string{"mcp", "login", "synthetic"}
			if scenario == "explicit-scopes" {
				args = append(args, "--account", "work", "--timeout", "2m", "--scopes", "openid,user:read")
				t.Setenv("CODEX_HOME", "/unrelated-home")
			}
			inv, err := parseInvocation(args)
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() { result <- loginMCPContext(ctx, store, inv, input, out) }()
			if scenario == "wrong-profile" {
				if err := <-result; err == nil || !strings.Contains(err.Error(), "selected profile") {
					t.Fatalf("wrong profile: %v", err)
				}
				if _, err := os.Stat(record); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("started OAuth in wrong profile")
				}
				return
			}
			var authorizationURL string
			select {
			case authorizationURL = <-out.urls:
			case err := <-result:
				t.Fatalf("login ended before authorization URL: %v", err)
			case <-ctx.Done():
				t.Fatal("no authorization URL")
			}
			if scenario != "early-completion" {
				if lock, err := store.Lock("codex", "work"); !errors.Is(err, ErrAccountBusy) {
					if lock != nil {
						lock.Close()
					}
					t.Fatalf("OAuth did not retain profile lock: %v", err)
				}
			}
			switch scenario {
			case "cancel":
				cancel()
			case "early-completion":
				callbackWriter.Close()
			default:
				auth, _ := url.Parse(authorizationURL)
				callback := auth.Query().Get("redirect_uri") + "?code=synthetic-callback-code&state=synthetic-state"
				// Reject a stale callback, then allow the correct callback in the same login.
				fmt.Fprintln(callbackWriter, strings.Replace(callback, "synthetic-state", "stale-state", 1))
				fmt.Fprintln(callbackWriter, callback)
			}
			err = <-result
			if scenario == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
			} else if scenario == "oauth-failure" {
				if err == nil || !strings.Contains(err.Error(), "OAuth failure") {
					t.Fatalf("provider rejection: %v", err)
				}
			} else if err != nil || !strings.Contains(out.String(), "OAuth login completed") {
				t.Fatalf("login: %v; output: %s", err, out.String())
			}
			if strings.Contains(out.String(), "synthetic-callback-code") || (err != nil && strings.Contains(err.Error(), "synthetic-callback-code")) {
				t.Fatal("echoed callback code or provider error")
			}
			lock, lockErr := store.Lock("codex", "work")
			if lockErr != nil {
				t.Fatalf("retained lock after login: %v", lockErr)
			}
			lock.Close()
			data, err := os.ReadFile(record)
			if err != nil {
				t.Fatal(err)
			}
			var request struct {
				Params        map[string]any
				Cwd, Home     string
				SQLiteHome    string
				AmbientAPIKey string
				PID           int
			}
			if err := json.Unmarshal(data, &request); err != nil {
				t.Fatal(err)
			}
			cwd, _ := os.Getwd()
			if request.Home != account.NativeDir || request.SQLiteHome != account.NativeDir || request.Cwd != cwd || request.AmbientAPIKey != "" {
				t.Fatalf("native context: %+v", request)
			}
			if request.Params["timeoutSecs"] != float64(inv.mcp.timeout/time.Second) {
				t.Fatalf("native timeout: %+v", request.Params)
			}
			if scenario == "explicit-scopes" {
				if !reflect.DeepEqual(request.Params["scopes"], []any{"openid", "user:read"}) {
					t.Fatalf("native scopes: %+v", request.Params)
				}
			} else if _, exists := request.Params["scopes"]; exists {
				t.Fatal("overrode native default scopes")
			}
			if scenario == "success" || scenario == "shared-no-account" || scenario == "explicit-scopes" {
				if _, err := os.Stat(filepath.Join(account.NativeDir, "synthetic-credential")); err != nil {
					t.Fatalf("native process did not save credentials: %v", err)
				}
			}
			if err := syscall.Kill(request.PID, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("app-server was not reaped: %v", err)
			}
		})
	}
}
