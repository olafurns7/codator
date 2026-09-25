package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

func claudeStatus(state claudeProbeCache, found bool, readErr error, now time.Time) string {
	if readErr != nil {
		return claudeStatusUnavailable(state, "local usage snapshot is unavailable", now)
	}
	if !found {
		return "usage unavailable (no stored observation)"
	}
	if state.Outcome == "reserved" {
		return claudeStatusUnavailable(state, "probe reservation is unresolved", now)
	}
	if state.ObservedAt.IsZero() {
		return claudeStatusUnavailable(state, "snapshot is missing", now)
	}
	age := now.Sub(state.ObservedAt)
	if age < 0 || age >= claudeSuccessCooldown {
		return claudeStatusUnavailable(state, "snapshot is stale", now)
	}
	if len(state.Snapshot) == 0 {
		return claudeStatusUnavailable(state, "snapshot is missing", now)
	}
	var usage claudeUsageResponse
	if err := json.Unmarshal(state.Snapshot, &usage); err != nil {
		return claudeStatusUnavailable(state, "snapshot is malformed", now)
	}
	if !state.Subscription {
		return claudeStatusUnavailable(state, "Claude subscription is unverified", now)
	}
	if usage.RateLimitsAvailable == nil || !*usage.RateLimitsAvailable {
		return claudeStatusUnavailable(state, "Claude rate limits are unavailable", now)
	}

	type bucket struct {
		label  string
		window *claudeUsageWindow
	}
	var buckets []bucket
	add := func(label string, window *claudeUsageWindow) {
		buckets = append(buckets, bucket{label: label, window: window})
	}
	limits := usage.RateLimits
	var fiveHour, sevenDay, oauthApps, opus, sonnet *claudeUsageWindow
	if limits != nil {
		fiveHour, sevenDay, oauthApps = limits.FiveHour, limits.SevenDay, limits.SevenDayOAuthApps
		opus, sonnet = limits.SevenDayOpus, limits.SevenDaySonnet
	}
	add("Current session", fiveHour)
	add("Weekly (all models)", sevenDay)
	if oauthApps != nil {
		add("Weekly (OAuth apps)", oauthApps)
	}
	modelCounts := make(map[string]int)
	if opus != nil {
		modelCounts["Opus"]++
	}
	if sonnet != nil {
		modelCounts["Sonnet"]++
	}
	if limits != nil && limits.ModelScoped != nil {
		for _, window := range *limits.ModelScoped {
			if window.DisplayName != "" && window.DisplayName != "unknown" {
				modelCounts[window.DisplayName]++
			}
		}
	}
	modelSeen := make(map[string]int)
	otherModels := 0
	addModel := func(name string, window *claudeUsageWindow) {
		if name == "" || name == "unknown" {
			otherModels++
			name = fmt.Sprintf("other reported model %d", otherModels)
		} else {
			modelSeen[name]++
			if modelCounts[name] > 1 {
				name = fmt.Sprintf("%s #%d", name, modelSeen[name])
			}
		}
		add("Weekly ("+name+")", window)
	}
	if opus != nil {
		addModel("Opus", opus)
	}
	if sonnet != nil {
		addModel("Sonnet", sonnet)
	}
	hasFable := false
	if limits != nil && limits.ModelScoped != nil {
		for i := range *limits.ModelScoped {
			window := &(*limits.ModelScoped)[i]
			if window.DisplayName == "Fable" {
				hasFable = true
			}
			addModel(window.DisplayName, window)
		}
	}
	if !hasFable {
		add("Weekly (Fable)", nil)
	}

	status := make([]string, 0, len(buckets))
	for _, item := range buckets {
		status = append(status, item.label+": "+claudeStatusWindow(item.window, now))
	}
	for _, denial := range state.Denials {
		if denial.ObservedAt.IsZero() || denial.ObservedAt.After(now) || !denial.ResetsAt.After(now) || claudeStatusDenialShown(limits, denial) {
			continue
		}
		status = append(status, fmt.Sprintf("known exhausted: %s until %s (last observed at %s)",
			claudeStatusDenialScope(state, denial), whenText(denial.ResetsAt, now), whenText(denial.ObservedAt, now)))
	}
	return "last observed at " + whenText(state.ObservedAt, now) + "\n  " + strings.Join(status, "\n  ")
}

