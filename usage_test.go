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

func TestStatusTimesAreLocalAndRelative(t *testing.T) {
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
	if got := whenText(now.Add(-2*time.Minute), now); got != "Fri Sep 25 16:44 (2m ago)" {
		t.Errorf("past time=%q", got)
	}
	if got := whenText(now.Add(90*time.Minute), now.In(time.FixedZone("UTC+2", 2*60*60))); got != "Fri Sep 25 20:16 (in 1h 30m)" {
		t.Errorf("time is not shown in the viewer's zone: %q", got)
	}
	if got := windowText(100, time.Time{}, now); got != "0.0% remaining (exhausted)" {
		t.Errorf("exhausted window without a reset=%q", got)
	}
	if got := windowText(34, now.Add(-time.Minute), now); got != "66.0% remaining" {
		t.Errorf("past reset was shown: %q", got)
	}
}
