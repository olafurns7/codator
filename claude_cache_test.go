package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeQuotaProjectionIsCanonicalAndModelScoped(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	sonnetReset := now.Add(24 * time.Hour).Format(time.RFC3339)
	fableReset := now.Add(48 * time.Hour).Format(time.RFC3339)
	payload := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{` +
		`"five_hour":{"utilization":10,"resets_at":"` + sonnetReset + `"},` +
		`"model_scoped":[` +
		`{"display_name":"Fable","utilization":100,"resets_at":"` + sonnetReset + `"},` +
		`{"display_name":"Sonnet","utilization":25,"resets_at":"` + sonnetReset + `"},` +
		`{"display_name":"Haiku","utilization":"bad","resets_at":"` + sonnetReset + `"},` +
		`{"display_name":"Opus","utilization":10,"resets_at":"` + sonnetReset + `"}],` +
		`"limits":[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}},"resets_at":"` + fableReset + `"}]}}`)

	snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(payload, now)
	if err != nil || !subscription || usable || models&claudeSonnetModelMask == 0 || models&claudeHaikuModelMask != 0 {
		t.Fatalf("projection subscription=%t models=%05b usable=%t err=%v", subscription, models, usable, err)
	}
	sonnet, err := parseClaudeQuotaForModel(snapshot, now, claudeModelSonnet)
	if err != nil || !sonnet.Known || !sonnet.Eligible {
		t.Fatalf("malformed Haiku sibling affected Sonnet: %#v, err=%v", sonnet, err)
	}
	fable, err := parseClaudeQuotaForModel(snapshot, now, claudeModelFable)
	if err != nil || !fable.Known || fable.Eligible {
		t.Fatalf("Fable denial was lost: %#v, err=%v", fable, err)
	}
	if len(denials) != 1 || denials[0].DisplayName != "Fable" || !denials[0].ResetsAt.Equal(now.Add(48*time.Hour)) {
		t.Fatalf("retained denials = %#v", denials)
	}
	canonical, _, secondModels, secondUsable, _, err := sanitizeClaudeQuotaPayload(snapshot, now)
	if err != nil || string(canonical) != string(snapshot) || secondModels != models || secondUsable != usable {
		t.Fatalf("projection is not idempotent: equal=%t models=%05b/%05b usable=%t/%t err=%v", string(canonical) == string(snapshot), models, secondModels, usable, secondUsable, err)
	}
}

func TestClaudeUnknownModelLabelsAreCanonicalAndReusable(t *testing.T) {
	for _, test := range []struct {
		name  string
		label json.RawMessage
	}{
		{"missing", nil},
		{"empty", json.RawMessage(`""`)},
		{"unrecognized", json.RawMessage(`"Future"`)},
		{"malformed type", json.RawMessage(`17`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetText := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
			unknownRow := fmt.Sprintf(`{"utilization":100,"resets_at":%q}`, resetText)
			if test.label != nil {
				unknownRow = fmt.Sprintf(`{"display_name":%s,"utilization":100,"resets_at":%q}`, test.label, resetText)
			}
			payload := json.RawMessage(fmt.Sprintf(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":10,"resets_at":%q},"model_scoped":[{"display_name":"Fable","utilization":20,"resets_at":%q},%s]}}`, resetText, resetText, unknownRow))
			now := time.Now().UTC()
			snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(payload, now)
			if err != nil || !subscription || !usable || models != claudeAllMasks || len(denials) != 1 || denials[0].DisplayName != "unknown" {
				t.Fatalf("initial projection sub=%t models=%05b usable=%t denials=%#v err=%v", subscription, models, usable, denials, err)
			}
			canonical, secondSubscription, secondModels, secondUsable, secondDenials, err := sanitizeClaudeQuotaPayload(snapshot, now)
			if err != nil || !bytes.Equal(canonical, snapshot) || secondSubscription != subscription || secondModels != models || secondUsable != usable || len(secondDenials) != len(denials) {
				t.Fatalf("canonical projection equal=%t sub=%t/%t models=%05b/%05b usable=%t/%t denials=%#v err=%v", bytes.Equal(canonical, snapshot), secondSubscription, subscription, secondModels, models, secondUsable, usable, secondDenials, err)
			}

			dataHome, _, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			installFakeClaude(t, binDir, payload, false, 0, false)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
			first, err := runClaudeProbe(t, store, account, claudeModelSonnet)
			if err != nil || !first.Subscription || !first.Known || first.Eligible || canLaunchExplicit(first) {
				t.Fatalf("first unknown-scope cap = %#v err=%v", first, err)
			}
			state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
			if err != nil || !found || state.Outcome != "usable" || !bytes.Equal(state.Snapshot, snapshot) || state.AllowanceModels != models || len(state.Denials) != 1 || state.Denials[0].DisplayName != "unknown" || !state.SubscriptionUntil.Equal(state.NextProbeAt) {
				t.Fatalf("completed canonical cache found=%t state=%#v err=%v", found, state, err)
			}
			second, err := runClaudeProbe(t, store, account, claudeModelFable)
			if err != nil || !second.Subscription || !second.Known || second.Eligible || canLaunchExplicit(second) {
				t.Fatalf("second model caller lost unknown-scope cap: %#v err=%v", second, err)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("second caller repeated the probe: count=%d", got)
			}
		})
	}
}

