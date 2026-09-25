package main

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestStatusJSONCodexAccountStates(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	soon, later := now.Add(time.Hour), now.Add(5*time.Hour)
	availableQuota := quota{
		Known:        true,
		Eligible:     true,
		Headroom:     8.5,
		ResetCredits: 2,
		Windows:      []usageWindow{{Label: "Weekly", Used: 91.5, ResetsAt: later}},
	}
	available := quotaStatusAccount("codex", "ready", availableQuota, nil, now, statusDetails{Windows: codexStatusWindows(availableQuota.Windows)})
	if available.State != "available" || !available.Launchable || available.HeadroomPercent == nil || *available.HeadroomPercent != 8.5 || available.ResetCredits != 2 {
		t.Fatalf("available account = %+v", available)
	}

	exhaustedQuota := quota{
		Known:   true,
		Reason:  "all applicable usage headroom is exhausted",
		Windows: []usageWindow{{Label: "Daily", Used: 100, ResetsAt: soon}, {Label: "Weekly", Used: 100, ResetsAt: later}},
	}
	exhausted := quotaStatusAccount("codex", "spent", exhaustedQuota, nil, now, statusDetails{Windows: codexStatusWindows(exhaustedQuota.Windows)})
	if exhausted.State != "exhausted" || exhausted.Launchable || exhausted.AvailableAt == nil || !exhausted.AvailableAt.Equal(later) {
		t.Fatalf("exhausted account = %+v, want latest reset %s", exhausted, later)
	}
	denialUntil := now.Add(7 * time.Hour)
	denialExhausted := quotaStatusAccount("claude", "denial", quota{Known: true}, nil, now, statusDetails{
		Windows:        []statusWindow{{Label: "Weekly", Exhausted: true, ResetsAt: statusTime(soon), resetForState: soon}},
		KnownExhausted: []statusKnownExhausted{{Scope: "Weekly (Haiku)", Until: denialUntil, ObservedAt: now.Add(-time.Minute)}},
	})
	if denialExhausted.State != "exhausted" || denialExhausted.AvailableAt == nil || !denialExhausted.AvailableAt.Equal(denialUntil) {
		t.Fatalf("known Claude denial was not included in latest available_at: %+v", denialExhausted)
	}

	busy := unavailableStatusAccount("codex", "busy", "busy", "", "busy")
	unknown := quotaStatusAccount("codex", "unknown", quota{Reason: "usage response is malformed"}, nil, now, statusDetails{})
	probeFailed := quotaStatusAccount("codex", "failed", quota{}, errors.New("probe failed"), now, statusDetails{})
	if busy.State != "busy" || busy.Launchable || unknown.State != "unknown" || unknown.Reason != "usage response is malformed" || probeFailed.State != "unknown" || probeFailed.Reason != "usage check failed" {
		t.Fatalf("busy/unknown states: busy=%+v unknown=%+v failed=%+v", busy, unknown, probeFailed)
	}

	report := statusReport{
		SchemaVersion: 1,
		GeneratedAt:   now,
		Providers:     []statusProvider{{Provider: "codex", Status: "ok"}},
		Accounts:      []statusAccount{available, exhausted, busy, unknown, probeFailed},
	}
	var output strings.Builder
	if err := renderStatusJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	var decoded statusReport
	if err := json.Unmarshal([]byte(output.String()), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, output.String())
	}
	if len(decoded.Accounts) != 5 || decoded.Accounts[0].State != "available" || decoded.Accounts[0].HeadroomPercent == nil || *decoded.Accounts[0].HeadroomPercent != 8.5 || decoded.Accounts[1].State != "exhausted" || decoded.Accounts[1].AvailableAt == nil || !decoded.Accounts[1].AvailableAt.Equal(later) || decoded.Accounts[2].State != "busy" || decoded.Accounts[3].State != "unknown" || decoded.Accounts[4].State != "unknown" {
		t.Fatalf("Codex JSON account states = %+v", decoded.Accounts)
	}
}

