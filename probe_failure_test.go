package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeProbeFailureClassification(t *testing.T) {
	for _, test := range []struct{ raw, kind string }{
		{`{"status":401,"message":"secret-token"}`, "auth"},
		{`{"data":{"http_status":403}}`, "auth"},
		{`{"type":"authentication_error"}`, "auth"},
		{`"Usage API error: 401 unauthorized secret-token"`, "auth"},
		{`{"code":"token_expired"}`, "auth"},
		{`{"statusCode":429,"headers":{"retry-after":"120"}}`, "rate_limit"},
		{`{"error":{"type":"rate_limit_error"},"retry_after":120}`, "rate_limit"},
		{`{"code":"ENOTFOUND","message":"secret-token"}`, "transport"},
		{`"fetch failed: secret-token"`, "transport"},
		{`{"message":"request timed out"}`, "transport"},
		{`{"message":"secret-token"}`, "malformed"},
		{`[]`, "malformed"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			failure := probeFailureOf(nativeProbeFailure(json.RawMessage(test.raw)))
			if failure.Kind != test.kind || strings.Contains(failure.Error(), "secret-token") {
				t.Fatalf("failure=%+v", failure)
			}
			if strings.Contains(test.raw, "120") && time.Until(failure.RetryAt) < 119*time.Second {
				t.Fatalf("lost retry-after: %+v", failure)
			}
		})
	}
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	failure := probeFailureOf(nativeProbeFailure(json.RawMessage(`{"status":429,"headers":{"Retry-After":"` + at.Format(http.TimeFormat) + `"}}`)))
	if !failure.RetryAt.Equal(at) {
		t.Fatalf("HTTP-date retry=%v want=%v", failure.RetryAt, at)
	}
}

func TestNativeControlAndRPCFailuresAreSanitized(t *testing.T) {
	for _, test := range []struct{ raw, kind string }{
		{`{"status":401,"message":"secret-token"}`, "auth"},
		{`{"status":429,"retry_after":90}`, "rate_limit"},
		{`"network error secret-token"`, "transport"},
	} {
		frame := []byte(`{"type":"control_response","response":{"subtype":"error","request_id":"usage-1","error":` + test.raw + `}}`)
		_, matched, err := decodeClaudeControlFrame(frame, "usage-1")
		if !matched || probeFailureOf(err).Kind != test.kind || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("Claude matched=%t err=%v", matched, err)
		}
		_, err = readRPC(bufio.NewScanner(strings.NewReader(`{"id":3,"error":`+test.raw+`}`)), 3)
		if probeFailureOf(err).Kind != test.kind || strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("Codex err=%v", err)
		}
	}
}

func installClaudeFailure(t *testing.T, binDir string, raw string) {
	t.Helper()
	installFakeClaude(t, binDir, json.RawMessage(`{"error":`+raw+`}`), false, 0, false)
}

func ageClaudeAttempt(t *testing.T, store *Store, account Account, age time.Duration) {
	t.Helper()
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found {
		t.Fatalf("cache found=%t err=%v", found, err)
	}
	interval := state.NextProbeAt.Sub(state.AttemptedAt)
	state.AttemptedAt = time.Now().UTC().Add(-age)
	state.NextProbeAt = state.AttemptedAt.Add(interval)
	if state.Outcome == "usable" || state.Outcome == "unknown" {
		state.SubscriptionUntil = state.NextProbeAt
	}
	if err := store.writeClaudeProbeCache(account, state); err != nil {
		t.Fatal(err)
	}
}