func TestClaudeLegacyProjectedDenialsUseCanonicalLabels(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	payload := json.RawMessage(fmt.Sprintf(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{`+
		`"five_hour":{"utilization":10,"resets_at":%q},`+
		`"seven_day_opus":{"utilization":100,"resets_at":%q},`+
		`"seven_day_sonnet":{"utilization":100,"resets_at":%q},`+
		`"model_scoped":[{"display_name":"Fable","utilization":20,"resets_at":%q}]}}`, reset, reset, reset, reset))
	installFakeClaude(t, binDir, payload, false, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	opus, err := runClaudeProbe(t, store, account, claudeModelOpus)
	if err != nil || !opus.Known || opus.Eligible || canLaunchExplicit(opus) {
		t.Fatalf("legacy Opus cap = %#v err=%v", opus, err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || len(state.Denials) != 2 || state.Denials[0].DisplayName != "Opus" || state.Denials[1].DisplayName != "Sonnet" {
		t.Fatalf("legacy denial cache found=%t denials=%#v err=%v", found, state.Denials, err)
	}
	sonnet, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !sonnet.Known || sonnet.Eligible {
		t.Fatalf("legacy Sonnet cap = %#v err=%v", sonnet, err)
	}
	for _, model := range []claudeModelFamily{claudeModelHaiku, claudeModelFable} {
		q, err := runClaudeProbe(t, store, account, model)
		if err != nil || !q.Known || !q.Eligible {
			t.Fatalf("legacy cap leaked into %s: %#v err=%v", model, q, err)
		}
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("legacy cap lookup started %d probes, want 1", got)
	}
}

func TestClaudeCombinedWindowLimitIsCanonicalAndPersistable(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	payload := claudeWindowPayload(time.Now().UTC().Add(24*time.Hour), 10, 100, 28)
	if len(payload) >= claudeMaxFrame {
		t.Fatalf("boundary payload size=%d exceeds frame bound", len(payload))
	}
	installFakeClaude(t, binDir, payload, false, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !q.Known || !q.Eligible {
		t.Fatalf("combined row-bound probe = %#v err=%v", q, err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || state.Outcome != "usable" {
		t.Fatalf("boundary cache found=%t outcome=%q err=%v", found, state.Outcome, err)
	}
	var usage claudeUsageResponse
	if err := json.Unmarshal(state.Snapshot, &usage); err != nil {
		t.Fatalf("decode canonical snapshot: %v", err)
	}
	rowCount := 0
	if usage.RateLimits != nil && usage.RateLimits.ModelScoped != nil {
		rowCount = len(*usage.RateLimits.ModelScoped)
	}
	if rowCount != claudeCacheMaxWindows {
		t.Fatalf("canonical row count=%d", rowCount)
	}
	canonical, _, models, usable, _, err := sanitizeClaudeQuotaPayload(state.Snapshot, state.ObservedAt)
	if err != nil || !bytes.Equal(canonical, state.Snapshot) || models != state.AllowanceModels || !usable {
		t.Fatalf("cached snapshot is not idempotent: equal=%t models=%05b/%05b usable=%t err=%v", bytes.Equal(canonical, state.Snapshot), models, state.AllowanceModels, usable, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("boundary projection started %d probes, want 1", got)
	}
}

func TestClaudeCacheReusedAcrossWrapperProcesses(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"automatic launch", []string{"claude", "--model", "sonnet", "prompt"}},
		{"explicit different model", []string{"claude", "--account", "work", "--model", "fable", "prompt"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, home, binDir, account := newFakeClaudeProfile(t)
			installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 10), false, 0, false)

			code, stdout, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "claude"})
			if code != 0 || !strings.Contains(stdout, "percentage headroom") {
				t.Fatalf("status code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("status started %d probes, want 1", got)
			}

			code, stdout, stderr = runCodatorProcess(t, home, dataHome, binDir, test.args)
			if code != 0 {
				t.Fatalf("cached launch code=%d stdout=%q stderr=%q", code, stdout, stderr)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("launch repeated the probe: count=%d", got)
			}
			if got := fakeClaudeCount(t, account, ".launch-count"); got != 1 {
				t.Fatalf("native launch count=%d, want 1", got)
			}
		})
	}
}

func TestClaudeProbeCooldownAfterNullOrTransportError(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload json.RawMessage
		fail    bool
	}{
		{"null quota keeps verified subscription", json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":null}`), false},
		{"transport error leaves reservation", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, _, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			installFakeClaude(t, binDir, test.payload, test.fail, 0, false)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

			first, err := runClaudeProbe(t, store, account, claudeModelSonnet)
			if err != nil || first.Known || !strings.Contains(first.Reason, claudeRetryPrefix) {
				t.Fatalf("first probe = %#v, err=%v", first, err)
			}
			if test.fail && first.Subscription {
				t.Fatalf("transport failure verified subscription: %#v", first)
			}
			if !test.fail && (!first.Subscription || !canLaunchExplicit(first)) {
				t.Fatalf("null quota lost existing explicit-subscription behavior: %#v", first)
			}
			if second, err := runClaudeProbe(t, store, account, claudeModelFable); err != nil || !strings.Contains(second.Reason, claudeRetryPrefix) {
				t.Fatalf("cooldown probe = %#v, err=%v", second, err)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("cooldown started %d probes, want 1", got)
			}

			state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
			if err != nil || !found {
				t.Fatalf("read cache: found=%t err=%v", found, err)
			}
			state.AttemptedAt = time.Now().UTC().Add(-16 * time.Minute)
			state.NextProbeAt = state.AttemptedAt.Add(claudeFailureCooldown)
			if state.Subscription {
				state.SubscriptionUntil = state.NextProbeAt
			}
			if err := store.writeClaudeProbeCache(account, state); err != nil {
				t.Fatal(err)
			}
			if _, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil {
				t.Fatalf("probe after fixture expiry: %v", err)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 2 {
				t.Fatalf("expired cooldown probe count=%d, want 2", got)
			}
		})
	}
}