func claudeStatusWindow(window *claudeUsageWindow, now time.Time) string {
	if window == nil {
		return "unavailable (not reported)"
	}
	if window.Utilization == nil || math.IsNaN(*window.Utilization) || math.IsInf(*window.Utilization, 0) || *window.Utilization < 0 || *window.Utilization > 100 {
		return "unavailable (malformed utilization)"
	}
	reset, valid := claudeResetTime(window.ResetsAt)
	if !valid {
		return "unavailable (malformed reset time)"
	}
	if reset != nil && !reset.After(now) {
		return "unavailable (window expired)"
	}
	if reset == nil && *window.Utilization > 0 {
		return "unavailable (reset time unknown)"
	}
	var resetAt time.Time
	if reset != nil {
		resetAt = *reset
	}
	return windowText(*window.Utilization, resetAt, now)
}

func claudeStatusUnavailable(state claudeProbeCache, reason string, now time.Time) string {
	if !state.ObservedAt.IsZero() {
		reason += "; last observed at " + whenText(state.ObservedAt, now)
	}
	parts := []string{"usage unavailable (" + reason + ")"}
	for _, denial := range state.Denials {
		if denial.ObservedAt.IsZero() || denial.ObservedAt.After(now) || !denial.ResetsAt.After(now) {
			continue
		}
		parts = append(parts, fmt.Sprintf("known exhausted: %s until %s (last observed at %s)",
			claudeStatusDenialScope(state, denial), whenText(denial.ResetsAt, now), whenText(denial.ObservedAt, now)))
	}
	if state.NextProbeAt.After(now) {
		parts = append(parts, "probe cooldown until "+whenText(state.NextProbeAt, now))
	}
	return strings.Join(parts, "; ")
}

func claudeStatusDenialScope(state claudeProbeCache, denial claudeCachedDenial) string {
	if denial.DisplayName != "" {
		if denial.DisplayName == "unknown" {
			return "Weekly (other reported model)"
		}
		return "Weekly (" + denial.DisplayName + ")"
	}
	var usage claudeUsageResponse
	if json.Unmarshal(state.Snapshot, &usage) == nil && usage.RateLimits != nil {
		matches := make([]string, 0, 3)
		for _, item := range []struct {
			label  string
			window *claudeUsageWindow
		}{
			{"Current session", usage.RateLimits.FiveHour},
			{"Weekly (all models)", usage.RateLimits.SevenDay},
			{"Weekly (OAuth apps)", usage.RateLimits.SevenDayOAuthApps},
		} {
			if item.window == nil || item.window.Utilization == nil || *item.window.Utilization != 100 {
				continue
			}
			reset, valid := claudeResetTime(item.window.ResetsAt)
			if valid && reset != nil && reset.Equal(denial.ResetsAt) {
				matches = append(matches, item.label)
			}
		}
		if len(matches) != 0 {
			return strings.Join(matches, " / ")
		}
	}
	return "shared Claude quota"
}

func claudeStatusDenialShown(limits *claudeRateLimits, denial claudeCachedDenial) bool {
	shown := func(window *claudeUsageWindow) bool {
		if window == nil || window.Utilization == nil || *window.Utilization != 100 {
			return false
		}
		reset, valid := claudeResetTime(window.ResetsAt)
		return valid && reset != nil && reset.Equal(denial.ResetsAt)
	}
	if limits == nil {
		return false
	}
	if denial.DisplayName == "" {
		return shown(limits.FiveHour) || shown(limits.SevenDay) || shown(limits.SevenDayOAuthApps)
	}
	if denial.DisplayName == "Opus" && shown(limits.SevenDayOpus) || denial.DisplayName == "Sonnet" && shown(limits.SevenDaySonnet) {
		return true
	}
	if limits.ModelScoped != nil {
		for i := range *limits.ModelScoped {
			window := &(*limits.ModelScoped)[i]
			if window.DisplayName == denial.DisplayName && shown(window) {
				return true
			}
		}
	}
	return false
}
