package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestClaudeSetupRecoveryLaunchesSelectedProfile(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"automatic selection", []string{"claude", "--", "hello"}},
		{"explicit selection", []string{"claude", "--account", "work", "--", "hello"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, store, account, root, bin := newClaudeSetupProfile(t)
			original := `{"oauthAccount":{"token":"fixture","id":7},"number":9007199254740993,"unknown":{"keep":[1,2,3]}}`
			writeClaudeSetupConfig(t, filepath.Join(account.NativeDir, ".claude.json"), original)
			seen, argv, cwd := filepath.Join(root, "seen.json"), filepath.Join(root, "argv"), filepath.Join(root, "cwd")
			writeClaudeSetupLaunchStub(t, filepath.Join(bin, "claude"))
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(test.args)
			if err != nil {
				t.Fatal(err)
			}
			launchDir := filepath.Join(root, "launch")
			if err := os.Mkdir(launchDir, 0700); err != nil {
				t.Fatal(err)
			}
			canonicalLaunchDir, err := filepath.EvalSymlinks(launchDir)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCodatorPassthroughChild$")
			cmd.Dir = launchDir
			cmd.Env = []string{
				"HOME=" + root,
				"PATH=" + bin + ":/usr/bin:/bin",
				"XDG_DATA_HOME=" + dataHome,
				"CODATOR_PASSTHROUGH_CHILD=1",
				"CODATOR_TEST_ARGS=" + string(encoded),
				"CODATOR_SETUP_SEEN=" + seen,
				"CODATOR_SETUP_ARGV=" + argv,
				"CODATOR_SETUP_CWD=" + cwd,
			}
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err = cmd.Run()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 23 {
				t.Fatalf("launch error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
			}
			assertClaudeSetupConfig(t, seen, original, true)
			captured, err := os.ReadFile(argv)
			if err != nil || string(captured) != "--\x00hello\x00" {
				t.Fatalf("native argv=%q err=%v", captured, err)
			}
			gotCWD, err := os.ReadFile(cwd)
			if err != nil || strings.TrimSpace(string(gotCWD)) != canonicalLaunchDir {
				t.Fatalf("native cwd=%q err=%v", gotCWD, err)
			}
			if countClaudeSetupFile(t, account, ".auth-count") != 1 || !strings.Contains(stderr.String(), "recovered Claude setup state") {
				t.Fatalf("auth count=%d stderr=%q", countClaudeSetupFile(t, account, ".auth-count"), stderr.String())
			}
			lock, err := store.Lock("claude", "work")
			if err != nil {
				t.Fatalf("native launch did not release the profile lock: %v", err)
			}
			lock.Close()
		})
	}
}

func TestClaudeSetupRecoveryLeavesNonCandidatesUntouched(t *testing.T) {
	for _, test := range []struct {
		name       string
		config     string
		authOutput string
		wantErr    bool
	}{
		{"already complete", `{"oauthAccount":{"token":"fixture"},"hasCompletedOnboarding":true}`, goodClaudeAuthStatus, false},
		{"explicit false", `{"oauthAccount":{"token":"fixture"},"hasCompletedOnboarding":false}`, goodClaudeAuthStatus, false},
		{"explicit null", `{"oauthAccount":{"token":"fixture"},"hasCompletedOnboarding":null}`, goodClaudeAuthStatus, false},
		{"no saved account", `{"other":1}`, goodClaudeAuthStatus, false},
		{"malformed config", `{`, goodClaudeAuthStatus, true},
		{"unauthenticated status", `{"oauthAccount":{"token":"fixture"}}`, `{"loggedIn":false}`, true},
		{"malformed status", `{"oauthAccount":{"token":"fixture"}}`, `{`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, store, account, _, bin := newClaudeSetupProfile(t)
			config := filepath.Join(account.NativeDir, ".claude.json")
			writeClaudeSetupConfig(t, config, test.config)
			writeClaudeSetupStatusStub(t, filepath.Join(bin, "claude"), test.authOutput, "")
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			lock, err := store.Lock("claude", account.Name)
			if err != nil {
				t.Fatal(err)
			}
			err = recoverClaudeSetup(context.Background(), store, account)
			lock.Close()
			if (err != nil) != test.wantErr {
				t.Fatalf("recovery error=%v, want error=%t", err, test.wantErr)
			}
			got, readErr := os.ReadFile(config)
			if readErr != nil || string(got) != test.config {
				t.Fatalf("config changed to %q, err=%v", got, readErr)
			}
			wantAuth := test.config != "{" && strings.Contains(test.config, "oauthAccount") && !strings.Contains(test.config, "hasCompletedOnboarding")
			if got := countClaudeSetupFile(t, account, ".auth-count"); got != boolToInt(wantAuth) {
				t.Fatalf("auth calls=%d, want %d", got, boolToInt(wantAuth))
			}
		})
	}
}

