package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type mcpLoginOptions struct {
	server  string
	timeout time.Duration
	scopes  []string
}

func parseMCPInvocation(args []string) (invocation, error) {
	if len(args) == 1 && args[0] == "share" {
		return invocation{verb: "mcp-share", provider: "codex"}, nil
	}
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") ||
		len(args) == 2 && (args[0] == "login" || args[0] == "share") && (args[1] == "--help" || args[1] == "-h") {
		return invocation{verb: "help"}, nil
	}
	if len(args) < 2 || args[0] != "login" || strings.HasPrefix(args[1], "-") ||
		args[1] == "" || strings.ContainsFunc(args[1], func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return invocation{}, fmt.Errorf("%w: expected mcp share or mcp login SERVER [--account NAME] [--timeout 30m] [--scopes SCOPE,...]", errUsage)
	}
	options := &mcpLoginOptions{server: args[1]}
	flags := flag.NewFlagSet("mcp login", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	account := flags.String("account", "", "Codator profile")
	scopes := flags.String("scopes", "", "comma-separated OAuth scopes")
	flags.DurationVar(&options.timeout, "timeout", 30*time.Minute, "OAuth callback timeout")
	if err := flags.Parse(args[2:]); errors.Is(err, flag.ErrHelp) {
		return invocation{verb: "help"}, nil
	} else if err != nil || flags.NArg() != 0 {
		return invocation{}, fmt.Errorf("%w: invalid MCP login options", errUsage)
	}
	accountSet := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "account" {
			accountSet = true
		}
	})
	if accountSet && !validLabel(*account) {
		return invocation{}, fmt.Errorf("%w: --account needs a valid profile name", errUsage)
	}
	if options.timeout < time.Second || options.timeout > 24*time.Hour || options.timeout%time.Second != 0 {
		return invocation{}, fmt.Errorf("%w: --timeout must be whole seconds between 1s and 24h", errUsage)
	}
	if *scopes != "" {
		for _, scope := range strings.Split(*scopes, ",") {
			scope = strings.TrimSpace(scope)
			if scope == "" || strings.ContainsFunc(scope, func(r rune) bool { return r <= ' ' || r >= 127 || r == '"' || r == '\\' }) {
				return invocation{}, fmt.Errorf("%w: --scopes needs comma-separated OAuth scope names", errUsage)
			}
			options.scopes = append(options.scopes, scope)
		}
	}
	return invocation{verb: "mcp-login", provider: "codex", account: *account, mcp: options}, nil
}

// Without sharing, MCP configuration belongs to a profile. Quota selection is
// unnecessary for OAuth, which starts no model turn.
func mcpLoginAccount(store *Store, name, inheritedHome string) (Account, error) {
	accounts, err := store.Accounts("codex")
	if err != nil {
		return Account{}, err
	}
	for _, account := range accounts {
		matches := name != "" && account.Name == name
		if name == "" && inheritedHome != "" {
			matches = account.NativeDir != "" && filepath.Clean(inheritedHome) == account.NativeDir
		}
		if name == "" && inheritedHome == "" && len(accounts) == 1 {
			matches = true
		}
		if matches {
			if account.Err != nil {
				return Account{}, errors.New("selected profile has an unsafe native directory")
			}
			return account, nil
		}
	}
	if name != "" {
		return Account{}, fmt.Errorf("no codex account named %q", name)
	}
	return Account{}, errors.New("choose a Codator profile with --account NAME, or run MCP login inside that profile's Codex session")
}

func loginMCP(store *Store, inv invocation, input io.Reader, out io.Writer) error {
	signals := newProbeSignalScope()
	defer signals.stopListening()
	return loginMCPContext(signals.ctx, store, inv, input, out)
}

func loginMCPContext(ctx context.Context, store *Store, inv invocation, input io.Reader, out io.Writer) error {
	shared, err := syncSharedMCP(ctx, store, false, out)
	if err != nil {
		return err
	}
	account, err := mcpLoginAccount(store, inv.account, os.Getenv("CODEX_HOME"))
	if err != nil && shared && inv.account == "" {
		// All profiles now have the same MCP configuration and native keyring.
		accounts, listErr := store.Accounts("codex")
		if listErr == nil && len(accounts) > 0 && accounts[0].Err == nil {
			account, err = accounts[0], nil
		}
	}
	if err != nil {
		return err
	}
	path, err := findNative("codex")
	if err != nil {
		return err
	}
	lock, err := store.Lock("codex", account.Name)
	if err != nil {
		return err
	}
	defer lock.Close()
	if _, err := fmt.Fprintf(out, "codator: MCP login for %s using codex account %s\n", inv.mcp.server, account.Name); err != nil {
		return err
	}
	if shared {
		fmt.Fprintln(out, "MCP credentials are shared by all Codator Codex accounts through the native keyring.")
	}
	return runMCPLogin(ctx, path, account, *inv.mcp, input, out)
}