func forcedClaudeProbe(t *testing.T, store *Store, account Account) quota {
	t.Helper()
	lock, err := store.Lock("claude", account.Name)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	q, err := store.probeClaudeAccount(context.Background(), account, claudeModelSonnet, true)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func TestClaudeFailurePolicyAndExplicitLaunch(t *testing.T) {
	for _, test := range []struct {
		name, raw, kind string
		explicit        bool
		interval        time.Duration
	}{
		{"DNS outage", `{"code":"ENOTFOUND","message":"secret-token"}`, "transport", true, time.Minute},
		{"expired login", `{"status":401,"message":"secret-token"}`, "auth", false, time.Minute},
		{"rate limit default", `{"status":429}`, "rate_limit", true, 15 * time.Minute},
		{"rate limit retry-after", `{"status":429,"retry_after":120}`, "rate_limit", true, 2 * time.Minute},
		{"malformed error", `{"message":"secret-token"}`, "malformed", false, time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, home, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().Add(24*time.Hour), 10), false, 0, false)
			if q, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil || !q.Eligible {
				t.Fatalf("prime=%+v err=%v", q, err)
			}
			ageClaudeAttempt(t, store, account, 6*time.Minute)
			installClaudeFailure(t, binDir, test.raw)
			q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
			if err != nil || q.ProbeFailure != test.kind || q.Eligible || q.Known || canLaunchExplicit(q) != test.explicit || strings.Contains(q.Reason, "secret-token") {
				t.Fatalf("failure quota=%+v err=%v", q, err)
			}
			state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
			if err != nil || !found {
				t.Fatalf("cache found=%t err=%v", found, err)
			}
			interval := state.NextProbeAt.Sub(state.AttemptedAt)
			if interval < test.interval || interval > test.interval+time.Second {
				t.Fatalf("interval=%v want=%v", interval, test.interval)
			}
			data, err := os.ReadFile(filepath.Join(filepath.Dir(account.NativeDir), claudeCacheName))
			if err != nil || strings.Contains(string(data), "secret-token") {
				t.Fatalf("unsafe cache err=%v", err)
			}
			code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"claude", "--account", "work", "--model", "sonnet", "prompt"})
			if (code == 0) != test.explicit || test.explicit && !strings.Contains(stderr, "usage unknown") {
				t.Fatalf("explicit code=%d stderr=%q", code, stderr)
			}
			if test.kind == "auth" && !strings.Contains(stderr, "codator login claude work") {
				t.Fatalf("missing fix: %s", stderr)
			}
			code, _, stderr = runCodatorProcess(t, home, dataHome, binDir, []string{"claude", "--model", "sonnet", "prompt"})
			if code == 0 || test.kind == "auth" && !strings.Contains(stderr, "codator login claude work") {
				t.Fatalf("automatic code=%d stderr=%s", code, stderr)
			}
			code, stdout, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "claude", "--json"})
			if code != 0 || test.kind == "auth" && !strings.Contains(stdout, "codator login claude work") {
				t.Fatalf("status code=%d out=%s err=%s", code, stdout, stderr)
			}
			if count := fakeClaudeCount(t, account, ".probe-count"); count != 2 {
				t.Fatalf("cooldown repeated probe: %d", count)
			}
		})
	}
}

func TestClaudeRefreshBypassesCooldownButKeepsAuthAndMinimumInterval(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	installClaudeFailure(t, binDir, `{"status":401}`)
	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || q.ProbeFailure != "auth" {
		t.Fatalf("auth=%+v err=%v", q, err)
	}
	ageClaudeAttempt(t, store, account, time.Hour)
	if q, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil || q.ProbeFailure != "auth" || fakeClaudeCount(t, account, ".probe-count") != 1 {
		t.Fatalf("auth retried automatically: %+v err=%v", q, err)
	}
	installClaudeFailure(t, binDir, `{"code":"ENOTFOUND"}`)
	if q := forcedClaudeProbe(t, store, account); q.ProbeFailure != "auth" || canLaunchExplicit(q) {
		t.Fatalf("network revived auth denial: %+v", q)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().Add(24*time.Hour), 10), false, 0, false)
	if q := forcedClaudeProbe(t, store, account); q.ProbeFailure != "auth" || fakeClaudeCount(t, account, ".probe-count") != 2 {
		t.Fatalf("force bypassed minimum interval: %+v", q)
	}
	ageClaudeAttempt(t, store, account, 11*time.Second)
	if q := forcedClaudeProbe(t, store, account); !q.Eligible || q.ProbeFailure != "" {
		t.Fatalf("force could not recover: %+v", q)
	}
}

func TestClaudeLegacyFailedReservationsDoNotWaitFifteenMinutes(t *testing.T) {
	for _, outcome := range []string{"reserved", "unknown"} {
		t.Run(outcome, func(t *testing.T) {
			dataHome, _, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			state := claudeProbeCache{Version: 2, AttemptedAt: now.Add(-2 * time.Minute), NextProbeAt: now.Add(13 * time.Minute), Outcome: outcome}
			if outcome == "unknown" {
				state.ObservedAt = now.Add(-2 * time.Minute)
				state.Snapshot, _, _, _, _, err = sanitizeClaudeQuotaPayload(json.RawMessage(`{"subscription_type":"unknown","rate_limits_available":false,"rate_limits":null}`), state.ObservedAt)
				if err != nil {
					t.Fatal(err)
				}
			}
			dir, err := store.accountRoot("claude", "work", false)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(state)
			err = replaceClaudeCacheFile(dir, data)
			dir.Close()
			if err != nil {
				t.Fatal(err)
			}
			installFakeClaude(t, binDir, fullClaudeUsagePayload(now.Add(time.Hour), 10), false, 0, false)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			if q, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil || !q.Eligible || fakeClaudeCount(t, account, ".probe-count") != 1 {
				t.Fatalf("legacy locked out: %+v err=%v", q, err)
			}
		})
	}
}