func TestClaudeSetupRecoveryProtectsConfigAndRechecksCandidate(t *testing.T) {
	t.Run("legacy config takes precedence", func(t *testing.T) {
		_, store, account, _, bin := newClaudeSetupProfile(t)
		legacy := filepath.Join(account.NativeDir, ".claude.json")
		preferred := filepath.Join(account.NativeDir, ".config.json")
		writeClaudeSetupConfig(t, legacy, `{"oauthAccount":{"token":"legacy"},"hasCompletedOnboarding":false}`)
		writeClaudeSetupConfig(t, preferred, `{"oauthAccount":{"token":"preferred"},"number":1}`)
		writeClaudeSetupStatusStub(t, filepath.Join(bin, "claude"), goodClaudeAuthStatus, "")
		t.Setenv("PATH", bin+":/usr/bin:/bin")
		if err := recoverClaudeSetup(context.Background(), store, account); err != nil {
			t.Fatal(err)
		}
		assertClaudeSetupConfig(t, preferred, `{"oauthAccount":{"token":"preferred"},"number":1}`, true)
		gotLegacy, err := os.ReadFile(legacy)
		if err != nil || strings.Contains(string(gotLegacy), `"hasCompletedOnboarding":true`) {
			t.Fatalf("legacy config changed=%q err=%v", gotLegacy, err)
		}
	})
	t.Run("unsafe config is never followed or changed", func(t *testing.T) {
		_, store, account, root, bin := newClaudeSetupProfile(t)
		target := filepath.Join(root, "target.json")
		writeClaudeSetupConfig(t, target, `{"oauthAccount":{"token":"fixture"}}`)
		config := filepath.Join(account.NativeDir, ".claude.json")
		if err := os.Symlink(target, config); err != nil {
			t.Fatal(err)
		}
		writeClaudeSetupStatusStub(t, filepath.Join(bin, "claude"), goodClaudeAuthStatus, "")
		t.Setenv("PATH", bin+":/usr/bin:/bin")
		if err := recoverClaudeSetup(context.Background(), store, account); err == nil {
			t.Fatal("accepted a symlink config")
		}
		if got := countClaudeSetupFile(t, account, ".auth-count"); got != 0 {
			t.Fatalf("unsafe config ran auth status %d times", got)
		}
		if got, err := os.ReadFile(target); err != nil || strings.Contains(string(got), "hasCompletedOnboarding") {
			t.Fatalf("symlink target changed=%q err=%v", got, err)
		}
	})
	t.Run("unsafe mode is not changed", func(t *testing.T) {
		_, store, account, _, bin := newClaudeSetupProfile(t)
		config := filepath.Join(account.NativeDir, ".claude.json")
		original := `{"oauthAccount":{"token":"fixture"}}`
		writeClaudeSetupConfig(t, config, original)
		if err := os.Chmod(config, 0644); err != nil {
			t.Fatal(err)
		}
		writeClaudeSetupStatusStub(t, filepath.Join(bin, "claude"), goodClaudeAuthStatus, "")
		t.Setenv("PATH", bin+":/usr/bin:/bin")
		if err := recoverClaudeSetup(context.Background(), store, account); err == nil {
			t.Fatal("accepted a public config")
		}
		if info, err := os.Stat(config); err != nil || info.Mode().Perm() != 0644 {
			t.Fatalf("config mode changed: info=%v err=%v", info, err)
		}
	})
	t.Run("unexpected owner is not changed", func(t *testing.T) {
		if os.Geteuid() != 0 {
			t.Skip("creating a differently-owned private file requires root")
		}
		_, store, account, _, bin := newClaudeSetupProfile(t)
		config := filepath.Join(account.NativeDir, ".claude.json")
		writeClaudeSetupConfig(t, config, `{"oauthAccount":{"token":"fixture"}}`)
		if err := os.Chown(config, 1, -1); err != nil {
			t.Fatal(err)
		}
		writeClaudeSetupStatusStub(t, filepath.Join(bin, "claude"), goodClaudeAuthStatus, "")
		t.Setenv("PATH", bin+":/usr/bin:/bin")
		if err := recoverClaudeSetup(context.Background(), store, account); err == nil {
			t.Fatal("accepted a config owned by another user")
		}
		if got := countClaudeSetupFile(t, account, ".auth-count"); got != 0 {
			t.Fatalf("unexpected-owner config ran auth status %d times", got)
		}
	})
	t.Run("auth status update wins over migration", func(t *testing.T) {
		_, store, account, _, bin := newClaudeSetupProfile(t)
		config := filepath.Join(account.NativeDir, ".claude.json")
		writeClaudeSetupConfig(t, config, `{"oauthAccount":{"token":"before"},"number":1}`)
		rewrite := `{"oauthAccount":{"token":"after"},"hasCompletedOnboarding":false,"number":2}`
		writeClaudeSetupStatusStub(t, filepath.Join(bin, "claude"), goodClaudeAuthStatus, rewrite)
		t.Setenv("PATH", bin+":/usr/bin:/bin")
		if err := recoverClaudeSetup(context.Background(), store, account); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(config)
		if err != nil || string(got) != rewrite {
			t.Fatalf("auth status update was overwritten: %q err=%v", got, err)
		}
	})
}

