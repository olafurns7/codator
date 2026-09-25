package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"syscall"
	"time"
)

const (
	mcpSharedFile  = "codex-mcp-shared.json"
	mcpSharedTemp  = ".codex-mcp-shared.tmp"
	mcpSharedLock  = ".codex-mcp-shared.lock"
	mcpConfigLimit = 2 << 20
)

type mcpServers map[string]json.RawMessage

type mcpSharedProfile struct {
	Fingerprint string     `json:"fingerprint"`
	Servers     mcpServers `json:"servers"`
	Store       string     `json:"store"`
}

type mcpSharedState struct {
	Version  int                         `json:"version"`
	Servers  mcpServers                  `json:"servers"`
	Profiles map[string]mcpSharedProfile `json:"profiles"`
}

type mcpUserConfig struct {
	mcpSharedProfile
	Version string
	Data    []byte
}

// Use the native configuration API as the TOML parser/editor. It preserves
// unrelated settings and supports compare-and-swap using the layer version.
// Only the base user layer is shared; project, system, and session layers stay
// local to their existing scope.
type mcpConfigClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	encoder *json.Encoder
	events  <-chan mcpEvent
	ctx     context.Context
	cancel  context.CancelFunc
	nextID  int
}

func startMCPConfigClient(parent context.Context, path string, account Account) (*mcpConfigClient, error) {
	ctx, cancel := context.WithCancel(parent)
	cmd := probeCommand(ctx, path, "app-server", "--listen", "stdio://")
	cmd.Env, cmd.Dir = codexProbeEnv(account.NativeDir), account.NativeDir
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot read Codex configuration process")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, errors.New("cannot write Codex configuration process")
	}
	if err := cmd.Start(); err != nil {
		cancel()
		stdin.Close()
		return nil, errors.New("cannot start Codex configuration process")
	}
	client := &mcpConfigClient{cmd: cmd, stdin: stdin, encoder: json.NewEncoder(stdin), events: readMCPEvents(ctx, stdout), ctx: ctx, cancel: cancel, nextID: 1}
	err = sendRPC(client.encoder, "initialize", 1, map[string]any{
		"clientInfo":   map[string]string{"name": "codator-mcp-sharing", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true, "requestAttestation": false},
	})
	if err == nil {
		err = waitMCPInitialize(ctx, client.events, account.NativeDir)
	}
	if err == nil {
		err = client.encoder.Encode(rpcRequest{Method: "initialized"})
	}
	if err != nil {
		client.close()
		return nil, errors.New("Codex configuration handshake failed; update Codex if needed")
	}
	return client, nil
}

func (c *mcpConfigClient) close() {
	c.cancel()
	c.stdin.Close()
	finishProbe(c.cmd)
}

func (c *mcpConfigClient) request(method string, params any, result any) error {
	c.nextID++
	if err := sendRPC(c.encoder, method, c.nextID, params); err != nil {
		return errors.New("cannot send Codex configuration request")
	}
	for {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case event, ok := <-c.events:
			if !ok {
				return errors.New("Codex configuration process closed before responding")
			}
			if event.err != nil {
				return event.err
			}
			if string(event.message.ID) != strconv.Itoa(c.nextID) {
				continue
			}
			if len(event.message.Error) > 0 && string(event.message.Error) != "null" {
				var detail struct{ Code int }
				_ = json.Unmarshal(event.message.Error, &detail)
				return fmt.Errorf("Codex configuration request failed (RPC %d); configuration may have changed concurrently", detail.Code)
			}
			if json.Unmarshal(event.message.Result, result) != nil {
				return errors.New("Codex returned an invalid configuration response")
			}
			return nil
		}
	}
}

func (c *mcpConfigClient) readUserConfig(dir *os.Root, account Account) (mcpUserConfig, error) {
	before, fingerprint, err := readMCPConfigFile(dir)
	if err != nil {
		return mcpUserConfig{}, err
	}
	raw, version, err := c.userLayer(account)
	if err != nil {
		return mcpUserConfig{}, err
	}
	_, after, err := readMCPConfigFile(dir)
	if err != nil || fingerprint != after {
		return mcpUserConfig{}, errors.New("Codex config changed while being read; retry the command")
	}
	var config struct {
		Servers mcpServers `json:"mcp_servers"`
		Store   string     `json:"mcp_oauth_credentials_store"`
	}
	if len(raw) != 0 && json.Unmarshal(raw, &config) != nil {
		return mcpUserConfig{}, errors.New("Codex returned an invalid configuration response")
	}
	if config.Servers == nil {
		config.Servers = mcpServers{}
	}
	return mcpUserConfig{mcpSharedProfile: mcpSharedProfile{Fingerprint: fingerprint, Servers: config.Servers, Store: config.Store}, Version: version, Data: before}, nil
}