func TestClaudeConcurrentForcedRefreshUsesOneProbe(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().Add(time.Hour), 10), false, 100*time.Millisecond, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	lock, err := store.Lock("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	var group sync.WaitGroup
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			lock, err := store.Lock("claude", "work")
			if err == ErrAccountBusy {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			defer lock.Close()
			if _, err := store.probeClaudeAccount(context.Background(), account, claudeModelSonnet, true); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	if count := fakeClaudeCount(t, account, ".probe-count"); count != 1 {
		t.Fatalf("forced burst probes=%d", count)
	}
}

func installCodexProbeFixture(t *testing.T, binDir, mode string) {
	t.Helper()
	account := `{"id":2,"result":{"account":{"type":"chatgpt","email":null,"planType":"plus"}}}`
	usage := `{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":10},"spendControlReached":false}}}`
	init := `{"id":1,"result":{}}`
	switch mode {
	case "auth":
		usage = `{"id":3,"error":{"code":401,"message":"secret-token"}}`
	case "account auth":
		account = `{"id":2,"error":{"data":{"status":401},"message":"secret-token"}}`
	case "handshake auth":
		init = `{"id":1,"error":{"code":401,"message":"secret-token"}}`
	case "signed out":
		account = `{"id":2,"result":{"account":null}}`
	case "transport":
		usage = `{"id":3,"error":{"message":"network error secret-token"}}`
	case "malformed":
		usage = `{"id":3,"result":[]}`
	case "empty":
		usage = `{"id":3,"result":{}}`
	case "partial":
		usage = `{"id":3,"result":{"ordinaryUsageAllowed":true}}`
	case "exhausted":
		usage = `{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"primary":{"usedPercent":100},"spendControlReached":false}}}`
	case "rate limit":
		usage = `{"id":3,"error":{"data":{"status":429,"retry_after":120}}}`
	}
	script := `#!/bin/sh
printf 'probe\n' >> "$CODEX_HOME/.fixture-probes"
while IFS= read -r line; do
 case "$line" in
 *'"method":"initialize"'*) printf '%s\n' ` + shellQuote(init) + ` ;;
 *'"method":"account/read"'*) printf '%s\n' ` + shellQuote(account) + ` ;;
 *'"method":"account/rateLimits/read"'*) printf '%s\n' ` + shellQuote(usage) + ` ;;
 esac
done
`
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestCodexFailureCausesAndPersistentAuth(t *testing.T) {
	for _, mode := range []string{"auth", "account auth", "handshake auth", "signed out", "transport", "malformed", "rate limit"} {
		t.Run(mode, func(t *testing.T) {
			store, err := newStore(tempDataHome(t))
			if err != nil {
				t.Fatal(err)
			}
			account, err := store.EnsureAccount("codex", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			binDir := t.TempDir()
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			t.Setenv("HOME", t.TempDir())
			installCodexProbeFixture(t, binDir, mode)
			lock, err := store.Lock("codex", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			q, _ := store.probeCodexAccount(context.Background(), account, false)
			auth := strings.Contains(mode, "auth") || mode == "signed out"
			expected := mode
			if auth {
				expected = "auth"
			}
			if mode == "rate limit" {
				expected = "rate_limit"
			}
			if q.ProbeFailure != expected || strings.Contains(q.Reason, "secret-token") || canLaunchExplicit(q) != (mode == "transport" || mode == "rate limit") {
				t.Fatalf("mode=%s q=%+v", mode, q)
			}
			if auth {
				if !strings.Contains(q.Reason, "codator login codex fixture") {
					t.Fatalf("missing fix: %+v", q)
				}
				installCodexProbeFixture(t, binDir, "success")
				q, _ = store.probeCodexAccount(context.Background(), account, true)
				if q.ProbeFailure != "auth" {
					t.Fatalf("bypassed auth refresh fence: %+v", q)
				}
				q, _ = store.probeCodexAccount(context.Background(), account, false)
				if q.ProbeFailure != "auth" {
					t.Fatalf("forgot auth: %+v", q)
				}
				data, err := os.ReadFile(filepath.Join(account.NativeDir, ".fixture-probes"))
				if err != nil || strings.Count(string(data), "\n") != 1 {
					t.Fatalf("auth re-probed: %q err=%v", data, err)
				}
				// Auth survives across invocations; a failed refresh cannot clear it.
				dir, err := store.accountRoot("codex", "fixture", false)
				if err != nil {
					t.Fatal(err)
				}
				state, _ := json.Marshal(codexProbeState{AttemptedAt: time.Now().Add(-time.Minute), AuthRequired: true})
				if err := replaceProbeCacheFile(dir, state, codexProbeStateName, ".codex-probe.tmp"); err != nil {
					t.Fatal(err)
				}
				dir.Close()
				installCodexProbeFixture(t, binDir, "transport")
				q, _ = store.probeCodexAccount(context.Background(), account, true)
				if q.ProbeFailure != "auth" || canLaunchExplicit(q) {
					t.Fatalf("network revived auth: %+v", q)
				}
				if err := store.invalidateCodexProbeOnLogin(account); err != nil {
					t.Fatal(err)
				}
				installCodexProbeFixture(t, binDir, "success")
				q, err = store.probeCodexAccount(context.Background(), account, false)
				if err != nil || !q.Eligible {
					t.Fatalf("login did not recover: %+v err=%v", q, err)
				}
			}
		})
	}
}

func TestStatusRefreshCLI(t *testing.T) {
	for _, args := range [][]string{
		{"status", "--refresh"}, {"status", "claude", "--refresh", "--json"}, {"status", "--refresh", "--json", "codex"},
	} {
		inv, err := parseInvocation(args)
		if err != nil || !inv.refresh {
			t.Fatalf("args=%v inv=%+v err=%v", args, inv, err)
		}
	}
	if _, err := parseInvocation([]string{"status", "--refresh", "--refresh"}); err == nil {
		t.Fatal("accepted duplicate --refresh")
	}
	dataHome, home, binDir, account := newFakeClaudeProfile(t)
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().Add(time.Hour), 10), false, 0, false)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "claude"}); code != 0 {
		t.Fatal(stderr)
	}
	ageClaudeAttempt(t, store, account, 11*time.Second)
	for _, args := range [][]string{{"status", "--refresh", "claude", "--json"}, {"status", "claude", "--refresh"}} {
		code, stdout, stderr := runCodatorProcess(t, home, dataHome, binDir, args)
		if code != 0 || stdout == "" {
			t.Fatalf("args=%v code=%d stdout=%s stderr=%s", args, code, stdout, stderr)
		}
	}
	if count := fakeClaudeCount(t, account, ".probe-count"); count != 2 {
		t.Fatalf("CLI refresh probes=%d want=2", count)
	}
}

