package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func watchFixture(now time.Time) statusReport {
	reset := now.Add(time.Hour)
	observed := now.Add(-time.Minute)
	login := now.Add(2 * 24 * time.Hour)
	used, remaining := 75.0, 25.0
	window := statusWindow{Label: "Weekly limit", UsedPercent: &used, RemainingPercent: &remaining, ResetsAt: &reset}
	claudeWindow := window
	claudeWindow.Label = "Weekly (all models)"
	return statusReport{SchemaVersion: 1, GeneratedAt: now, Providers: []statusProvider{{Provider: "codex", Status: "ok"}, {Provider: "claude", Status: "ok"}}, Accounts: []statusAccount{
		{Provider: "codex", Account: "work", Email: "work@example.invalid", State: "available", Launchable: true, HeadroomPercent: &remaining, CreditsBalance: "42.50", ResetCredits: 2, Windows: []statusWindow{window, window}},
		{Provider: "codex", Account: "busy", State: "busy"},
		{Provider: "codex", Account: "unknown", State: "unknown", Reason: "usage check failed"},
		{Provider: "claude", Account: "personal", Email: "person@example.invalid", State: "available", Launchable: true, HeadroomPercent: &remaining, ObservedAt: &observed, LoginExpiresAt: &login, Windows: []statusWindow{{Label: "Current session", UsedPercent: &used, RemainingPercent: &remaining, ResetsAt: &reset}, claudeWindow, {Label: "Weekly (Fable)", Unavailable: "not reported"}}},
		{Provider: "claude", Account: "exhausted", State: "exhausted", Reason: "specific denial", ObservedAt: &observed, claudeUnavailable: "fixture usage unavailable", NextProbeAt: &reset, KnownExhausted: []statusKnownExhausted{{Scope: "Fable", Until: reset, ObservedAt: observed}}},
	}}
}

func TestWatchGateAndRetention(t *testing.T) {
	now := time.Now()
	wall := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := watchState{next: now}
	if !s.begin(now, wall) {
		t.Fatal("first attempt gated")
	}
	for _, d := range []time.Duration{0, watchInterval, 24 * time.Hour} {
		if s.begin(now.Add(d), wall) {
			t.Fatal("overlapping collection")
		}
	}
	s.finish(watchFixture(wall), nil, now.Add(120*time.Second), wall.Add(120*time.Second))
	for i := 0; i < 100; i++ {
		if s.begin(now.Add(419*time.Second), wall) {
			t.Fatal("manual request bypassed completion gate")
		}
	}
	if !s.begin(now.Add(420*time.Second), wall) {
		t.Fatal("success gate boundary")
	}
	before := s.snapshot
	s.finish(statusReport{}, context.DeadlineExceeded, now.Add(430*time.Second), wall.Add(430*time.Second))
	if s.snapshot != before || s.lastSuccess != wall.Add(120*time.Second) || s.errorText == "" {
		t.Fatal("failed partial attempt replaced success")
	}
	if s.begin(now.Add(1329*time.Second), wall) || !s.begin(now.Add(1330*time.Second), wall) {
		t.Fatal("900s failure gate boundary")
	}
	s.finish(statusReport{Providers: []statusProvider{{Provider: "codex", Status: "unavailable"}}}, nil, now.Add(1340*time.Second), wall)
	if s.errorText != "" || s.snapshot.report.Providers[0].Status != "unavailable" {
		t.Fatal("nil-error uncertainty reclassified")
	}
	if !s.begin(now.Add(48*time.Hour), wall) || s.begin(now.Add(48*time.Hour), wall) {
		t.Fatal("suspend caused catch-up/overlap")
	}
	s.disabled = true
	s.finish(statusReport{}, nil, now, wall)
	if s.begin(now.Add(72*time.Hour), wall) {
		t.Fatal("refresh after cleanup uncertainty")
	}
}