func TestClaudeSetupRecoveryCancelsAuthStatusAndItsChildren(t *testing.T) {
	_, store, account, root, bin := newClaudeSetupProfile(t)
	config := filepath.Join(account.NativeDir, ".claude.json")
	original := `{"oauthAccount":{"token":"fixture"}}`
	writeClaudeSetupConfig(t, config, original)
	child := filepath.Join(root, "auth-child")
	writeInstallFixture(t, filepath.Join(bin, "claude"), "#!/bin/sh\nif [ \"$1\" = auth ]; then /bin/sleep 30 & printf '%s' \"$!\" > "+shellQuote(child)+"; wait; fi\n")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	err := recoverClaudeSetup(ctx, store, account)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recovery error=%v, want deadline exceeded", err)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != original {
		t.Fatalf("cancelled auth changed config=%q err=%v", got, err)
	}
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for running, _ := processRunning(pid); running && time.Now().Before(deadline); running, _ = processRunning(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if running, err := processRunning(pid); err != nil || running {
		t.Fatalf("auth descendant is still running=%t err=%v", running, err)
	}
	lock, err := store.Lock("claude", account.Name)
	if err != nil {
		t.Fatalf("recovery leaked account lock: %v", err)
	}
	lock.Close()
}

func TestClaudeSetupRecoveryCleansAuthStatusParentExitChildren(t *testing.T) {
	_, store, account, root, bin := newClaudeSetupProfile(t)
	config := filepath.Join(account.NativeDir, ".claude.json")
	original := `{"oauthAccount":{"token":"fixture"}}`
	writeClaudeSetupConfig(t, config, original)
	child := filepath.Join(root, "auth-child")
	writeInstallFixture(t, filepath.Join(bin, "claude"), "#!/bin/sh\nif [ \"$1\" = auth ]; then /bin/sleep 30 & printf '%s' \"$!\" > "+shellQuote(child)+"; printf '%s\\n' "+shellQuote(goodClaudeAuthStatus)+"; exit 0; fi\n")
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	if err := recoverClaudeSetup(context.Background(), store, account); err != nil {
		t.Fatal(err)
	}
	assertProbeChildStopped(t, child)
	assertClaudeSetupConfig(t, config, original, true)
}

