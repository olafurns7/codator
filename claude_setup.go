package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	claudeConfigMaxBytes = 1 << 20
	claudeConfigTemp     = ".codator-onboarding.tmp"
)

var claudeAuthStatusTimeout = 5 * time.Second

type claudeSetupConfig struct {
	name   string
	data   []byte
	values map[string]json.RawMessage
}

// recoverClaudeSetup handles one narrowly-scoped upstream migration. It never
// creates a config and it leaves all non-candidate cases to native Claude.
func recoverClaudeSetup(ctx context.Context, store *Store, account Account) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	config, err := store.claudeSetupCandidate(account)
	if err != nil || config == nil {
		return err
	}
	if err := claudeAuthStatus(ctx, account); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Read again after the bounded native command. Do not replace an updated
	// config or add the flag when the candidate condition no longer holds.
	config, err = store.claudeSetupCandidate(account)
	if err != nil || config == nil {
		return err
	}
	config.values["hasCompletedOnboarding"] = json.RawMessage("true")
	data, err := json.Marshal(config.values)
	if err != nil || len(data) > claudeConfigMaxBytes {
		return errors.New("Claude config is too large to update safely")
	}
	dir, err := store.claudeNativeRoot(account)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := replaceClaudeSetupFile(dir, config.name, config.data, data); err != nil {
		return fmt.Errorf("cannot atomically update Claude setup state: %w", err)
	}
	fmt.Fprintf(os.Stderr, "codator: recovered Claude setup state for account %s\n", account.Name)
	return nil
}

func (s *Store) claudeSetupCandidate(account Account) (*claudeSetupConfig, error) {
	config, err := s.readClaudeConfig(account)
	if err != nil || config == nil {
		return nil, err
	}
	oauth, hasOAuth := config.values["oauthAccount"]
	if !hasOAuth || isClaudeJSONNull(oauth) {
		return nil, nil
	}
	if _, complete := config.values["hasCompletedOnboarding"]; complete {
		return nil, nil
	}
	return config, nil
}

// claudeEmail returns the signed-in address Claude records in its config.
func (s *Store) claudeEmail(account Account) string {
	config, err := s.readClaudeConfig(account)
	if err != nil || config == nil {
		return ""
	}
	var oauth struct {
		EmailAddress string `json:"emailAddress"`
	}
	json.Unmarshal(config.values["oauthAccount"], &oauth)
	return oauth.EmailAddress
}

// claudeLoginMaxMillis is 9999-12-31T23:59:59.999Z, the last time RFC 3339 can express.
const claudeLoginMaxMillis = 253402300799999

// claudeLoginExpiry returns when Claude Code recorded that the account's login
// ends. It decodes that one timestamp and nothing else from the credentials
// file. Anything missing or malformed is unknown: macOS keeps the login in the
// keychain, which Codator does not read.
func (s *Store) claudeLoginExpiry(account Account) (time.Time, bool) {
	dir, err := s.claudeNativeRoot(account)
	if err != nil {
		return time.Time{}, false
	}
	defer dir.Close()
	file, err := openPrivateFile(dir, ".credentials.json", os.O_RDONLY, 0600)
	if err != nil {
		return time.Time{}, false
	}
	data, err := io.ReadAll(io.LimitReader(file, claudeConfigMaxBytes+1))
	file.Close()
	if err != nil || len(data) > claudeConfigMaxBytes {
		return time.Time{}, false
	}
	var credentials struct {
		OAuth struct {
			RefreshTokenExpiresAt *float64 `json:"refreshTokenExpiresAt"`
		} `json:"claudeAiOauth"`
	}
	if json.Unmarshal(data, &credentials) != nil {
		return time.Time{}, false
	}
	millis := credentials.OAuth.RefreshTokenExpiresAt
	if millis == nil || !(*millis >= 1 && *millis <= claudeLoginMaxMillis) {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(*millis)).UTC(), true
}

// claudeLoginWarnDays is when Claude Code itself starts warning about the login.
const claudeLoginWarnDays = 3

// loginDaysLeft rounds up like Claude Code does; zero or less means expired.
func loginDaysLeft(expires, now time.Time) int {
	// Milliseconds, not a Duration: that saturates near 292 years.
	return int(math.Ceil(float64(expires.UnixMilli()-now.UnixMilli()) / 86400000))
}

// loginExpiryText words the time left and reports whether it deserves a warning.
func loginExpiryText(expires, now time.Time) (string, bool) {
	days := loginDaysLeft(expires, now)
	switch {
	case days <= 0:
		return "login expired", true
	case days == 1:
		return "login expires in 1 day", true
	}
	return fmt.Sprintf("login expires in %d days", days), days <= claudeLoginWarnDays
}