func TestStatusJSONIncludesClaudeCacheDetailsAndAbsoluteTimes(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	observedAt := now.Add(-time.Minute)
	nextProbeAt := now.Add(4 * time.Minute)
	denialObservedAt := now.Add(-10 * time.Minute)
	denialUntil := now.Add(2 * time.Hour)
	snapshot := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":25,"resets_at":"2026-09-25T17:00:00Z"},"seven_day":{"utilization":50,"resets_at":"2026-09-30T12:00:00Z"}}}`)
	state := claudeProbeCache{
		Outcome:      "usable",
		ObservedAt:   observedAt,
		NextProbeAt:  nextProbeAt,
		Subscription: true,
		Snapshot:     snapshot,
		Denials:      []claudeCachedDenial{{DisplayName: "Haiku", ResetsAt: denialUntil, ObservedAt: denialObservedAt}},
	}
	claudeData := claudeStatusDetails(state, true, nil, now)
	wantText := "last observed at Fri Sep 25 11:59 (1m ago)\n" +
		"  Current session: 75.0% remaining, resets Fri Sep 25 17:00 (in 5h 0m)\n" +
		"  Weekly (all models): 50.0% remaining, resets Wed Sep 30 12:00 (in 5d 0h)\n" +
		"  Weekly (Fable): unavailable (not reported)\n" +
		"  known exhausted: Weekly (Haiku) until Fri Sep 25 14:00 (in 2h 0m) (last observed at Fri Sep 25 11:50 (10m ago))"
	if claudeData.Text != wantText {
		t.Fatalf("Claude text = %q, want %q", claudeData.Text, wantText)
	}
	jsonState := state
	jsonState.ObservedAt = observedAt.Add(123 * time.Millisecond)
	jsonState.NextProbeAt = nextProbeAt.Add(456 * time.Millisecond)
	jsonState.Denials = []claudeCachedDenial{{
		DisplayName: "Haiku",
		ResetsAt:    denialUntil.Add(789 * time.Millisecond),
		ObservedAt:  denialObservedAt.Add(321 * time.Millisecond),
	}}
	jsonClaudeData := claudeStatusDetails(jsonState, true, nil, now)
	q, err := parseClaudeQuotaForModel(snapshot, now, "")
	if err != nil {
		t.Fatal(err)
	}
	claude := quotaStatusAccount("claude", "work", q, nil, now, statusDetails{
		Windows:        jsonClaudeData.Windows,
		KnownExhausted: jsonClaudeData.KnownExhausted,
		ObservedAt:     statusTime(jsonState.ObservedAt),
		NextProbeAt:    statusFutureTime(jsonState.NextProbeAt, now),
	})
	report := statusReport{
		SchemaVersion: 1,
		GeneratedAt:   statusUTCSecond(now.Add(987 * time.Millisecond)),
		Providers:     []statusProvider{{Provider: "claude", Status: "ok"}},
		Accounts:      []statusAccount{claude},
	}
	var output strings.Builder
	if err := renderStatusJSON(&output, report); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(output.String()))
	var decoded statusReport
	if err := decoder.Decode(&decoded); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, output.String())
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("JSON output has trailing document/data: %v", err)
	}
	if decoded.SchemaVersion != 1 || decoded.GeneratedAt.Location() != time.UTC || decoded.GeneratedAt.Nanosecond() != 0 || len(decoded.Accounts) != 1 {
		t.Fatalf("decoded report = %+v", decoded)
	}
	got := decoded.Accounts[0]
	if got.State != "available" || !got.Launchable || got.HeadroomPercent == nil || math.Abs(*got.HeadroomPercent-50) > 0.0001 {
		t.Fatalf("Claude quota fields = %+v", got)
	}
	if got.ObservedAt == nil || !got.ObservedAt.Equal(observedAt) || got.ObservedAt.Nanosecond() != 0 || got.NextProbeAt == nil || !got.NextProbeAt.Equal(nextProbeAt) || got.NextProbeAt.Nanosecond() != 0 {
		t.Fatalf("Claude timestamps: observed=%v next_probe=%v", got.ObservedAt, got.NextProbeAt)
	}
	if len(got.Windows) != 3 || got.Windows[0].Label != "Current session" || got.Windows[0].UsedPercent == nil || *got.Windows[0].UsedPercent != 25 || got.Windows[0].RemainingPercent == nil || *got.Windows[0].RemainingPercent != 75 {
		t.Fatalf("Claude windows = %+v", got.Windows)
	}
	if got.Windows[2].Label != "Weekly (Fable)" || got.Windows[2].Unavailable != "not reported" || len(got.KnownExhausted) != 1 || got.KnownExhausted[0].Scope != "Weekly (Haiku)" || !got.KnownExhausted[0].Until.Equal(denialUntil) || got.KnownExhausted[0].Until.Nanosecond() != 0 || !got.KnownExhausted[0].ObservedAt.Equal(denialObservedAt) || got.KnownExhausted[0].ObservedAt.Nanosecond() != 0 {
		t.Fatalf("Claude cache details = windows:%+v denials:%+v", got.Windows, got.KnownExhausted)
	}
	for _, window := range got.Windows {
		if window.ResetsAt != nil && window.ResetsAt.Nanosecond() != 0 {
			t.Errorf("window reset retained fractional seconds: %+v", window)
		}
	}
	for _, relative := range []string{"(in ", " ago)", "resets Mon", "last observed at"} {
		if strings.Contains(output.String(), relative) {
			t.Errorf("JSON output contains human time %q:\n%s", relative, output.String())
		}
	}
}

func TestStatusReasonTruncatesClaudeCooldownTimestamp(t *testing.T) {
	input := "Claude usage probe cooling down; retry after 2026-09-25T17:30:01.987654321Z"
	want := "Claude usage probe cooling down; retry after 2026-09-25T17:30:01Z"
	row := quotaStatusAccount("claude", "cooldown", quota{Reason: input}, nil, time.Now(), statusDetails{})
	if row.Reason != want {
		t.Fatalf("status reason = %q, want %q", row.Reason, want)
	}
}

func TestCollectStatusGeneratedAtUsesWholeSeconds(t *testing.T) {
	report, err := collectStatus(&Store{root: t.TempDir()}, []string{"codex"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.GeneratedAt.Location() != time.UTC || report.GeneratedAt.Nanosecond() != 0 {
		t.Fatalf("generated_at = %s", report.GeneratedAt.Format(time.RFC3339Nano))
	}
}

func TestStatusTextRendererKeepsQuotaFormat(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	q := quota{Known: true, Eligible: true, Headroom: 25, Windows: []usageWindow{{Label: "Weekly", Used: 75}}}
	row := quotaStatusAccount("codex", "work", q, nil, now, statusDetails{Windows: codexStatusWindows(q.Windows)})
	var output strings.Builder
	renderStatusEvent(&output, statusProvider{Provider: "codex", Status: "ok"}, &row)
	if got, want := output.String(), "codex work: 25.0% headroom\n  Weekly: 25.0% remaining\n"; got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}

func TestStatusTextKeepsProviderErrorLine(t *testing.T) {
	var output strings.Builder
	if err := status(&Store{root: t.TempDir()}, &output, "invalid"); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "invalid: unavailable (unknown provider \"invalid\")\n"; got != want {
		t.Fatalf("status text = %q, want %q", got, want)
	}
}