func TestClaudeSetupRecoveryBoundsDetachedAuthStatusStdout(t *testing.T) {
	setsid, err := exec.LookPath("setsid")
	if err != nil {
		t.Skip("setsid is required to create an escaped fixture child")
	}
	_, store, account, _, bin := newClaudeSetupProfile(t)
	config := filepath.Join(account.NativeDir, ".claude.json")
	original := `{"oauthAccount":{"token":"fixture"}}`
	writeClaudeSetupConfig(t, config, original)
	child := filepath.Join(account.NativeDir, ".detached-auth-child")
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then\n" +
		shellQuote(setsid) + " /bin/sh -c 'printf \"%s\" \"$$\" > \"$CLAUDE_CONFIG_DIR/.detached-auth-child\"; exec /bin/sleep 30' &\n" +
		"while [ ! -f \"$CLAUDE_CONFIG_DIR/.detached-auth-child\" ]; do :; done\n" +
		"printf '%s\\n' " + shellQuote(goodClaudeAuthStatus) + "\nexit 0\nfi\n"
	writeInstallFixture(t, filepath.Join(bin, "claude"), script)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	oldTimeout := claudeAuthStatusTimeout
	claudeAuthStatusTimeout = 80 * time.Millisecond
	t.Cleanup(func() { claudeAuthStatusTimeout = oldTimeout })
	started := time.Now()
	err = recoverClaudeSetup(context.Background(), store, account)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("recovery error=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("detached stdout held recovery for %v", elapsed)
	}
	data, err := os.ReadFile(child)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for running, _ := processRunning(pid); running && time.Now().Before(deadline); running, _ = processRunning(pid) {
		time.Sleep(10 * time.Millisecond)
	}
	if running, err := processRunning(pid); err != nil || running {
		t.Fatalf("detached fixture child is still running=%t err=%v", running, err)
	}
	if got, err := os.ReadFile(config); err != nil || string(got) != original {
		t.Fatalf("timed-out auth changed config=%q err=%v", got, err)
	}
}

func TestClaudeSetupReplacementRequiresAnUnchangedDestination(t *testing.T) {
	_, store, account, _, _ := newClaudeSetupProfile(t)
	dir, err := store.claudeNativeRoot(account)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	config := filepath.Join(account.NativeDir, ".claude.json")
	expected := []byte(`{"oauthAccount":{"token":"before"}}`)
	writeClaudeSetupConfig(t, config, string(expected))
	updated := []byte(`{"oauthAccount":{"token":"before"},"hasCompletedOnboarding":true}`)
	if err := os.WriteFile(config, []byte(`{"oauthAccount":{"token":"changed"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := replaceClaudeSetupFile(dir, ".claude.json", expected, updated); err == nil {
		t.Fatal("replaced a changed config")
	}
	if err := os.Remove(config); err != nil {
		t.Fatal(err)
	}
	if err := replaceClaudeSetupFile(dir, ".claude.json", expected, updated); err == nil {
		t.Fatal("recreated a disappeared config")
	}
	if _, err := os.Stat(config); !os.IsNotExist(err) {
		t.Fatalf("disappeared config was recreated: %v", err)
	}
}

func TestClaudeSetupSignalCancelsAuthStatusBeforeNativeLaunch(t *testing.T) {
	dataHome, _, account, root, bin := newClaudeSetupProfile(t)
	config := filepath.Join(account.NativeDir, ".claude.json")
	original := `{"oauthAccount":{"token":"fixture"}}`
	writeClaudeSetupConfig(t, config, original)
	child := filepath.Join(account.NativeDir, ".auth-child")
	usage := `{"type":"control_response","response":{"subtype":"success","request_id":"usage-1","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":10,"resets_at":"2099-01-01T00:00:00Z"}}}}}`
	script := `#!/bin/sh