// userLayer returns the profile's own config.toml as Codex parsed it.
func (c *mcpConfigClient) userLayer(account Account) (json.RawMessage, string, error) {
	var response struct {
		Layers []struct {
			Name struct {
				Type, File string
				Profile    *string
			}
			Version        string
			Config         json.RawMessage
			DisabledReason *string
		}
	}
	if err := c.request("config/read", map[string]bool{"includeLayers": true}, &response); err != nil {
		return nil, "", err
	}
	for _, layer := range response.Layers {
		if layer.Name.Type == "user" && layer.Name.Profile == nil && layer.Name.File == filepath.Join(account.NativeDir, "config.toml") {
			if layer.Version == "" || layer.DisabledReason != nil {
				return nil, "", errors.New("Codex user configuration is unavailable for sharing")
			}
			return layer.Config, layer.Version, nil
		}
	}
	return nil, "", errors.New("Codex did not return the selected profile's user configuration")
}

func (c *mcpConfigClient) writeUserMCP(config mcpUserConfig, servers mcpServers) error {
	var response json.RawMessage
	return c.request("config/batchWrite", map[string]any{
		"expectedVersion": config.Version,
		"edits": []map[string]any{
			{"keyPath": "mcp_servers", "value": servers, "mergeStrategy": "replace"},
			{"keyPath": "mcp_oauth_credentials_store", "value": "keyring", "mergeStrategy": "replace"},
		},
	}, &response)
}

func sameMCPValue(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	decode := func(data []byte) (any, error) {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		err := decoder.Decode(&value)
		return value, err
	}
	av, ae := decode(a)
	bv, be := decode(b)
	return ae == nil && be == nil && reflect.DeepEqual(av, bv)
}

func sameMCPServers(a, b mcpServers) bool {
	if len(a) != len(b) {
		return false
	}
	for name, value := range a {
		if !sameMCPValue(value, b[name]) {
			return false
		}
	}
	return true
}

// Compare each profile to its last synchronized value, rather than choosing the
// newest whole file. An unrelated model/trust edit cannot resurrect an old MCP
// entry, and a deletion in one profile propagates instead of being re-imported.
func mergeSharedMCP(previous mcpSharedState, current map[string]mcpUserConfig) (mcpSharedState, error) {
	next := mcpSharedState{Version: 1, Servers: mcpServers{}, Profiles: map[string]mcpSharedProfile{}}
	for name, value := range previous.Servers {
		next.Servers[name] = value
	}
	changes := mcpServers{}
	owners := map[string]string{}
	names := make([]string, 0, len(current))
	for name := range current {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		config := current[name]
		next.Profiles[name] = config.mcpSharedProfile
		old, known := previous.Profiles[name]
		keys := map[string]bool{}
		for key := range config.Servers {
			keys[key] = true
		}
		if known {
			for key := range old.Servers {
				keys[key] = true
			}
		}
		for key := range keys {
			value := config.Servers[key]
			if known && sameMCPValue(old.Servers[key], value) || sameMCPValue(previous.Servers[key], value) {
				continue
			}
			if owner, changed := owners[key]; changed && !sameMCPValue(changes[key], value) {
				return mcpSharedState{}, fmt.Errorf("conflicting MCP edits for %q in accounts %q and %q; align those server entries before retrying", key, owner, name)
			}
			// A newly enrolled profile must not replace an existing shared server
			// with an unrelated account or endpoint under the same name.
			if !known && len(previous.Servers[key]) != 0 && !sameMCPValue(previous.Servers[key], value) {
				return mcpSharedState{}, fmt.Errorf("new profile %q conflicts with shared MCP server %q; align its server entry before retrying", name, key)
			}
			owners[key], changes[key] = name, value
		}
	}
	for name, value := range changes {
		if len(value) == 0 {
			delete(next.Servers, name)
		} else {
			next.Servers[name] = value
		}
	}
	return next, nil
}