type mcpMessage struct {
	rpcResponse
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpEvent struct {
	message mcpMessage
	err     error
}

func readMCPEvents(ctx context.Context, input io.Reader) <-chan mcpEvent {
	events := make(chan mcpEvent)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 1<<20)
		for scanner.Scan() {
			var event mcpEvent
			if json.Unmarshal(scanner.Bytes(), &event.message) != nil {
				event.err = errors.New("Codex app-server returned invalid JSON")
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
			if event.err != nil {
				return
			}
		}
		if scanner.Err() != nil {
			select {
			case events <- mcpEvent{err: errors.New("Codex app-server output was too large or unreadable")}:
			case <-ctx.Done():
			}
		}
	}()
	return events
}

func mcpResponse(message mcpMessage) (json.RawMessage, error) {
	if len(message.Error) != 0 && string(message.Error) != "null" {
		// Provider errors can include codes or tokens. Only expose the RPC code.
		var rpcError struct{ Code int }
		_ = json.Unmarshal(message.Error, &rpcError)
		return nil, fmt.Errorf("Codex rejected the MCP login request (RPC %d); check the server is configured in this profile and update Codex if needed", rpcError.Code)
	}
	if len(message.Result) == 0 || string(message.Result) == "null" {
		return nil, errors.New("Codex app-server returned an empty response")
	}
	return message.Result, nil
}

func waitMCPInitialize(ctx context.Context, events <-chan mcpEvent, home string) error {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return errors.New("Codex app-server initialization timed out")
		case event, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return errors.New("Codex app-server closed before initialization")
			}
			if event.err != nil {
				return event.err
			}
			if string(event.message.ID) != "1" {
				continue
			}
			result, err := mcpResponse(event.message)
			if err != nil {
				return err
			}
			var initialized struct{ CodexHome string }
			if json.Unmarshal(result, &initialized) != nil || initialized.CodexHome != home {
				return errors.New("Codex app-server did not confirm the selected profile; update Codex if needed")
			}
			return nil
		}
	}
}

func runMCPLogin(parent context.Context, path string, account Account, options mcpLoginOptions, input io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	cmd := probeCommand(ctx, path, "app-server", "--listen", "stdio://")
	cmd.Env = codexEnv(account.NativeDir, os.Environ())
	// Keep the caller's working directory so trusted project MCP config applies.
	// No thread or model turn is created and no quota probe is needed.
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return errors.New("cannot read Codex app-server output")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return errors.New("cannot write Codex app-server input")
	}
	defer stdin.Close()
	if err := cmd.Start(); err != nil {
		return errors.New("cannot start Codex app-server for MCP login")
	}
	defer func() {
		cancel()
		finishProbe(cmd)
	}()
	events := readMCPEvents(ctx, stdout)
	encoder := json.NewEncoder(stdin)
	if err := sendRPC(encoder, "initialize", 1, map[string]any{
		"clientInfo":   map[string]string{"name": "codator-mcp-login", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}); err != nil {
		return errors.New("cannot initialize Codex app-server")
	}
	if err := waitMCPInitialize(ctx, events, account.NativeDir); err != nil {
		return err
	}
	if err := encoder.Encode(rpcRequest{Method: "initialized"}); err != nil {
		return errors.New("cannot finish Codex app-server initialization")
	}
	params := map[string]any{"name": options.server, "timeoutSecs": int64(options.timeout / time.Second)}
	if len(options.scopes) > 0 {
		params["scopes"] = options.scopes
	}
	if err := sendRPC(encoder, "mcpServer/oauth/login", 2, params); err != nil {
		return errors.New("cannot start Codex MCP OAuth login")
	}
	return waitMCPOAuth(ctx, events, options, input, out)
}

type mcpCallbackInput struct {
	url string
	err error
}

func readMCPCallbacks(ctx context.Context, input io.Reader) <-chan mcpCallbackInput {
	callbacks := make(chan mcpCallbackInput)
	go func() {
		defer close(callbacks)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 4096), 64<<10)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			select {
			case callbacks <- mcpCallbackInput{url: line}:
			case <-ctx.Done():
				return
			}
		}
		if scanner.Err() != nil {
			select {
			case callbacks <- mcpCallbackInput{err: errors.New("callback input was too long or unreadable; complete the login through the browser")}:
			case <-ctx.Done():
			}
		}
	}()
	return callbacks
}