func TestWatchCleanupGraceAndLateResult(t *testing.T) {
	now := time.Now()
	wall := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := watchState{}
	s.finish(watchFixture(wall), nil, now, wall)
	retained := s.snapshot
	if s.checkCleanup(now, context.DeadlineExceeded) {
		t.Fatal("idle cleanup started")
	}
	if !s.begin(now.Add(watchInterval), wall) {
		t.Fatal("next collection gated")
	}
	expired := now.Add(watchInterval + watchDeadline)
	if s.checkCleanup(expired, nil) || !s.checkCleanup(expired, context.DeadlineExceeded) {
		t.Fatal("timeout cleanup transition")
	}
	if s.disabled || s.checkCleanup(expired.Add(watchCleanupGrace-time.Nanosecond), context.DeadlineExceeded) {
		t.Fatal("cleanup grace shortened")
	}
	if !s.checkCleanup(expired.Add(watchCleanupGrace), context.DeadlineExceeded) || !s.disabled || s.snapshot != retained {
		t.Fatal("uncertain cleanup did not disable/retain")
	}
	s.finish(statusReport{}, nil, expired.Add(watchCleanupGrace+time.Second), wall)
	if s.snapshot != retained || s.begin(now.Add(24*time.Hour), wall) {
		t.Fatal("late result restarted uncertain session")
	}
}

func TestWatchStaleAndNext(t *testing.T) {
	wall := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	now := time.Now()
	s := watchState{}
	s.finish(watchFixture(wall), nil, now, wall)
	if s.stale(now, wall) || s.nextAccount("claude", now, wall) != "personal" {
		t.Fatal("absent informational Fable window excluded candidate")
	}
	if !s.stale(now.Add(300*time.Second), wall.Add(300*time.Second)) || s.nextAccount("codex", now.Add(300*time.Second), wall.Add(300*time.Second)) != "unknown / stale" {
		t.Fatal("stale report resurrected")
	}
	if !s.failed(now.Add(time.Minute), wall.Add(70*time.Second)) || !s.failed(now.Add(time.Minute), wall.Add(-time.Minute)) {
		t.Fatal("clock jump hidden")
	}
	original := s.snapshot.report
	for _, change := range []func(*statusAccount){
		func(a *statusAccount) { a.ObservedAt = nil },
		func(a *statusAccount) {
			a.Windows = append([]statusWindow(nil), a.Windows...)
			a.Windows[0].ResetsAt = &wall
		},
		func(a *statusAccount) { future := wall.Add(time.Second); a.ObservedAt = &future },
	} {
		s.snapshot.report = original
		s.snapshot.report.Accounts = append([]statusAccount(nil), original.Accounts...)
		change(&s.snapshot.report.Accounts[3])
		if s.nextAccount("claude", now, wall) != "unknown / stale" {
			t.Fatal("provider-wide stale suppression missing")
		}
	}
	s.snapshot.report = original
	for _, bad := range []statusWindow{
		{Label: "Weekly (Fable)", Unavailable: "not reported", Exhausted: true},
		{Label: "Weekly (Fable)", Unavailable: "Not reported"},
		{Label: "Weekly (Fable)", Unavailable: "not reported", RemainingPercent: original.Accounts[3].HeadroomPercent},
	} {
		s.snapshot.report.Accounts = append([]statusAccount(nil), original.Accounts...)
		s.snapshot.report.Accounts[3].Windows = append([]statusWindow(nil), original.Accounts[3].Windows...)
		s.snapshot.report.Accounts[3].Windows[2] = bad
		if s.nextAccount("claude", now, wall) != "none known" {
			t.Fatal("invalid Fable admitted")
		}
	}
	s.snapshot.report = original
	s.snapshot.report.Accounts = append([]statusAccount(nil), original.Accounts...)
	a := original.Accounts[0]
	a.Account = "aa"
	s.snapshot.report.Accounts = append(s.snapshot.report.Accounts, a)
	if s.nextAccount("codex", now, wall) != "aa" {
		t.Fatal("headroom tie not by raw label")
	}
	zero := 0.0
	for i := range s.snapshot.report.Accounts {
		a := &s.snapshot.report.Accounts[i]
		if a.State == "available" && a.Provider == "codex" {
			a.HeadroomPercent = &zero
			a.OnCredits = true
			a.CreditsBalance = "999"
		}
	}
	if s.nextAccount("codex", now, wall) != "aa" {
		t.Fatal("credit balance influenced estimate")
	}
	s.errorText = "collection failed"
	if s.nextAccount("codex", now, wall) != "unknown / stale" {
		t.Fatal("failed collection supplied estimate")
	}
}