func readMCPConfigFile(dir *os.Root) ([]byte, string, error) {
	file, err := openMCPConfigFile(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing", nil
	}
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mcpConfigLimit+1))
	if err != nil || len(data) > mcpConfigLimit {
		return nil, "", errors.New("Codex config is too large or unreadable")
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

// A linked config.toml is shared from ~/.codex (see shareConfig) and is read
// through the link, as native Codex does. A profile's own copy stays private.
func openMCPConfigFile(dir *os.Root) (*os.File, error) {
	if !mcpConfigLinked(dir) {
		file, err := openPrivateFile(dir, "config.toml", os.O_RDONLY, 0600)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("Codex config must be a private regular file")
		}
		return file, err
	}
	file, err := os.OpenFile(filepath.Join(dir.Name(), "config.toml"), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, errors.New("shared Codex config must be a regular file")
	}
	return file, nil
}

func mcpConfigLinked(dir *os.Root) bool {
	info, err := dir.Lstat("config.toml")
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// Profiles linked to one shared config.toml synchronize as a single entry,
// keyed by its path. Account labels never contain a path separator.
func mcpConfigKey(dir *os.Root, account string) string {
	if !mcpConfigLinked(dir) {
		return account
	}
	if path, err := filepath.EvalSymlinks(filepath.Join(dir.Name(), "config.toml")); err == nil {
		return path
	}
	return account
}

func readSharedMCP(root *os.Root) (mcpSharedState, bool, error) {
	file, err := openPrivateFile(root, mcpSharedFile, os.O_RDONLY, 0600)
	if errors.Is(err, os.ErrNotExist) {
		return mcpSharedState{}, false, nil
	}
	if err != nil {
		return mcpSharedState{}, false, errors.New("shared MCP state must be a private regular file")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, mcpConfigLimit+1))
	if err != nil || len(data) > mcpConfigLimit {
		return mcpSharedState{}, false, errors.New("shared MCP state is too large or unreadable")
	}
	var state mcpSharedState
	if json.Unmarshal(data, &state) != nil || state.Version != 1 || state.Servers == nil || state.Profiles == nil {
		return mcpSharedState{}, false, errors.New("shared MCP state is malformed or from an unsupported version")
	}
	return state, true, nil
}