func (s *Store) readClaudeConfig(account Account) (*claudeSetupConfig, error) {
	dir, err := s.claudeNativeRoot(account)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	name, exists, err := claudeConfigName(dir)
	if err != nil || !exists {
		return nil, err
	}
	info, err := dir.Lstat(name)
	if err != nil {
		return nil, err
	}
	if err := checkPrivateFile(info, 0600); err != nil {
		return nil, fmt.Errorf("unsafe Claude config file: %w", err)
	}
	file, err := openPrivateFile(dir, name, os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, claudeConfigMaxBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > claudeConfigMaxBytes {
		return nil, errors.New("Claude config is too large")
	}
	values := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&values); err != nil || values == nil {
		return nil, errors.New("Claude config is malformed")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("Claude config is malformed")
	}
	return &claudeSetupConfig{name: name, data: data, values: values}, nil
}

func (s *Store) claudeNativeRoot(account Account) (*os.Root, error) {
	if account.Provider != "claude" || account.Err != nil || !filepath.IsAbs(account.NativeDir) {
		return nil, errors.New("Claude account profile is unavailable")
	}
	root, err := s.accountRoot(account.Provider, account.Name, false)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return openSubRoot(root, "native", false)
}

func claudeConfigName(dir *os.Root) (string, bool, error) {
	for _, name := range []string{".config.json", ".claude.json"} {
		_, err := dir.Lstat(name)
		if err == nil {
			return name, true, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", false, err
		}
	}
	return "", false, nil
}

func claudeAuthStatus(parent context.Context, account Account) error {
	path, err := findNative("claude")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, claudeAuthStatusTimeout)
	defer cancel()
	output := &limitedProbeOutput{limit: claudeConfigMaxBytes}
	cmd := probeCommand(ctx, path, "auth", "status", "--json")
	cmd.Dir, cmd.Env = account.NativeDir, claudeProbeEnv(account.NativeDir)
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return errors.New("cannot create isolated Claude auth status output")
	}
	cmd.Stdout = stdoutW
	if err := cmd.Start(); err != nil {
		stdoutR.Close()
		stdoutW.Close()
		return errors.New("cannot start isolated Claude auth status")
	}
	stdoutW.Close()
	stopReader := context.AfterFunc(ctx, func() { _ = stdoutR.Close() })
	defer stopReader()
	readDone := make(chan error, 1)
	go func() {
		_, err := io.Copy(output, stdoutR)
		_ = stdoutR.Close()
		readDone <- err
	}()
	if err := waitProbe(cmd); err != nil {
		<-readDone
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("isolated Claude auth status failed")
	}
	if err := <-readDone; err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("isolated Claude auth status output is unreadable")
	}
	if output.exceeded {
		return errors.New("isolated Claude auth status is too large")
	}
	var status struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		APIProvider      string `json:"apiProvider"`
		SubscriptionType string `json:"subscriptionType"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	if err := decoder.Decode(&status); err != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("isolated Claude auth status is malformed")
	}
	if !status.LoggedIn || status.AuthMethod != "claude.ai" || status.APIProvider != "firstParty" || !knownClaudeSubscription(strings.ToLower(status.SubscriptionType)) {
		return errors.New("isolated Claude auth status did not confirm a Claude.ai first-party subscription")
	}
	return nil
}

func replaceClaudeSetupFile(dir *os.Root, name string, expected, data []byte) error {
	if err := checkPrivateFileContents(dir, name, expected); err != nil {
		return err
	}
	if info, err := dir.Lstat(claudeConfigTemp); err == nil {
		if err := checkPrivateFile(info, 0600); err != nil {
			return fmt.Errorf("unsafe Claude config temporary file: %w", err)
		}
		if err := dir.Remove(claudeConfigTemp); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := dir.OpenFile(claudeConfigTemp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	created := true
	defer func() {
		if created {
			_ = dir.Remove(claudeConfigTemp)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if err := checkPrivateFileContents(dir, name, expected); err != nil {
		return err
	}
	if err := dir.Rename(claudeConfigTemp, name); err != nil {
		return err
	}
	created = false
	directory, err := dir.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func checkPrivateFileContents(dir *os.Root, name string, expected []byte) error {
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return errors.New("Claude config disappeared before recovery completed")
	}
	if err != nil {
		return err
	}
	if err := checkPrivateFile(info, 0600); err != nil {
		return fmt.Errorf("unsafe Claude config file: %w", err)
	}
	file, err := openPrivateFile(dir, name, os.O_RDONLY, 0600)
	if err != nil {
		return err
	}
	actual, readErr := io.ReadAll(io.LimitReader(file, int64(len(expected))+1))
	closeErr := file.Close()
	if readErr != nil {
		return readErr
	}
	if closeErr != nil {
		return closeErr
	}
	if !bytes.Equal(actual, expected) {
		return errors.New("Claude config changed before recovery completed")
	}
	return nil
}

type limitedProbeOutput struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (w *limitedProbeOutput) Write(data []byte) (int, error) {
	if len(data) > w.limit-w.Len() {
		available := w.limit - w.Len()
		if available > 0 {
			_, _ = w.Buffer.Write(data[:available])
		}
		w.exceeded = true
		return len(data), nil
	}
	return w.Buffer.Write(data)
}
