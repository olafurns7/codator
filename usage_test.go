package main

import (
	"math"
	"testing"
	"time"
)

func boolPtr(value bool) *bool { return &value }

func TestEvaluateUsageUsesMinimumPercentageHeadroom(t *testing.T) {
	got := evaluateUsage(boolPtr(true), nil, []float64{25, 70})
	if !got.Eligible || got.Headroom != 30 {
		t.Fatalf("quota=%+v, want 30%% headroom", got)
	}
	for _, got := range []quota{
		evaluateUsage(nil, nil, []float64{0}),
		evaluateUsage(boolPtr(false), nil, []float64{0}),
		evaluateUsage(boolPtr(true), boolPtr(true), []float64{0}),
		evaluateUsage(boolPtr(true), nil, []float64{100}),
		evaluateUsage(boolPtr(true), nil, []float64{math.NaN()}),
		evaluateUsage(boolPtr(true), nil, nil),
	} {
		if got.Eligible {
			t.Errorf("unexpected eligible quota: %+v", got)
		}
	}
}

func TestBestCandidateSkipsUnknownAndTiesByName(t *testing.T) {
	items := []candidate{
		{Account: Account{Name: "unknown"}, Quota: quota{Subscription: true, Reason: "missing"}},
		{Account: Account{Name: "z"}, Quota: quota{Eligible: true, Headroom: 40}},
		{Account: Account{Name: "a"}, Quota: quota{Eligible: true, Headroom: 40}},
	}
	got, ok := bestCandidate(items)
	if !ok || got.Account.Name != "a" {
		t.Fatalf("best=%+v ok=%v", got, ok)
	}
}

func TestBestCandidatePrefersIncludedUsageOverCredits(t *testing.T) {
	items := []candidate{
		{Account: Account{Name: "credits"}, Quota: quota{Known: true, Eligible: true, Headroom: 0, OnCredits: true}},
		{Account: Account{Name: "included"}, Quota: quota{Known: true, Eligible: true, Headroom: 1}},
	}
	got, ok := bestCandidate(items)
	if !ok || got.Account.Name != "included" {
		t.Fatalf("best=%+v ok=%v, want included-usage account", got, ok)
	}
	items = []candidate{
		{Account: Account{Name: "z"}, Quota: quota{Known: true, Eligible: true, Headroom: 0, OnCredits: true}},
		{Account: Account{Name: "a"}, Quota: quota{Known: true, Eligible: true, Headroom: 0, OnCredits: true}},
	}
	got, ok = bestCandidate(items)
	if !ok || got.Account.Name != "a" {
		t.Fatalf("best=%+v ok=%v, want name-ordered credits account", got, ok)
	}
}

func TestExplicitLaunchNeedsVerifiedIdentityAndRejectsKnownIneligibleQuota(t *testing.T) {
	for name, q := range map[string]quota{
		"verified with unknown quota": {Subscription: true},
		"verified and eligible":       {Subscription: true, Known: true, Eligible: true},
	} {
		if !canLaunchExplicit(q) {
			t.Errorf("rejected %s", name)
		}
	}
	for name, q := range map[string]quota{
		"unknown identity":      {Known: false},
		"known exhausted quota": {Subscription: true, Known: true, Eligible: false},
	} {
		if canLaunchExplicit(q) {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestStatusTimesAreRelative(t *testing.T) {
	for duration, want := range map[time.Duration]string{
		30 * time.Second:                  "<1m",
		23*time.Minute + 59*time.Second:   "23m",
		5*time.Hour + 4*time.Minute:       "5h 4m",
		32*time.Hour + 35*time.Minute:     "1d 8h",
		6*24*time.Hour + 23*time.Hour + 1: "6d 23h",
	} {
		if got := durationText(duration); got != want {
			t.Errorf("durationText(%s)=%q, want %q", duration, got, want)
		}
	}
	now := time.Date(2026, 9, 25, 16, 46, 30, 0, time.UTC)
	used, remaining := 100.0, 0.0
	exhausted := statusWindow{UsedPercent: &used, RemainingPercent: &remaining, Exhausted: true}
	if got := statusWindowCell(exhausted, now).paint(false); got != "░░░░░░░░░░   0%" {
		t.Errorf("exhausted window without a reset=%q", got)
	}
	used, remaining = 34, 66
	past := now.Add(-time.Minute)
	expired := statusWindow{UsedPercent: &used, RemainingPercent: &remaining, ResetsAt: &past}
	if got := statusWindowCell(expired, now).paint(false); got != "██████▌░░░  66%" {
		t.Errorf("past reset was shown: %q", got)
	}
}