func writeSharedMCP(root *os.Root, state mcpSharedState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil || len(data)+1 > mcpConfigLimit {
		return errors.New("shared MCP state is too large to save")
	}
	file, err := openPrivateFile(root, mcpSharedTemp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	defer root.Remove(mcpSharedTemp)
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if info, err := root.Lstat(mcpSharedFile); err == nil {
		if err := checkPrivateFile(info, 0600); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return root.Rename(mcpSharedTemp, mcpSharedFile)
}

func lockStoreFile(ctx context.Context, root *os.Root, name string) (*AccountLock, error) {
	file, err := openPrivateFile(root, name, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &AccountLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func backupMCPConfig(dir *os.Root, data []byte) error {
	if data == nil {
		return nil
	}
	const name = "config.toml.before-mcp-sharing"
	if info, err := dir.Lstat(name); err == nil {
		return checkPrivateFile(info, 0600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := openPrivateFile(dir, name, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// Sharing is opt-in. Unchanged configs take the hash-only path, with no native
// subprocess, network request, or credential access on ordinary launches.
func syncSharedMCP(parent context.Context, store *Store, enable bool, out io.Writer) (bool, error) {
	root, err := os.OpenRoot(store.root)
	if err != nil {
		return false, err
	}
	defer root.Close()
	if !enable {
		if _, err := root.Lstat(mcpSharedFile); errors.Is(err, os.ErrNotExist) {
			return false, nil
		} else if err != nil {
			return false, err
		}
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	lock, err := lockStoreFile(ctx, root, mcpSharedLock)
	if err != nil {
		return false, fmt.Errorf("lock shared MCP configuration: %w", err)
	}
	defer lock.Close()
	previous, exists, err := readSharedMCP(root)
	if err != nil {
		return false, err
	}
	if !exists && !enable {
		return false, nil
	}
	accounts, err := store.Accounts("codex")
	if err != nil {
		return false, err
	}
	if len(accounts) == 0 {
		return false, errors.New("enroll a Codex account before enabling MCP sharing")
	}
	dirs := map[string]*os.Root{}
	clients := map[string]*mcpConfigClient{}
	defer func() {
		for _, client := range clients {
			client.close()
		}
		for _, dir := range dirs {
			dir.Close()
		}
	}()
	clientFor := func(account Account) (*mcpConfigClient, error) {
		if client := clients[account.Name]; client != nil {
			return client, nil
		}
		path, err := findNative("codex")
		if err != nil {
			return nil, err
		}
		client, err := startMCPConfigClient(ctx, path, account)
		if err != nil {
			return nil, err
		}
		clients[account.Name] = client
		return client, nil
	}
	current := map[string]mcpUserConfig{}
	keys := map[string]string{}
	dirty := enable
	for _, account := range accounts {
		if account.Err != nil {
			return false, fmt.Errorf("cannot share MCP settings with unsafe profile %q", account.Name)
		}
		accountDir, err := store.accountRoot("codex", account.Name, false)
		if err != nil {
			return false, err
		}
		dir, err := openSubRoot(accountDir, "native", false)
		accountDir.Close()
		if err != nil {
			return false, err
		}
		dirs[account.Name] = dir
		key := mcpConfigKey(dir, account.Name)
		keys[account.Name] = key
		if _, seen := current[key]; seen {
			continue
		}
		_, fingerprint, err := readMCPConfigFile(dir)
		if err != nil {
			return false, fmt.Errorf("account %q: %w", account.Name, err)
		}
		if cached, ok := previous.Profiles[key]; ok && cached.Fingerprint == fingerprint {
			current[key] = mcpUserConfig{mcpSharedProfile: cached}
			if cached.Store != "keyring" || !sameMCPServers(cached.Servers, previous.Servers) {
				dirty = true
			}
			continue
		}
		dirty = true
		client, err := clientFor(account)
		if err != nil {
			return false, err
		}
		config, err := client.readUserConfig(dir, account)
		if err != nil {
			return false, fmt.Errorf("read MCP config for %q: %w", account.Name, err)
		}
		current[key] = config
	}
	if !dirty && len(current) == len(previous.Profiles) {
		return true, nil
	}
	for _, account := range accounts {
		if current[keys[account.Name]].Store != "keyring" {
			if _, err := dirs[account.Name].Lstat(".credentials.json"); err == nil {
				return false, fmt.Errorf("account %q has file-based MCP credentials; configure and sign in with the native keyring before enabling sharing", account.Name)
			} else if !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
	}
	next, err := mergeSharedMCP(previous, current)
	if err != nil {
		return false, err
	}
	// Record accepted edits before broadcasting, so an interrupted write can be
	// retried without treating a stale profile as the new source of truth.
	if err := writeSharedMCP(root, next); err != nil {
		return false, err
	}
	updated := 0
	synced := map[string]bool{}
	for _, account := range accounts {
		key := keys[account.Name]
		if synced[key] {
			continue
		}
		synced[key] = true
		config := current[key]
		if config.Store == "keyring" && sameMCPServers(config.Servers, next.Servers) {
			continue
		}
		profileLock, err := store.Lock("codex", account.Name)
		if err != nil {
			return true, fmt.Errorf("cannot synchronize MCP settings for %q: %w; retry after its guarded operation finishes", account.Name, err)
		}
		// Release every profile lock even when the native editor rejects a stale
		// version or a profile is being edited concurrently outside Codator.
		err = func() error {
			defer profileLock.Close()
			dir := dirs[account.Name]
			client, err := clientFor(account)
			if err != nil {
				return err
			}
			fresh, err := client.readUserConfig(dir, account)
			if err != nil {
				return err
			}
			if fresh.Fingerprint != config.Fingerprint {
				return errors.New("profile config changed concurrently; retry the command")
			}
			if fresh.Store != "keyring" {
				if _, err := dir.Lstat(".credentials.json"); err == nil {
					return errors.New("profile has MCP credentials in a file; configure and sign in with the native keyring before enabling sharing")
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if err := backupMCPConfig(dir, fresh.Data); err != nil {
				return err
			}
			if err := client.writeUserMCP(fresh, next.Servers); err != nil {
				return err
			}
			written, err := client.readUserConfig(dir, account)
			if err != nil {
				return err
			}
			if written.Store != "keyring" || !sameMCPServers(written.Servers, next.Servers) {
				return errors.New("native Codex did not retain the shared MCP settings")
			}
			next.Profiles[key] = written.mcpSharedProfile
			updated++
			return nil
		}()
		if err != nil {
			return true, fmt.Errorf("synchronize MCP settings for %q: %w", account.Name, err)
		}
	}
	if err := writeSharedMCP(root, next); err != nil {
		return true, err
	}
	if enable || updated > 0 {
		_, err = fmt.Fprintf(out, "codator: MCP settings and keyring logins shared across %d Codex profiles\n", len(accounts))
	}
	return true, err
}