func TestWatchSanitizationIsDisplayOnly(t *testing.T) {
	now := time.Now().UTC()
	r := watchFixture(now)
	a := r.Accounts[3]
	a.Account = "raw\x1b[2J"
	a.Email = "email\r\n@example.invalid"
	a.Reason = "reason\u202e"
	a.claudeUnavailable = "private\x00"
	a.Windows = append([]statusWindow(nil), a.Windows...)
	a.Windows[0].Label = "window\x1b"
	a.Windows[0].Unavailable = "bad\n"
	a.KnownExhausted = []statusKnownExhausted{{Scope: "scope\x1b", Until: now.Add(time.Hour), ObservedAt: now}}
	p := statusProvider{Provider: "title\x1b", Status: "unavailable", Error: "error\r"}
	before := a
	lines := watchPanelLines(p, []statusAccount{a}, now)
	var text strings.Builder
	for _, line := range lines {
		text.WriteString(line.paint(false))
	}
	if strings.ContainsAny(text.String(), "\x1b\r\n\x00\u202e") || !strings.Contains(text.String(), "private?") || !strings.Contains(text.String(), "error?") {
		t.Fatal("unsafe/private/title display not handled")
	}
	if !reflect.DeepEqual(before, a) {
		t.Fatal("display mutated stored report")
	}
	if got := watchClean(strings.Repeat("a", 300)); len(got) != 300 {
		t.Fatal("long display text silently truncated")
	}
}

func TestWatchCollectorContextAndEmit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &Store{root: t.TempDir()}
	r, err := collectStatusContext(ctx, store, []string{"codex"}, nil)
	if !errors.Is(err, context.Canceled) || len(r.Providers) != 0 {
		t.Fatal("cancelled collector continued")
	}
	var emitted []statusProvider
	r, err = collectStatusContext(context.Background(), store, []string{"codex", "invalid"}, func(p statusProvider, a *statusAccount) {
		if a != nil {
			t.Fatal("unexpected row")
		}
		emitted = append(emitted, p)
	})
	if err != nil || !reflect.DeepEqual(emitted, r.Providers) || emitted[0].Status != "no_accounts" || emitted[1].Status != "unavailable" {
		t.Fatalf("emit/report behavior: %+v %v", r, err)
	}
}

func TestWatchRendererLegacyBytes(t *testing.T) {
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	r := watchFixture(now)
	for _, color := range []bool{false, true} {
		var old, current, watched strings.Builder
		for _, p := range r.Providers {
			var rows []statusAccount
			for _, a := range r.Accounts {
				if a.Provider == p.Provider {
					rows = append(rows, a)
				}
			}
			renderStatusPanel(&old, p, rows, color, now)
			renderStatusPanel(&current, p, rows, color, now)
			for _, line := range watchPanelLines(p, rows, now) {
				fmt.Fprintln(&watched, line.paint(color))
			}
		}
		if current.String() != old.String() || watched.String() != old.String() {
			t.Fatalf("legacy output changed, color=%t", color)
		}
		expected := "d228b68db353c63183d042cb1e681d502917e0febb15245a001cda3d0ea4782a"
		if color {
			expected = "89ccdaf600e1364bf051327c5bcfddaa4131347c3ba26df809d0e45eff94e6e0"
		}
		if got := fmt.Sprintf("%x", sha256.Sum256([]byte(old.String()))); got != expected {
			t.Fatalf("legacy fixture changed: color=%t hash=%s", color, got)
		}
		if dir := os.Getenv("CODATOR_WATCH_CAPTURE_DIR"); dir != "" {
			label := "plain"
			if color {
				label = "ansi"
			}
			if err := os.WriteFile(filepath.Join(dir, "legacy-"+label+".txt"), []byte(old.String()), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "watch-"+label+".txt"), []byte(watched.String()), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