func waitMCPOAuth(ctx context.Context, events <-chan mcpEvent, options mcpLoginOptions, input io.Reader, out io.Writer) error {
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	var authorizationURL string
	var callbacks <-chan mcpCallbackInput
	var completion *bool
	for {
		// A fast browser callback can race the login request's response. Retain
		// the completion until both it and the authorization response arrive.
		if completion != nil && authorizationURL != "" {
			if !*completion {
				return errors.New("Codex reported MCP OAuth failure; start a fresh login and check the provider's consent and scopes")
			}
			_, err := fmt.Fprintln(out, "OAuth login completed and credentials saved by Codex. Reconnect the MCP server or restart the Codex session to load its tools. Tool access still depends on the provider's permissions.")
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			if authorizationURL == "" {
				return errors.New("Codex did not return an MCP authorization URL within one minute")
			}
			return errors.New("MCP OAuth login timed out; start a new login for a fresh authorization URL")
		case callback, ok := <-callbacks:
			if !ok {
				// A closed stdin must not stop a normal browser or SSH-forwarded callback.
				callbacks = nil
				continue
			}
			if callback.err != nil {
				fmt.Fprintln(out, callback.err)
				continue
			}
			if err := relayMCPCallback(ctx, authorizationURL, callback.url); err != nil {
				fmt.Fprintln(out, "Callback not accepted:", err)
				continue
			}
			fmt.Fprintln(out, "Callback delivered; waiting for Codex to confirm login.")
		case event, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
					return err
				}
				return errors.New("Codex app-server closed before confirming MCP login")
			}
			if event.err != nil {
				return event.err
			}
			message := event.message
			if string(message.ID) == "2" && authorizationURL == "" {
				result, err := mcpResponse(message)
				if err != nil {
					return err
				}
				var login struct{ AuthorizationURL string }
				if json.Unmarshal(result, &login) != nil {
					return errors.New("Codex returned an invalid MCP login response")
				}
				u, err := url.Parse(login.AuthorizationURL)
				if err != nil || u.Host == "" || u.User != nil ||
					strings.ContainsFunc(login.AuthorizationURL, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) ||
					(u.Scheme != "https" && !mcpLoopbackURL(u)) {
					return errors.New("Codex returned an invalid MCP authorization URL")
				}
				authorizationURL = login.AuthorizationURL
				if _, err := fmt.Fprintf(out, "Open this URL to authorize %s:\n%s\n\nOAuth callback timeout: %s. Keep this command running.\nIf your browser is on another machine, paste its full callback URL here.\n", options.server, authorizationURL, options.timeout); err != nil {
					return err
				}
				// The native timeout begins before this response; allow time for its
				// completion notification before applying our final process bound.
				timer.Reset(options.timeout + 10*time.Second)
				callbacks = readMCPCallbacks(ctx, input)
			} else if message.Method == "mcpServer/oauthLogin/completed" {
				var done struct {
					Name     string
					ThreadID *string
					Success  *bool
				}
				if json.Unmarshal(message.Params, &done) != nil {
					return errors.New("Codex returned an invalid OAuth completion notification")
				}
				if done.Name == options.server && done.ThreadID == nil {
					if done.Success == nil {
						return errors.New("Codex did not confirm the OAuth result")
					}
					completion = done.Success
				}
			}
		}
	}
}

// Relay only the matching callback to Codex's own loopback listener. Codex owns
// PKCE, the token exchange, and credential persistence. Never follow redirects
// or use an HTTP proxy when sending a callback containing an authorization code.
func relayMCPCallback(ctx context.Context, authorizationURL, callback string) error {
	auth, err := url.Parse(authorizationURL)
	if err != nil {
		return errors.New("authorization URL is invalid")
	}
	authQuery, err := url.ParseQuery(auth.RawQuery)
	if err != nil || len(authQuery["redirect_uri"]) != 1 || len(authQuery["state"]) != 1 || authQuery.Get("state") == "" {
		return errors.New("authorization URL does not support a pasted callback; complete it through the browser")
	}
	expected, err := url.Parse(authQuery.Get("redirect_uri"))
	if err != nil || !mcpLoopbackURL(expected) {
		return errors.New("this login uses an external callback; complete it through the browser")
	}
	actual, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || !mcpLoopbackURL(actual) || actual.Host != expected.Host || actual.EscapedPath() != expected.EscapedPath() {
		return errors.New("callback does not match this login's listener and path; paste the full URL from the current login")
	}
	query, err := url.ParseQuery(actual.RawQuery)
	if err != nil || len(query["state"]) != 1 || query.Get("state") != authQuery.Get("state") {
		return errors.New("callback belongs to a different or expired login; use the current authorization URL")
	}
	expectedQuery, err := url.ParseQuery(expected.RawQuery)
	if err != nil {
		return errors.New("configured callback query is invalid")
	}
	for key, values := range expectedQuery {
		if len(query[key]) != len(values) {
			return errors.New("callback does not match the configured query")
		}
		for i, value := range values {
			if query[key][i] != value {
				return errors.New("callback does not match the configured query")
			}
		}
	}
	hasCode := len(query["code"]) == 1 && query.Get("code") != ""
	hasError := len(query["error"]) == 1 && query.Get("error") != ""
	if hasCode == hasError || len(query["code"]) > 1 || len(query["error"]) > 1 {
		return errors.New("callback is missing the OAuth result; paste the full URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, actual.String(), nil)
	if err != nil {
		return errors.New("cannot create callback request")
	}
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("could not reach this login's callback listener; it may have expired")
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Errorf("Codex callback listener rejected the callback (HTTP %d)", response.StatusCode)
	}
	return nil
}

func mcpLoopbackURL(u *url.URL) bool {
	if u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "localhost", "::1":
		return true
	default:
		return false
	}
}