func TestClaudeAuthPartialRefreshCannotRestoreExplicitLaunch(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	installClaudeFailure(t, binDir, `{"status":401}`)
	if _, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil {
		t.Fatal(err)
	}
	ageClaudeAttempt(t, store, account, time.Minute)
	installFakeClaude(t, binDir, json.RawMessage(`{"subscription_type":"max","rate_limits_available":false,"rate_limits":null}`), false, 0, false)
	if q := forcedClaudeProbe(t, store, account); q.ProbeFailure != "auth" || canLaunchExplicit(q) {
		t.Fatalf("partial response revived auth: %+v", q)
	}
}

func TestClaudeLegacyMigrationRetainsDenialAndRejectsAmbiguousIdentity(t *testing.T) {
	dataHome, _, _, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	snapshot, subscription, _, _, denials, err := sanitizeClaudeQuotaPayload(fullClaudeUsagePayload(now.Add(time.Hour), 100), now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	state := claudeProbeCache{Version: 2, AttemptedAt: now.Add(-30 * time.Second), NextProbeAt: now.Add(14*time.Minute + 30*time.Second), Outcome: "reserved", ObservedAt: now.Add(-time.Minute), Subscription: subscription, SubscriptionUntil: now.Add(4 * time.Minute), Snapshot: snapshot, Denials: denials}
	dir, err := store.accountRoot("claude", "work", false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(state)
	if err := replaceClaudeCacheFile(dir, data); err != nil {
		t.Fatal(err)
	}
	dir.Close()
	migrated, found, err := store.readClaudeProbeCache(account, now)
	if err != nil || !found || migrated.Version != 3 || !migrated.NextProbeAt.Equal(state.AttemptedAt.Add(time.Minute)) {
		t.Fatalf("migration=%+v found=%t err=%v", migrated, found, err)
	}
	q := claudeCachedQuota(migrated, now, claudeModelSonnet)
	if !q.Known || q.Eligible || canLaunchExplicit(q) || q.Subscription {
		t.Fatalf("legacy failed reservation revived denial/identity: %+v", q)
	}
	// The same ambiguity without a denial must not authorize an explicit launch.
	migrated.Denials = nil
	migrated.Snapshot = nil
	migrated.ObservedAt = time.Time{}
	migrated.Subscription = false
	migrated.SubscriptionUntil = time.Time{}
	q = claudeCachedQuota(migrated, now, claudeModelSonnet)
	if canLaunchExplicit(q) {
		t.Fatalf("ambiguous legacy failure verified identity: %+v", q)
	}
}

func TestNativeProbeFailureIgnoresIncidentalDiagnostics(t *testing.T) {
	for _, test := range []struct{ raw, kind string }{
		{`{"code":"ENOTFOUND","message":"DNS lookup failed for proxy-401.internal"}`, "transport"},
		{`{"message":"unexpected payload at byte 429"}`, "malformed"},
		{`{"status":500,"message":"upstream did not return 401 or 429"}`, "transport"},
		{`{"code":"ENOTFOUND","message":"authentication_error is mentioned in documentation"}`, "transport"},
		{`{"message":"upstream was not unauthorized or rate limited"}`, "malformed"},
		{`{"metadata":{"status":401,"code":"rate_limit_error"}}`, "malformed"},
		{`{"data":{"request":{"status":429}},"message":"unexpected payload"}`, "malformed"},
		{`{"code":"ENOTFOUND","metadata":{"status":401}}`, "transport"},
		{`{"status":"4010","message":"unexpected payload"}`, "malformed"},
		{`{"code":"not_unauthorized","message":"unexpected payload"}`, "malformed"},
		{`{"message":"401"}`, "malformed"},
		{`"Usage API error: unexpected payload at byte 429"`, "malformed"},
		{`"Usage API error: proxy-401.internal"`, "malformed"},
		{`"Usage API error: 401.internal"`, "malformed"},
		{`{"status":"401","message":"secret-token"}`, "auth"},
		{`{"data":{"httpStatusCode":429},"message":"secret-token"}`, "rate_limit"},
		{`{"message":"HTTP error: 401 Unauthorized secret-token"}`, "auth"},
		{`"authentication failed: secret-token"`, "auth"},
	} {
		t.Run(test.raw, func(t *testing.T) {
			// Both native protocols use this classifier; neither may promote incidental text to a denial.
			_, _, claudeErr := decodeClaudeControlFrame([]byte(`{"type":"control_response","response":{"subtype":"error","request_id":"usage-1","error":`+test.raw+`}}`), "usage-1")
			_, codexErr := readRPC(bufio.NewScanner(strings.NewReader(`{"id":3,"error":`+test.raw+`}`)), 3)
			for _, err := range []error{claudeErr, codexErr} {
				failure := probeFailureOf(err)
				if failure.Kind != test.kind || strings.Contains(failure.Error(), "secret-token") {
					t.Fatalf("failure=%+v want=%s", failure, test.kind)
				}
			}
		})
	}
}

func TestCodexAuthRefreshRequiresKnownUsage(t *testing.T) {
	for _, mode := range []string{"empty", "partial", "exhausted", "success"} {
		t.Run(mode, func(t *testing.T) {
			store, err := newStore(tempDataHome(t))
			if err != nil {
				t.Fatal(err)
			}
			account, err := store.EnsureAccount("codex", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			binDir := t.TempDir()
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			t.Setenv("HOME", t.TempDir())
			lock, err := store.Lock("codex", "fixture")
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			installCodexProbeFixture(t, binDir, "auth")
			if q, err := store.probeCodexAccount(context.Background(), account, false); err != nil || q.ProbeFailure != "auth" {
				t.Fatalf("initial auth=%+v err=%v", q, err)
			}
			dir, err := store.accountRoot("codex", "fixture", false)
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			state, _ := json.Marshal(codexProbeState{AttemptedAt: time.Now().Add(-time.Minute), AuthRequired: true})
			if err := replaceProbeCacheFile(dir, state, codexProbeStateName, ".codex-probe.tmp"); err != nil {
				t.Fatal(err)
			}
			installCodexProbeFixture(t, binDir, mode)
			q, err := store.probeCodexAccount(context.Background(), account, true)
			if err != nil {
				t.Fatal(err)
			}
			incomplete := mode == "empty" || mode == "partial"
			if incomplete {
				marker, err := readCodexProbeState(dir)
				if err != nil || !marker.AuthRequired || q.ProbeFailure != "auth" || canLaunchExplicit(q) {
					t.Fatalf("partial usage cleared auth: q=%+v state=%+v err=%v", q, marker, err)
				}
				// A later ordinary invocation must still refuse the account without another probe.
				q, err = store.probeCodexAccount(context.Background(), account, false)
				data, readErr := os.ReadFile(filepath.Join(account.NativeDir, ".fixture-probes"))
				if err != nil || q.ProbeFailure != "auth" || readErr != nil || strings.Count(string(data), "\n") != 2 {
					t.Fatalf("ordinary retry lost auth: q=%+v err=%v read=%v probes=%q", q, err, readErr, data)
				}
			} else {
				if _, err := dir.Lstat(codexProbeStateName); !os.IsNotExist(err) {
					t.Fatalf("known response did not clear auth: %v", err)
				}
				if !q.Known || q.ProbeFailure != "" || canLaunchExplicit(q) != (mode == "success") {
					t.Fatalf("recovery quota=%+v", q)
				}
			}
		})
	}
}

func TestClaudeFailedForcedRefreshHidesYoungAllowanceAndKeepsDenials(t *testing.T) {
	for _, test := range []struct{ kind, raw string }{
		{"auth", `{"status":401}`}, {"transport", `{"code":"ENOTFOUND"}`},
		{"rate_limit", `{"status":429}`}, {"malformed", `{"message":"unexpected payload"}`},
	} {
		t.Run(test.kind, func(t *testing.T) {
			dataHome, _, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			payload := strings.Replace(string(fullClaudeUsagePayload(time.Now().Add(time.Hour), 10)), `"display_name":"Fable","utilization":50`, `"display_name":"Fable","utilization":100`, 1)
			installFakeClaude(t, binDir, json.RawMessage(payload), false, 0, false)
			if q, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil || !q.Eligible {
				t.Fatalf("prime=%+v err=%v", q, err)
			}
			ageClaudeAttempt(t, store, account, 11*time.Second)
			installClaudeFailure(t, binDir, test.raw)
			var out bytes.Buffer
			if err := statusJSONRefresh(store, &out, true, "claude"); err != nil {
				t.Fatal(err)
			}
			var report statusReport
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Accounts) != 1 {
				t.Fatalf("accounts=%+v", report.Accounts)
			}
			row := report.Accounts[0]
			if row.Launchable || len(row.Windows) != 0 || row.HeadroomPercent != nil && *row.HeadroomPercent > 0 {
				t.Fatalf("failed refresh exposed prior allowance: %+v", row)
			}
			if len(row.KnownExhausted) != 1 || row.KnownExhausted[0].Scope != "Weekly (Fable)" {
				t.Fatalf("lost denial projection: %+v", row.KnownExhausted)
			}
			var document struct {
				Accounts []map[string]json.RawMessage `json:"accounts"`
			}
			if err := json.Unmarshal(out.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{"provider", "account", "state", "launchable", "headroom_percent", "windows", "known_exhausted", "observed_at", "next_probe_at"} {
				if _, ok := document.Accounts[0][field]; !ok {
					t.Errorf("lost existing JSON field %s", field)
				}
			}
			state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
			details := claudeStatusDetails(state, found, err, time.Now())
			if err != nil || time.Since(state.ObservedAt) >= claudeSuccessCooldown || len(state.Snapshot) == 0 || details.Unavailable == "" {
				t.Fatalf("young failure state=%+v details=%+v err=%v", state, details, err)
			}
			out.Reset()
			if err := status(store, &out, "claude"); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if strings.Contains(text, "90%") || strings.Contains(text, "█") || !strings.Contains(text, "known exhausted: Weekly (Fable)") || !strings.Contains(text, "usage unavailable (last probe failed (") {
				t.Fatalf("misleading status: %s", text)
			}
		})
	}
}