if [ "$1" = --version ]; then printf 'unknown\n'; exit 0; fi
if [ "$1" = --print ]; then
  while IFS= read -r line; do
    case "$line" in
      *init-1*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}' ;;
      *usage-1*) printf '%s\n' '` + usage + `'; exit 0 ;;
    esac
  done
fi
if [ "$1" = auth ]; then
  /bin/sleep 30 &
  printf '%s' "$!" > "$CLAUDE_CONFIG_DIR/.auth-child"
  wait
fi
printf native > "$CLAUDE_CONFIG_DIR/.native-launch"
`
	writeInstallFixture(t, filepath.Join(bin, "claude"), script)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestCodatorClaudeSetupSignalChild$")
	cmd.Env = []string{
		"HOME=" + root,
		"PATH=" + bin + ":/usr/bin:/bin",
		"XDG_DATA_HOME=" + dataHome,
		"CODATOR_CLAUDE_SETUP_SIGNAL_CHILD=1",
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	waitForFile(t, child, cmd)
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("launch succeeded after SIGINT")
	}
	assertProbeChildStopped(t, child)
	if got, err := os.ReadFile(config); err != nil || string(got) != original {
		t.Fatalf("signal changed config=%q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(account.NativeDir, ".native-launch")); !os.IsNotExist(err) {
		t.Fatalf("native launch ran after signal: %v", err)
	}
}

func TestCodatorClaudeSetupSignalChild(t *testing.T) {
	if os.Getenv("CODATOR_CLAUDE_SETUP_SIGNAL_CHILD") == "1" {
		os.Exit(run([]string{"claude", "--", "hello"}))
	}
}

const goodClaudeAuthStatus = `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"max"}`

func newClaudeSetupProfile(t *testing.T) (string, *Store, Account, string, string) {
	t.Helper()
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	return dataHome, store, account, root, bin
}

func writeClaudeSetupConfig(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeClaudeSetupStatusStub(t *testing.T, path, status, rewrite string) {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = auth ]; then\nprintf 'auth\\n' >> \"$CLAUDE_CONFIG_DIR/.auth-count\"\n"
	if rewrite != "" {
		script += "printf '%s' " + shellQuote(rewrite) + " > \"$CLAUDE_CONFIG_DIR/.claude.json\"\n"
	}
	script += "printf '%s\\n' " + shellQuote(status) + "\nexit 0\nfi\nexit 99\n"
	writeInstallFixture(t, path, script)
}

func writeClaudeSetupLaunchStub(t *testing.T, path string) {
	t.Helper()
	usage := `{"type":"control_response","response":{"subtype":"success","request_id":"usage-1","response":{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":10,"resets_at":"2099-01-01T00:00:00Z"}}}}}`
	script := `#!/bin/sh
if [ "$1" = --version ]; then printf 'unknown\n'; exit 0; fi
if [ "$1" = --print ]; then
  while IFS= read -r line; do
    case "$line" in
      *init-1*) printf '%s\n' '{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}' ;;
      *usage-1*) printf '%s\n' '` + usage + `'; exit 0 ;;
    esac
  done
  exit 99
fi
if [ "$1" = auth ]; then
  printf 'auth\n' >> "$CLAUDE_CONFIG_DIR/.auth-count"
  printf '%s\n' '` + goodClaudeAuthStatus + `'
  exit 0
fi
/bin/cat "$CLAUDE_CONFIG_DIR/.claude.json" > "$CODATOR_SETUP_SEEN"
printf '%s\0' "$@" > "$CODATOR_SETUP_ARGV"
pwd -P > "$CODATOR_SETUP_CWD"
exit 23
`
	writeInstallFixture(t, path, script)
}

func assertClaudeSetupConfig(t *testing.T, path, original string, wantComplete bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got, want map[string]json.RawMessage
	if json.Unmarshal(data, &got) != nil || json.Unmarshal([]byte(original), &want) != nil {
		t.Fatalf("bad JSON got=%q", data)
	}
	if string(got["oauthAccount"]) != string(want["oauthAccount"]) || string(got["number"]) != string(want["number"]) || string(got["unknown"]) != string(want["unknown"]) {
		t.Fatalf("config values were not preserved: %s", data)
	}
	if string(got["hasCompletedOnboarding"]) != strconv.FormatBool(wantComplete) {
		t.Fatalf("onboarding flag=%s", got["hasCompletedOnboarding"])
	}
}

func countClaudeSetupFile(t *testing.T, account Account, name string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(account.NativeDir, name))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestLoginDaysLeftRoundsUpLikeClaudeCode(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		left     time.Duration
		wantDays int
		wantText string
		wantWarn bool
	}{
		{-time.Hour, 0, "login expired", true},
		{0, 0, "login expired", true},
		{time.Millisecond, 1, "login expires in 1 day", true},
		{24 * time.Hour, 1, "login expires in 1 day", true},
		{24*time.Hour + time.Millisecond, 2, "login expires in 2 days", true},
		{72 * time.Hour, 3, "login expires in 3 days", true},
		{72*time.Hour + time.Millisecond, 4, "login expires in 4 days", false},
		{10 * 24 * time.Hour, 10, "login expires in 10 days", false},
	} {
		expires := now.Add(test.left)
		text, warn := loginExpiryText(expires, now)
		if days := loginDaysLeft(expires, now); days != test.wantDays || text != test.wantText || warn != test.wantWarn {
			t.Errorf("left %s: days=%d text=%q warn=%t, want %d %q %t", test.left, days, text, warn, test.wantDays, test.wantText, test.wantWarn)
		}
	}
	// The furthest accepted date is beyond what a time.Duration can hold.
	if days := loginDaysLeft(time.UnixMilli(claudeLoginMaxMillis), now); days != 2912163 {
		t.Errorf("days until the furthest accepted date = %d, want 2912163", days)
	}
}

func TestStatusKeepsLoginExpiryForLockedClaudeAccount(t *testing.T) {
	_, store, account, _, _ := newClaudeSetupProfile(t)
	expires := time.Now().Add(47 * time.Hour)
	writeClaudeSetupConfig(t, filepath.Join(account.NativeDir, ".credentials.json"),
		`{"claudeAiOauth":{"accessToken":"fake-access","refreshTokenExpiresAt":`+strconv.FormatInt(expires.UnixMilli(), 10)+`}}`)
	lock, err := store.Lock("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	report, err := collectStatus(store, []string{"claude"}, nil)
	if err != nil || len(report.Accounts) != 1 || report.Accounts[0].State != "busy" || report.Accounts[0].Launchable {
		t.Fatalf("status report=%+v err=%v", report, err)
	}
	var out strings.Builder
	if err := renderStatusJSON(&out, report); err != nil {
		t.Fatal(err)
	}
	if want := `"login_expires_at": "` + expires.UTC().Truncate(time.Second).Format(time.RFC3339) + `"`; !strings.Contains(out.String(), want) {
		t.Fatalf("busy account JSON lacks %s:\n%s", want, out.String())
	}
	panel := statusPanelText(time.Now(), false, report.Accounts...)
	if !strings.Contains(panel, "LOGIN") || !strings.Contains(panel, "  2d ") || !strings.Contains(panel, "│   login expires in 2 days; run: codator login claude work") {
		t.Fatalf("busy account panel:\n%s", panel)
	}
	if strings.Contains(out.String()+panel, "fake-") {
		t.Fatalf("output leaks a credentials value: %s%s", out.String(), panel)
	}
}

func TestClaudeLoginExpiryShowsDaysLeftAndNothingElse(t *testing.T) {
	credentials := func(value string) string {
		return `{"claudeAiOauth":{"accessToken":"fake-access","refreshToken":"fake-refresh","expiresAt":1,"refreshTokenExpiresAt":` + value + `},"mcpOAuth":{"server":{"accessToken":"fake-mcp"}}}`
	}
	millis := func(left time.Duration) string {
		return strconv.FormatInt(time.Now().Add(left).UnixMilli(), 10)
	}
	const renew = "; run: codator login claude work"
	for _, test := range []struct {
		name        string
		credentials string // "" writes no file
		mode        os.FileMode
		known       bool
		cell        string // the LOGIN cell
		text        string // doctor wording, and the launch and table warning when warn is set
		warn        bool
	}{
		{"two days ahead", credentials(millis(47 * time.Hour)), 0600, true, "2d", "login expires in 2 days", true},
		{"ten days ahead", credentials(millis(239 * time.Hour)), 0600, true, "10d", "login expires in 10 days", false},
		{"expired", credentials(millis(-time.Hour)), 0600, true, "expired", "login expired", true},
		{"absent file", "", 0600, false, "", "", false},
		{"absent key", `{"claudeAiOauth":{"accessToken":"fake-access"},"mcpOAuth":{}}`, 0600, false, "", "", false},
		{"keychain host", `{"mcpOAuth":{"server":{"accessToken":"fake-mcp"}}}`, 0600, false, "", "", false},
		{"null", credentials("null"), 0600, false, "", "", false},
		{"string", credentials(`"` + millis(47*time.Hour) + `"`), 0600, false, "", "", false},
		{"negative", credentials("-1"), 0600, false, "", "", false},
		{"zero", credentials("0"), 0600, false, "", "", false},
		{"absurdly large", credentials("1e300"), 0600, false, "", "", false},
		{"not a float", credentials("1e999"), 0600, false, "", "", false},
		{"malformed", `{"claudeAiOauth":{"refreshToken":"fake-refresh"`, 0600, false, "", "", false},
		{"shared file", credentials(millis(47 * time.Hour)), 0644, false, "", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, store, account, root, bin := newClaudeSetupProfile(t)
			if test.credentials != "" {
				path := filepath.Join(account.NativeDir, ".credentials.json")
				if err := os.WriteFile(path, []byte(test.credentials), test.mode); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, test.mode); err != nil {
					t.Fatal(err)
				}
			}
			writeClaudeSetupConfig(t, filepath.Join(account.NativeDir, ".claude.json"), `{"hasCompletedOnboarding":true}`)
			codex, err := store.EnsureAccount("codex", "work")
			if err != nil {
				t.Fatal(err)
			}
			writeClaudeSetupConfig(t, filepath.Join(codex.NativeDir, ".credentials.json"), credentials(millis(47*time.Hour)))
			writeClaudeSetupLaunchStub(t, filepath.Join(bin, "claude"))
			t.Setenv("PATH", bin+":/usr/bin:/bin")
			t.Setenv("XDG_DATA_HOME", dataHome)

			expires, known := store.claudeLoginExpiry(account)
			if known != test.known || (!known && !expires.IsZero()) {
				t.Fatalf("login expiry = %s known=%t, want known=%t", expires, known, test.known)
			}

			report, err := collectStatus(store, []string{"codex", "claude"}, nil)
			if err != nil || len(report.Accounts) != 2 || report.SchemaVersion != 1 {
				t.Fatalf("status report=%+v err=%v", report, err)
			}
			var statusOut, doctorOut strings.Builder
			if err := renderStatusJSON(&statusOut, report); err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Accounts []map[string]any `json:"accounts"`
			}
			if err := json.Unmarshal([]byte(statusOut.String()), &decoded); err != nil {
				t.Fatal(err)
			}
			if _, has := decoded.Accounts[0]["login_expires_at"]; has || decoded.Accounts[0]["provider"] != "codex" {
				t.Fatalf("Codex row has a login expiry: %v", decoded.Accounts[0])
			}
			stamp, has := decoded.Accounts[1]["login_expires_at"]
			if has != test.known || (known && stamp != expires.Truncate(time.Second).Format(time.RFC3339)) {
				t.Fatalf("login_expires_at=%v present=%t, want present=%t for %s", stamp, has, test.known, expires)
			}
			now := time.Now()
			renderStatusPanel(&statusOut, statusProvider{Provider: "codex", Status: "ok"}, report.Accounts[:1], false, now)
			var panel strings.Builder
			renderStatusPanel(&panel, statusProvider{Provider: "claude", Status: "ok"}, report.Accounts[1:], false, now)
			statusOut.WriteString(panel.String())
			if strings.Contains(panel.String(), "LOGIN") != test.known || strings.Contains(panel.String(), "login") != test.warn ||
				(test.known && !strings.Contains(panel.String(), "  "+test.cell+" ")) || (test.warn && !strings.Contains(panel.String(), "│   "+test.text+renew)) {
				t.Fatalf("status panel:\n%s", panel.String())
			}
			assertStatusPanelAligned(t, panel.String())
			if strings.Contains(statusOut.String(), "LOGIN") != test.known {
				t.Fatalf("Codex panel shows a login column:\n%s", statusOut.String())
			}
			color := statusLoginCell(report.Accounts[1], now).paint(true)
			if wantColor := map[string]string{"2d": "\x1b[33m2d\x1b[0m", "10d": "10d", "expired": "\x1b[31mexpired\x1b[0m"}[test.cell]; color != wantColor {
				t.Fatalf("colored login cell = %q, want %q", color, wantColor)
			}

			doctorClaudeLogins(&doctorOut, now)
			wantDoctor := "claude: account work login expiry unknown\n"
			if test.known {
				wantDoctor = "claude: account work " + test.text + "\n"
			}
			if test.warn {
				wantDoctor = "claude: account work " + test.text + renew + "\n"
			}
			if doctorOut.String() != wantDoctor {
				t.Fatalf("doctor output=%q, want %q", doctorOut.String(), wantDoctor)
			}
			if code, err := execute(invocation{verb: "doctor", provider: "claude"}); err != nil || code != 0 {
				t.Fatalf("doctor code=%d err=%v", code, err)
			}

			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(exe, "-test.run=^TestCodatorPassthroughChild$")
			cmd.Dir = root
			cmd.Env = []string{
				"HOME=" + root,
				"PATH=" + bin + ":/usr/bin:/bin",
				"XDG_DATA_HOME=" + dataHome,
				"CODATOR_PASSTHROUGH_CHILD=1",
				`CODATOR_TEST_ARGS=["claude","--","hello"]`,
				"CODATOR_SETUP_SEEN=" + filepath.Join(root, "seen.json"),
				"CODATOR_SETUP_ARGV=" + filepath.Join(root, "argv"),
				"CODATOR_SETUP_CWD=" + filepath.Join(root, "cwd"),
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if exit, ok := cmd.Run().(*exec.ExitError); !ok || exit.ExitCode() != 23 {
				t.Fatalf("launch did not reach native Claude: stderr=%q", stderr.String())
			}
			wantLaunch := "codator: using claude account work\n"
			if test.warn {
				wantLaunch += "codator: claude account work " + test.text + renew + "\n"
			}
			if stderr.String() != wantLaunch {
				t.Fatalf("launch stderr=%q, want %q", stderr.String(), wantLaunch)
			}

			for _, output := range []string{expires.String(), statusOut.String(), doctorOut.String(), stderr.String()} {
				if strings.Contains(output, "fake-") {
					t.Fatalf("output leaks a credentials value: %q", output)
				}
			}
		})
	}
}