func TestClaudeUncacheableSharedCapPersistsInReservation(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	payload := claudeWindowPayload(time.Now().UTC().Add(24*time.Hour), 100, 129, 0)
	if len(payload) >= claudeMaxFrame {
		t.Fatalf("uncacheable payload size=%d exceeds frame bound", len(payload))
	}
	installFakeClaude(t, binDir, payload, false, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	first, err := runClaudeProbe(t, store, account, claudeModelFable)
	if err != nil || !first.Subscription || !first.Known || first.Eligible || canLaunchExplicit(first) {
		t.Fatalf("uncacheable shared cap = %#v err=%v", first, err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || state.Outcome != "reserved" || len(state.Snapshot) != 0 || len(state.Denials) != 1 || state.Denials[0].DisplayName != "" {
		t.Fatalf("uncacheable reservation found=%t state=%#v err=%v", found, state, err)
	}
	second, err := runClaudeProbe(t, store, account, claudeModelFable)
	if err != nil || !second.Known || second.Eligible || canLaunchExplicit(second) {
		t.Fatalf("cooldown caller bypassed uncacheable cap: %#v err=%v", second, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("uncacheable cooldown started %d probes, want 1", got)
	}
}

func TestClaudeOriginalDeadlineBlocksAfterUncacheableWriteFailure(t *testing.T) {
	dataHome, home, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 10), false, 50*time.Millisecond, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	first, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !first.Subscription || !first.Known || !first.Eligible {
		t.Fatalf("initial delayed observation = %#v err=%v", first, err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || !state.SubscriptionUntil.Equal(state.NextProbeAt) {
		t.Fatalf("verification was extended from completion: state=%#v found=%t err=%v", state, found, err)
	}

	// Model a first response completing one second after its original five-minute request gate.
	state.AttemptedAt = state.ObservedAt.Add(-claudeSuccessCooldown - time.Second)
	state.NextProbeAt = state.AttemptedAt.Add(claudeSuccessCooldown)
	state.SubscriptionUntil = state.NextProbeAt
	originalExpiry := state.SubscriptionUntil
	if !originalExpiry.Before(state.ObservedAt) {
		t.Fatalf("fixture original deadline is not expired at completion: %#v", state)
	}
	if err := store.writeClaudeProbeCache(account, state); err != nil {
		t.Fatalf("write delayed fixture: %v", err)
	}

	installFakeClaude(t, binDir, claudeWindowPayload(time.Now().UTC().Add(24*time.Hour), 100, 129, 0), false, 0, true)
	refresh, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !refresh.Subscription || !refresh.Known || refresh.Eligible || canLaunchExplicit(refresh) {
		t.Fatalf("uncacheable refresh cap = %#v err=%v", refresh, err)
	}
	state, found, err = store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || state.Outcome != "reserved" || !state.SubscriptionUntil.Equal(originalExpiry) || state.SubscriptionUntil.After(time.Now()) || len(state.Denials) != 0 {
		t.Fatalf("failed denial write changed durable reservation: state=%#v found=%t err=%v", state, found, err)
	}

	code, stdout, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"claude", "--account", "work", "prompt"})
	if code == 0 || !strings.Contains(stderr, claudeRetryPrefix) {
		t.Fatalf("later explicit launch code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 2 {
		t.Fatalf("later launch started another probe: count=%d, want 2", got)
	}
	if got := fakeClaudeCount(t, account, ".launch-count"); got != 0 {
		t.Fatalf("later explicit launch reached native CLI %d times", got)
	}
}

func TestClaudeCrashReservationSuppressesAndShowsRetryDeadline(t *testing.T) {
	dataHome, home, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.writeClaudeProbeCache(account, newClaudeReservation(time.Now().UTC())); err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 10), false, 0, false)

	code, stdout, stderr := runCodatorProcess(t, home, dataHome, binDir, []string{"status", "claude"})
	if code != 0 || !strings.Contains(stdout, claudeRetryPrefix) {
		t.Fatalf("reserved status code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, args := range [][]string{
		{"claude", "prompt"},
		{"claude", "--account", "work", "prompt"},
	} {
		code, stdout, stderr = runCodatorProcess(t, home, dataHome, binDir, args)
		if code == 0 || !strings.Contains(stderr, claudeRetryPrefix) {
			t.Fatalf("reserved launch args=%q code=%d stdout=%q stderr=%q", args, code, stdout, stderr)
		}
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 0 {
		t.Fatalf("crash reservation started %d probes, want 0", got)
	}
}

func TestClaudeDenialRetainedAcrossUnusableRefreshes(t *testing.T) {
	nullQuota := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":null}`)
	for _, test := range []struct {
		name    string
		payload json.RawMessage
		fail    bool
	}{
		{"null quota", nullQuota, false},
		{"malformed response", json.RawMessage(`[]`), false},
		{"transport error", nil, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, _, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			observedAt := now.Add(-6 * time.Minute)
			snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(
				fullClaudeUsagePayload(now.Add(24*time.Hour), 100), observedAt)
			if err != nil || !subscription || !usable || len(denials) != 1 {
				t.Fatalf("seed projection: subscription=%t usable=%t denials=%#v err=%v", subscription, usable, denials, err)
			}
			seed := claudeProbeCache{
				Version: claudeCacheVersion, AttemptedAt: observedAt,
				NextProbeAt: observedAt.Add(claudeSuccessCooldown), Outcome: "usable",
				ObservedAt: observedAt, Subscription: subscription,
				SubscriptionUntil: observedAt.Add(claudeSuccessCooldown),
				AllowanceModels:   models, Denials: denials, Snapshot: snapshot,
			}
			if err := store.writeClaudeProbeCache(account, seed); err != nil {
				t.Fatalf("write seed cache: %v", err)
			}
			installFakeClaude(t, binDir, test.payload, test.fail, 0, false)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

			q, err := runClaudeProbe(t, store, account, claudeModelFable)
			if err != nil || !q.Known || q.Eligible || !strings.Contains(q.Reason, "all applicable usage headroom is exhausted") {
				t.Fatalf("unusable refresh lost prior denial: %#v err=%v", q, err)
			}
			if canLaunchExplicit(q) {
				t.Fatalf("explicit launch bypassed retained denial: %#v", q)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
				t.Fatalf("refresh probe count=%d, want 1", got)
			}
			if q, err := runClaudeProbe(t, store, account, claudeModelFable); err != nil || !q.Known || q.Eligible {
				t.Fatalf("retained denial disappeared during backoff: %#v err=%v", q, err)
			}
			state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
			if err != nil || !found || len(state.Denials) != 1 {
				t.Fatalf("denial cache found=%t denials=%#v err=%v", found, state.Denials, err)
			}
		})
	}
}

func TestClaudeAllowanceCacheIsModelSpecificWhenGlobalResultIsUnknown(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	reset := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	payload := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{` +
		`"five_hour":{"utilization":0,"resets_at":"` + reset + `"},` +
		`"model_scoped":[` +
		`{"display_name":"Sonnet","utilization":20,"resets_at":"` + reset + `"},` +
		`{"display_name":"Fable","utilization":60,"resets_at":"` + reset + `"},` +
		`{"display_name":"Haiku","utilization":"bad","resets_at":"` + reset + `"}]}}`)
	installFakeClaude(t, binDir, payload, false, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	sonnet, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !sonnet.Known || !sonnet.Eligible || sonnet.Headroom != 80 {
		t.Fatalf("initial Sonnet quota = %#v err=%v", sonnet, err)
	}
	fable, err := runClaudeProbe(t, store, account, claudeModelFable)
	if err != nil || !fable.Known || !fable.Eligible || fable.Headroom != 40 {
		t.Fatalf("cached Fable quota = %#v err=%v", fable, err)
	}
	haiku, err := runClaudeProbe(t, store, account, claudeModelHaiku)
	if err != nil || haiku.Known || haiku.Eligible || !haiku.Subscription || !strings.Contains(haiku.Reason, claudeRetryPrefix) {
		t.Fatalf("malformed Haiku scope became usable: %#v err=%v", haiku, err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || state.Outcome != "unknown" || state.AllowanceModels&claudeSonnetModelMask == 0 || state.AllowanceModels&claudeFableModelMask == 0 || state.AllowanceModels&claudeHaikuModelMask != 0 {
		t.Fatalf("model mask state=%#v found=%t err=%v", state, found, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("model switching started %d probes, want 1", got)
	}
}

func TestClaudeTransportFailureDoesNotRenewCompletedSubscriptionExpiry(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	attemptedAt := time.Now().UTC().Add(-16 * time.Minute)
	observedAt := attemptedAt.Add(10 * time.Second)
	payload := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":null}`)
	snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(payload, observedAt)
	if err != nil || !subscription || usable || models != 0 {
		t.Fatalf("seed projection sub=%t models=%05b usable=%t err=%v", subscription, models, usable, err)
	}
	expiry := attemptedAt.Add(claudeFailureCooldown)
	seed := claudeProbeCache{
		Version: claudeCacheVersion, AttemptedAt: attemptedAt, NextProbeAt: expiry,
		Outcome: "unknown", ObservedAt: observedAt, Subscription: true,
		SubscriptionUntil: expiry, AllowanceModels: models, Denials: denials, Snapshot: snapshot,
	}
	if err := store.writeClaudeProbeCache(account, seed); err != nil {
		t.Fatalf("write seed cache: %v", err)
	}
	installFakeClaude(t, binDir, nil, true, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || q.Subscription || !strings.Contains(q.Reason, claudeRetryPrefix) {
		t.Fatalf("failed retry renewed subscription: %#v err=%v", q, err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || !state.Subscription || !state.SubscriptionUntil.Equal(expiry) {
		t.Fatalf("stored subscription expiry changed: state=%#v found=%t err=%v", state, found, err)
	}
}

func TestClaudeProbeScoresResetAtResponseCompletion(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(250*time.Millisecond), 0), false, 750*time.Millisecond, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || q.Known || q.Eligible || !q.Subscription {
		t.Fatalf("window that reset during probe was scored as usable: %#v err=%v", q, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("probe count=%d, want 1", got)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || state.Outcome != "unknown" || !state.NextProbeAt.After(time.Now()) {
		t.Fatalf("completed reset state=%#v found=%t err=%v", state, found, err)
	}
}

func TestClaudeLoginInvalidatesObservationButKeepsCooldown(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().UTC().Add(-time.Minute)
	snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(
		fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 100), observedAt)
	if err != nil || !subscription || !usable || len(denials) == 0 {
		t.Fatalf("seed projection sub=%t usable=%t denials=%#v err=%v", subscription, usable, denials, err)
	}
	state := claudeProbeCache{
		Version: claudeCacheVersion, AttemptedAt: observedAt,
		NextProbeAt: observedAt.Add(claudeSuccessCooldown), Outcome: "usable",
		ObservedAt: observedAt, Subscription: true,
		SubscriptionUntil: observedAt.Add(claudeSuccessCooldown),
		AllowanceModels:   models, Denials: denials, Snapshot: snapshot,
	}
	if err := store.writeClaudeProbeCache(account, state); err != nil {
		t.Fatalf("write seed cache: %v", err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 10), false, 0, false)
	if err := store.invalidateClaudeCacheOnLogin(account); err != nil {
		t.Fatalf("invalidate login observation: %v", err)
	}
	cleared, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || !cleared.AttemptedAt.Equal(state.AttemptedAt) || !cleared.NextProbeAt.Equal(state.NextProbeAt) ||
		len(cleared.Snapshot) != 0 || cleared.Subscription || !cleared.SubscriptionUntil.IsZero() || cleared.AllowanceModels != 0 || len(cleared.Denials) != 0 {
		t.Fatalf("login cache state=%#v found=%t err=%v", cleared, found, err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	if q, err := runClaudeProbe(t, store, account, claudeModelSonnet); err != nil || q.Subscription || !strings.Contains(q.Reason, claudeRetryPrefix) {
		t.Fatalf("login discarded cooldown: quota=%#v err=%v", q, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 0 {
		t.Fatalf("login cooldown started %d probes, want 0", got)
	}
}

func TestClaudeCorruptAndUnsafeCacheNeverProbe(t *testing.T) {
	for _, test := range []struct {
		name   string
		unsafe bool
	}{
		{"corrupt", false},
		{"unsafe permissions", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dataHome, _, binDir, account := newFakeClaudeProfile(t)
			store, err := newStore(dataHome)
			if err != nil {
				t.Fatal(err)
			}
			cachePath := filepath.Join(filepath.Dir(account.NativeDir), claudeCacheName)
			if test.unsafe {
				if err := store.writeClaudeProbeCache(account, newClaudeReservation(time.Now().UTC())); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(cachePath, 0644); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(cachePath, []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
			installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 10), false, 0, false)
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

			q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
			if test.unsafe && err == nil {
				t.Fatalf("unsafe cache was accepted: %#v", q)
			}
			if !test.unsafe && (err != nil || !strings.Contains(q.Reason, claudeRetryPrefix)) {
				t.Fatalf("corrupt cache did not install a suppressing reservation: %#v err=%v", q, err)
			}
			if got := fakeClaudeCount(t, account, ".probe-count"); got != 0 {
				t.Fatalf("invalid cache started %d probes, want 0", got)
			}
		})
	}
}

func TestClaudeOldObservationExpiresWithoutCorruptingCache(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	observedAt := time.Now().UTC().Add(-9 * 24 * time.Hour)
	payload := fullClaudeUsagePayload(observedAt.Add(7*24*time.Hour), 0)
	snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(payload, observedAt)
	if err != nil || !subscription || !usable {
		t.Fatalf("old seed projection sub=%t usable=%t err=%v", subscription, usable, err)
	}
	state := claudeProbeCache{
		Version: claudeCacheVersion, AttemptedAt: observedAt,
		NextProbeAt: observedAt.Add(claudeSuccessCooldown), Outcome: "usable",
		ObservedAt: observedAt, Subscription: subscription,
		SubscriptionUntil: observedAt.Add(claudeSuccessCooldown),
		AllowanceModels:   models, Denials: denials, Snapshot: snapshot,
	}
	if err := store.writeClaudeProbeCache(account, state); err != nil {
		t.Fatalf("write old cache: %v", err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 0), false, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !q.Known || !q.Eligible {
		t.Fatalf("old observation blocked fresh probe: %#v err=%v", q, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("old cache caused %d probes, want one fresh attempt", got)
	}
}

func TestClaudeFarFutureExhaustionSurvivesUnusableRefresh(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	observedAt := now.Add(-9 * 24 * time.Hour)
	resetAt := time.Date(2099, time.January, 1, 0, 0, 0, 0, time.UTC)
	snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(fullClaudeUsagePayload(resetAt, 100), observedAt)
	if err != nil || !subscription || !usable || len(denials) != 1 || !denials[0].ResetsAt.Equal(resetAt) {
		t.Fatalf("far-future seed subscription=%t usable=%t denials=%#v err=%v", subscription, usable, denials, err)
	}
	state := claudeProbeCache{
		Version: claudeCacheVersion, AttemptedAt: observedAt,
		NextProbeAt: observedAt.Add(claudeSuccessCooldown), Outcome: "usable",
		ObservedAt: observedAt, Subscription: subscription,
		SubscriptionUntil: observedAt.Add(claudeSuccessCooldown),
		AllowanceModels:   models, Denials: denials, Snapshot: snapshot,
	}
	if err := store.writeClaudeProbeCache(account, state); err != nil {
		t.Fatalf("write far-future cache: %v", err)
	}
	installFakeClaude(t, binDir, json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":null}`), false, 0, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	q, err := runClaudeProbe(t, store, account, claudeModelFable)
	if err != nil || !q.Known || q.Eligible || !strings.Contains(q.Reason, "all applicable usage headroom is exhausted") {
		t.Fatalf("old far-future cap did not survive unusable refresh: %#v err=%v", q, err)
	}
	if canLaunchExplicit(q) {
		t.Fatalf("explicit launch bypassed old far-future cap: %#v", q)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("refresh probe count=%d, want 1", got)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || len(state.Denials) != 1 || !state.Denials[0].ResetsAt.Equal(resetAt) {
		t.Fatalf("far-future denial cache found=%t state=%#v err=%v", found, state, err)
	}
}

func TestClaudeFailedResultWriteReportsDurableReservationDeadline(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 0), false, 0, true)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || q.Known || q.Subscription {
		t.Fatalf("failed result write returned new cached result: %#v err=%v", q, err)
	}
	durable, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || durable.Outcome != "reserved" {
		t.Fatalf("durable reservation=%#v found=%t err=%v", durable, found, err)
	}
	if want := claudeRetryReason(durable.NextProbeAt); !strings.Contains(q.Reason, want) {
		t.Fatalf("reported retry does not match durable deadline: reason=%q durable=%q", q.Reason, want)
	}
	_ = os.Remove(filepath.Join(filepath.Dir(account.NativeDir), claudeCacheTemp))
}

func TestClaudeCancelledProbeKeepsReservationAndSuppressesRetry(t *testing.T) {
	dataHome, _, binDir, account := newFakeClaudeProfile(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	installFakeClaude(t, binDir, fullClaudeUsagePayload(time.Now().UTC().Add(24*time.Hour), 0), false, 5*time.Second, false)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(100*time.Millisecond, cancel)
	defer timer.Stop()
	if _, err := runClaudeProbeContext(t, store, account, claudeModelSonnet, ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled probe error=%v, want context canceled", err)
	}
	state, found, err := store.readClaudeProbeCache(account, time.Now().UTC())
	if err != nil || !found || state.Outcome != "reserved" || !state.NextProbeAt.After(time.Now()) {
		t.Fatalf("cancelled probe lost reservation: state=%#v found=%t err=%v", state, found, err)
	}
	q, err := runClaudeProbe(t, store, account, claudeModelSonnet)
	if err != nil || !strings.Contains(q.Reason, claudeRetryPrefix) {
		t.Fatalf("cancelled probe retried instead of suppressing: %#v err=%v", q, err)
	}
	if got := fakeClaudeCount(t, account, ".probe-count"); got != 1 {
		t.Fatalf("cancellation retry started %d probes, want 1", got)
	}
}

func newFakeClaudeProfile(t *testing.T) (dataHome, home, binDir string, account Account) {
	t.Helper()
	dataHome = tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err = store.EnsureAccount("claude", "work")
	if err != nil {
		t.Fatal(err)
	}
	home = t.TempDir()
	binDir = filepath.Join(home, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	return dataHome, home, binDir, account
}

func fullClaudeUsagePayload(reset time.Time, fiveHour float64) json.RawMessage {
	resetText := reset.Format(time.RFC3339Nano)
	return json.RawMessage(fmt.Sprintf(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{`+
		`"five_hour":{"utilization":%g,"resets_at":%q},`+
		`"seven_day":{"utilization":20,"resets_at":%q},`+
		`"seven_day_oauth_apps":{"utilization":10,"resets_at":%q},`+
		`"seven_day_opus":{"utilization":30,"resets_at":%q},`+
		`"seven_day_sonnet":{"utilization":40,"resets_at":%q},`+
		`"model_scoped":[`+
		`{"display_name":"Opus","utilization":30,"resets_at":%q},`+
		`{"display_name":"Sonnet","utilization":40,"resets_at":%q},`+
		`{"display_name":"Haiku","utilization":10,"resets_at":%q},`+
		`{"display_name":"Fable","utilization":50,"resets_at":%q}]}}`,
		fiveHour, resetText, resetText, resetText, resetText, resetText, resetText, resetText, resetText, resetText))
}

func claudeWindowPayload(reset time.Time, fiveHour float64, projectedRows, rawRows int) json.RawMessage {
	resetText := reset.Format(time.RFC3339Nano)
	var payload strings.Builder
	fmt.Fprintf(&payload, `{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":%g,"resets_at":%q},"model_scoped":[`, fiveHour, resetText)
	for i := 0; i < projectedRows; i++ {
		if i != 0 {
			payload.WriteByte(',')
		}
		name := fmt.Sprintf("Fixture projected %d", i)
		if i == 0 {
			name = "Fable"
		}
		fmt.Fprintf(&payload, `{"display_name":%q,"utilization":0,"resets_at":%q}`, name, resetText)
	}
	payload.WriteString(`],"limits":[`)
	for i := 0; i < rawRows; i++ {
		if i != 0 {
			payload.WriteByte(',')
		}
		fmt.Fprintf(&payload, `{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":%q}},"resets_at":%q}`, fmt.Sprintf("Fixture raw %d", i), resetText)
	}
	payload.WriteString(`]}}`)
	return json.RawMessage(payload.String())
}

func installFakeClaude(t *testing.T, binDir string, payload json.RawMessage, fail bool, delay time.Duration, breakCacheWrite bool) {
	t.Helper()
	initFrame := `{"type":"control_response","response":{"subtype":"success","request_id":"init-1","response":{}}}`
	usageFrame := `{"type":"control_response","response":{"subtype":"success","request_id":"usage-1","response":` + string(payload) + `}}`
	var script strings.Builder
	script.WriteString("#!/bin/sh\nprofile=${CLAUDE_CONFIG_DIR%/native}\n")
	script.WriteString("if [ \"$1\" = \"--print\" ]; then\n  printf 'probe\\n' >> \"$profile/.probe-count\"\n")
	if fail {
		script.WriteString("  exit 91\nfi\nprintf 'launch\\n' >> \"$profile/.launch-count\"\nexit 0\n")
	} else {
		script.WriteString("  while IFS= read -r line; do\n    case \"$line\" in\n")
		script.WriteString("      *init-1*) printf '%s\\n' " + shellQuote(initFrame) + " ;;\n")
		script.WriteString("      *usage-1*)\n")
		if delay > 0 {
			script.WriteString(fmt.Sprintf("        sleep %.3f\n", delay.Seconds()))
		}
		if breakCacheWrite {
			script.WriteString("        ln -s /dev/null \"$profile/.claude-usage.tmp\" 2>/dev/null || true\n")
		}
		script.WriteString("        printf '%s\\n' " + shellQuote(usageFrame) + "\n        exit 0\n        ;;\n")
		script.WriteString("    esac\n  done\n  exit 92\nfi\nprintf 'launch\\n' >> \"$profile/.launch-count\"\nexit 0\n")
	}
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte(script.String()), 0700); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func runCodatorProcess(t *testing.T, home, dataHome, binDir string, args []string) (int, string, string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestCodatorPassthroughChild$")
	cmd.Env = []string{
		"HOME=" + home,
		"PATH=" + binDir + string(os.PathListSeparator) + "/usr/bin:/bin",
		"XDG_DATA_HOME=" + dataHome,
		"CODATOR_PASSTHROUGH_CHILD=1",
		"CODATOR_TEST_ARGS=" + string(argsJSON),
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	t.Fatalf("Codator subprocess failed to start: %v", err)
	return -1, stdout.String(), stderr.String()
}

func fakeClaudeCount(t *testing.T, account Account, filename string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(filepath.Dir(account.NativeDir), filename))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(data), "\n")
}

func runClaudeProbe(t *testing.T, store *Store, account Account, model claudeModelFamily) (quota, error) {
	return runClaudeProbeContext(t, store, account, model, context.Background())
}

func runClaudeProbeContext(t *testing.T, store *Store, account Account, model claudeModelFamily, ctx context.Context) (quota, error) {
	t.Helper()
	lock, err := store.Lock("claude", account.Name)
	if err != nil {
		return quota{}, err
	}
	defer lock.Close()
	return store.probeClaudeAccount(ctx, account, model)
}
