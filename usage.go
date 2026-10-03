package main

import (
	"fmt"
	"math"
	"time"
)

type quota struct {
	Headroom     float64
	Eligible     bool
	Known        bool
	Subscription bool
	Reason       string
	Windows      []usageWindow // reported limits, shown by status
	ResetCredits int           // Codex rate-limit resets the account can redeem
	Credits      string        // Codex credit balance, display only
	OnCredits    bool          // eligible only through Codex credits
	Email        string        // signed-in address, display only
}

type usageWindow struct {
	Label    string
	Used     float64
	ResetsAt time.Time // zero when not reported
}

// Subscription is verified identity; Known and Eligible describe quota only.
// Explicit selection can proceed with unknown quota after identity is verified.
func canLaunchExplicit(q quota) bool {
	return q.Subscription && (!q.Known || q.Eligible)
}

type candidate struct {
	Account Account
	Quota   quota
}

func evaluateUsage(ordinary, spendControl *bool, usedPercent []float64) quota {
	if ordinary == nil {
		return quota{Reason: "ordinary usage permission is unknown"}
	}
	if !*ordinary {
		return quota{Known: true, Reason: "ordinary included usage is disallowed"}
	}
	if spendControl != nil && *spendControl {
		return quota{Known: true, Reason: "spend control is reached"}
	}
	if len(usedPercent) == 0 {
		return quota{Reason: "no applicable usage windows"}
	}
	headroom := 100.0
	for _, used := range usedPercent {
		if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
			return quota{Reason: "usage window is malformed"}
		}
		if remaining := 100 - used; remaining < headroom {
			headroom = remaining
		}
	}
	if headroom == 0 {
		return quota{Headroom: headroom, Known: true, Reason: "all applicable usage headroom is exhausted"}
	}
	return quota{Headroom: headroom, Eligible: true, Known: true}
}

func bestCandidate(accounts []candidate) (candidate, bool) {
	var best candidate
	found := false
	for _, item := range accounts {
		if !item.Quota.Eligible {
			continue
		}
		if !found || item.Quota.Headroom > best.Quota.Headroom || item.Quota.Headroom == best.Quota.Headroom && item.Account.Name < best.Account.Name {
			best, found = item, true
		}
	}
	return best, found
}

func durationText(d time.Duration) string {
	minutes := int(d / time.Minute)
	switch {
	case minutes < 1:
		return "<1m"
	case minutes < 60:
		return fmt.Sprintf("%dm", minutes)
	case minutes < 24*60:
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	default:
		return fmt.Sprintf("%dd %dh", minutes/(24*60), minutes%(24*60)/60)
	}
}
