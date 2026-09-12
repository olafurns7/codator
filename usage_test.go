package main

import (
	"math"
	"testing"
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
