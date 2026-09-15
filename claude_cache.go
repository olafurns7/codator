package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"strings"
	"time"
)

const (
	claudeCacheName       = ".claude-usage.json"
	claudeCacheTemp       = ".claude-usage.tmp"
	claudeCacheVersion    = 2
	claudeCacheMaxBytes   = 64 << 10
	claudeCacheMaxWindows = 128
	claudeCacheMaxDenials = 6
	claudeSuccessCooldown = 5 * time.Minute
	claudeFailureCooldown = 15 * time.Minute
	claudeRetryPrefix     = "Claude usage probe cooling down; retry after "
	claudeAllModelMask    = 1 << 0
	claudeOpusModelMask   = 1 << 1
	claudeSonnetModelMask = 1 << 2
	claudeHaikuModelMask  = 1 << 3
	claudeFableModelMask  = 1 << 4
	claudeAllMasks        = claudeAllModelMask | claudeOpusModelMask | claudeSonnetModelMask | claudeHaikuModelMask | claudeFableModelMask
)

var errClaudeCacheCorrupt = errors.New("Claude usage cache is corrupt")

type claudeProbeCache struct {
	Version           int                  `json:"version"`
	AttemptedAt       time.Time            `json:"attempted_at"`
	NextProbeAt       time.Time            `json:"next_probe_at"`
	Outcome           string               `json:"outcome"`
	ObservedAt        time.Time            `json:"observed_at"`
	Subscription      bool                 `json:"subscription"`
	SubscriptionUntil time.Time            `json:"subscription_until"`
	AllowanceModels   uint8                `json:"allowance_models"`
	Denials           []claudeCachedDenial `json:"denials,omitempty"`
	Snapshot          json.RawMessage      `json:"snapshot,omitempty"`
}

type claudeCachedDenial struct {
	DisplayName string    `json:"display_name,omitempty"`
	ResetsAt    time.Time `json:"resets_at"`
	ObservedAt  time.Time `json:"observed_at"`
}

func (s *Store) probeClaudeAccount(ctx context.Context, account Account, model claudeModelFamily) (quota, error) {
	attemptedAt := time.Now().UTC()
	state, found, err := s.readClaudeProbeCache(account, attemptedAt)
	if errors.Is(err, errClaudeCacheCorrupt) {
		state = newClaudeReservation(attemptedAt)
		if writeErr := s.writeClaudeProbeCache(account, state); writeErr != nil {
			return quota{Reason: "Claude local usage state is invalid and its cooldown could not be recorded"}, errors.New("Claude local usage state is invalid")
		}
		return claudeCachedQuota(state, attemptedAt, model), nil
	}
	if err != nil {
		return quota{Reason: "Claude local usage state is unsafe or unavailable"}, errors.New("Claude local usage state is unsafe or unavailable")
	}
	if found {
		now := time.Now().UTC()
		if now.Before(state.NextProbeAt) {
			return claudeCachedQuota(state, now, model), nil
		}
	}

	reservation := state
	if !found {
		reservation = newClaudeReservation(attemptedAt)
	} else {
		reservation.AttemptedAt = attemptedAt
		reservation.NextProbeAt = attemptedAt.Add(claudeFailureCooldown)
		reservation.Outcome = "reserved"
		reservation.AllowanceModels = 0
		reservation.Denials = freshClaudeDenials(reservation.Denials, attemptedAt)
	}
	if err := s.writeClaudeProbeCache(account, reservation); err != nil {
		return quota{Reason: "Claude usage cooldown could not be recorded"}, errors.New("Claude usage cooldown could not be recorded")
	}

	payload, err := probeClaudePayloadWithContext(ctx, account)
	if err != nil {
		if ctx.Err() != nil {
			return quota{}, ctx.Err()
		}
		return claudeCachedQuota(reservation, time.Now().UTC(), model), nil
	}

	completedAt := time.Now().UTC()
	snapshot, subscription, models, usable, denials, err := sanitizeClaudeQuotaPayload(payload, completedAt)
	if err != nil {
		currentDenials := claudeUncacheableDenials(payload, completedAt)
		reservation.Denials = mergeClaudeDenials(reservation.Denials, currentDenials, completedAt)
		if len(currentDenials) != 0 {
			_ = s.writeClaudeProbeCache(account, reservation)
		}
		result, parseErr := parseClaudeQuotaForModel(payload, completedAt, model)
		if parseErr != nil {
			result = quota{Reason: "Claude usage response is unknown"}
		}
		if !result.Known || result.Eligible {
			result.Known, result.Eligible, result.Headroom = false, false, 0
		}
		result = applyClaudeDenials(result, reservation.Denials, completedAt, model, result.Subscription, reservation.NextProbeAt)
		if !result.Known || !result.Eligible {
			result.Reason = withClaudeRetry(result.Reason, reservation.NextProbeAt)
		}
		return result, nil
	}

	completed := reservation
	completed.ObservedAt = completedAt
	completed.Subscription = subscription
	completed.AllowanceModels = models
	completed.Snapshot = snapshot
	if usable {
		completed.Denials = denials
	} else {
		completed.Denials = mergeClaudeDenials(reservation.Denials, denials, completedAt)
	}
	if usable {
		completed.Outcome = "usable"
		completed.NextProbeAt = attemptedAt.Add(claudeSuccessCooldown)
	} else {
		completed.Outcome = "unknown"
		completed.NextProbeAt = attemptedAt.Add(claudeFailureCooldown)
	}
	if completed.Subscription {
		completed.SubscriptionUntil = completed.NextProbeAt
	} else {
		completed.SubscriptionUntil = time.Time{}
	}
	if err := s.writeClaudeProbeCache(account, completed); err != nil {
		// The reservation remains durable, so only its deadline may be reported.
		reservation.Denials = completed.Denials
		return claudeCachedQuota(reservation, completedAt, model), nil
	}

	result, parseErr := parseClaudeQuotaForModel(snapshot, completedAt, model)
	if parseErr != nil {
		return quota{Reason: "Claude usage response is unknown; " + claudeRetryReason(completed.NextProbeAt)}, nil
	}
	result.Subscription = completed.Subscription
	result = applyClaudeDenials(result, completed.Denials, completedAt, model, completed.Subscription, completed.NextProbeAt)
	if (!result.Known || !result.Eligible) && completed.NextProbeAt.After(completedAt) {
		result.Reason = withClaudeRetry(result.Reason, completed.NextProbeAt)
	}
	return result, nil
}

func newClaudeReservation(now time.Time) claudeProbeCache {
	return claudeProbeCache{
		Version:     claudeCacheVersion,
		AttemptedAt: now,
		NextProbeAt: now.Add(claudeFailureCooldown),
		Outcome:     "reserved",
	}
}

func claudeRetryReason(at time.Time) string {
	return claudeRetryPrefix + at.UTC().Format(time.RFC3339Nano)
}

func claudeRetryHint(reason string) string {
	if strings.Contains(reason, claudeRetryPrefix) {
		return reason
	}
	return ""
}

func claudeCachedQuota(state claudeProbeCache, now time.Time, model claudeModelFamily) quota {
	subscription := state.Subscription && now.Before(state.SubscriptionUntil)
	if q := applyClaudeDenials(quota{}, state.Denials, now, model, subscription, state.NextProbeAt); q.Known {
		return q
	}
	if len(state.Snapshot) != 0 {
		q, err := parseClaudeQuotaForModel(state.Snapshot, now, model)
		if err == nil {
			age := now.Sub(state.ObservedAt)
			fresh := !state.ObservedAt.IsZero() && age >= 0 && age < claudeSuccessCooldown
			q.Subscription = subscription
			if q.Known && !q.Eligible && q.Reason == "all applicable usage headroom is exhausted" {
				return applyClaudeDenials(q, state.Denials, now, model, subscription, state.NextProbeAt)
			}
			if state.Outcome != "reserved" && state.AllowanceModels&claudeModelMask(model) != 0 && fresh && q.Known {
				return q
			}
			if subscription {
				q.Eligible = false
				q.Known = false
				q.Headroom = 0
				q.Reason = withClaudeRetry(q.Reason, state.NextProbeAt)
				return q
			}
		}
	}
	return quota{Subscription: subscription, Reason: claudeRetryReason(state.NextProbeAt)}
}

func withClaudeRetry(reason string, at time.Time) string {
	if strings.Contains(reason, claudeRetryPrefix) {
		return reason
	}
	if reason != "" {
		return reason + "; " + claudeRetryReason(at)
	}
	return claudeRetryReason(at)
}

func claudeModelMask(model claudeModelFamily) uint8 {
	switch model {
	case claudeModelOpus:
		return claudeOpusModelMask
	case claudeModelSonnet:
		return claudeSonnetModelMask
	case claudeModelHaiku:
		return claudeHaikuModelMask
	case claudeModelFable:
		return claudeFableModelMask
	default:
		return claudeAllModelMask
	}
}

func applyClaudeDenials(q quota, denials []claudeCachedDenial, now time.Time, model claudeModelFamily, subscription bool, retryAt time.Time) quota {
	for _, denial := range denials {
		age := now.Sub(denial.ObservedAt)
		if age < 0 || !denial.ResetsAt.After(now) {
			continue
		}
		family, known := claudeModelBucket(denial.DisplayName)
		knownModel := model == claudeModelOpus || model == claudeModelSonnet || model == claudeModelHaiku || model == claudeModelFable
		if denial.DisplayName != "" && knownModel && known && family != model {
			continue
		}
		q.Subscription = subscription
		q.Known, q.Eligible, q.Headroom = true, false, 0
		q.Reason = withClaudeRetry("all applicable usage headroom is exhausted", retryAt)
		return q
	}
	return q
}

func freshClaudeDenials(denials []claudeCachedDenial, now time.Time) []claudeCachedDenial {
	out := make([]claudeCachedDenial, 0, len(denials))
	for _, denial := range denials {
		age := now.Sub(denial.ObservedAt)
		if age >= 0 && denial.ResetsAt.After(now) {
			out = append(out, denial)
		}
	}
	return compactClaudeDenials(out)
}

func mergeClaudeDenials(first, second []claudeCachedDenial, now time.Time) []claudeCachedDenial {
	merged := append(freshClaudeDenials(first, now), freshClaudeDenials(second, now)...)
	return compactClaudeDenials(merged)
}

func compactClaudeDenials(denials []claudeCachedDenial) []claudeCachedDenial {
	var out []claudeCachedDenial
	for _, denial := range denials {
		found := false
		for i := range out {
			if out[i].DisplayName == denial.DisplayName {
				if denial.ResetsAt.After(out[i].ResetsAt) || denial.ResetsAt.Equal(out[i].ResetsAt) && denial.ObservedAt.After(out[i].ObservedAt) {
					out[i] = denial
				}
				found = true
				break
			}
		}
		if !found {
			out = append(out, denial)
		}
	}
	if len(out) > claudeCacheMaxDenials {
		return out[:claudeCacheMaxDenials]
	}
	return out
}

func (s *Store) invalidateClaudeCacheOnLogin(account Account) error {
	now := time.Now().UTC()
	state, found, err := s.readClaudeProbeCache(account, now)
	if errors.Is(err, errClaudeCacheCorrupt) {
		return s.writeClaudeProbeCache(account, newClaudeReservation(now))
	}
	if err != nil || !found {
		return err
	}
	state.ObservedAt = time.Time{}
	state.Subscription = false
	state.SubscriptionUntil = time.Time{}
	state.AllowanceModels = 0
	state.Denials = nil
	state.Snapshot = nil
	return s.writeClaudeProbeCache(account, state)
}

func (s *Store) readClaudeProbeCache(account Account, now time.Time) (claudeProbeCache, bool, error) {
	if account.Provider != "claude" {
		return claudeProbeCache{}, false, errors.New("invalid Claude account")
	}
	dir, err := s.accountRoot(account.Provider, account.Name, false)
	if err != nil {
		return claudeProbeCache{}, false, err
	}
	defer dir.Close()
	info, err := dir.Lstat(claudeCacheName)
	if errors.Is(err, fs.ErrNotExist) {
		return claudeProbeCache{}, false, nil
	}
	if err != nil {
		return claudeProbeCache{}, false, err
	}
	if err := checkPrivateFile(info, 0600); err != nil {
		return claudeProbeCache{}, false, fmt.Errorf("unsafe Claude cache file: %w", err)
	}
	file, err := openPrivateFile(dir, claudeCacheName, os.O_RDONLY, 0600)
	if err != nil {
		return claudeProbeCache{}, false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, claudeCacheMaxBytes+1))
	if err != nil {
		return claudeProbeCache{}, false, err
	}
	if len(data) > claudeCacheMaxBytes {
		return claudeProbeCache{}, true, errClaudeCacheCorrupt
	}
	var state claudeProbeCache
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&state); err != nil {
		return claudeProbeCache{}, true, errClaudeCacheCorrupt
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return claudeProbeCache{}, true, errClaudeCacheCorrupt
	}
	if err := validateClaudeProbeCache(state, now); err != nil {
		return claudeProbeCache{}, true, errClaudeCacheCorrupt
	}
	return state, true, nil
}

func validateClaudeProbeCache(state claudeProbeCache, now time.Time) error {
	if state.Version != claudeCacheVersion || state.AttemptedAt.IsZero() || state.AttemptedAt.After(now.Add(2*time.Minute)) {
		return errClaudeCacheCorrupt
	}
	interval := claudeFailureCooldown
	switch state.Outcome {
	case "usable":
		interval = claudeSuccessCooldown
	case "unknown", "reserved":
	default:
		return errClaudeCacheCorrupt
	}
	if !state.NextProbeAt.Equal(state.AttemptedAt.Add(interval)) || state.AllowanceModels&^claudeAllMasks != 0 || state.Outcome == "reserved" && state.AllowanceModels != 0 {
		return errClaudeCacheCorrupt
	}
	if len(state.Snapshot) == 0 {
		if !state.ObservedAt.IsZero() || state.Subscription || !state.SubscriptionUntil.IsZero() || state.AllowanceModels != 0 || len(state.Denials) != 0 && state.Outcome != "reserved" {
			return errClaudeCacheCorrupt
		}
	} else {
		if state.ObservedAt.IsZero() || state.ObservedAt.After(now.Add(2*time.Minute)) {
			return errClaudeCacheCorrupt
		}
		if state.Subscription {
			if state.SubscriptionUntil.IsZero() || state.SubscriptionUntil.After(state.NextProbeAt) || state.Outcome != "reserved" && !state.SubscriptionUntil.Equal(state.NextProbeAt) {
				return errClaudeCacheCorrupt
			}
		} else if !state.SubscriptionUntil.IsZero() {
			return errClaudeCacheCorrupt
		}
		safe, subscription, models, usable, _, err := sanitizeClaudeQuotaPayload(state.Snapshot, state.ObservedAt)
		if err != nil || !bytes.Equal(safe, state.Snapshot) || state.Subscription != subscription || state.AllowanceModels&^models != 0 || state.Outcome == "usable" && !usable || state.Outcome == "unknown" && usable {
			return errClaudeCacheCorrupt
		}
	}
	if len(state.Denials) > claudeCacheMaxDenials {
		return errClaudeCacheCorrupt
	}
	for _, denial := range state.Denials {
		if !claudeCacheModelLabel(denial.DisplayName) || denial.ResetsAt.IsZero() || denial.ObservedAt.IsZero() || denial.ResetsAt.Before(denial.ObservedAt) || denial.ObservedAt.After(now.Add(2*time.Minute)) {
			return errClaudeCacheCorrupt
		}
	}
	return nil
}

func claudeCacheModelLabel(value string) bool {
	if value == "" || value == "unknown" {
		return true
	}
	_, ok := claudeModelBucket(value)
	return ok
}

func (s *Store) writeClaudeProbeCache(account Account, state claudeProbeCache) error {
	if account.Provider != "claude" || validateClaudeProbeCache(state, time.Now().UTC()) != nil {
		return errors.New("invalid Claude usage cache state")
	}
	data, err := json.Marshal(state)
	if err != nil || len(data) > claudeCacheMaxBytes {
		return errors.New("Claude usage cache state is too large")
	}
	dir, err := s.accountRoot(account.Provider, account.Name, false)
	if err != nil {
		return err
	}
	defer dir.Close()
	return replaceClaudeCacheFile(dir, data)
}

func replaceClaudeCacheFile(dir *os.Root, data []byte) error {
	if len(data) > claudeCacheMaxBytes {
		return errors.New("Claude usage cache state is too large")
	}
	if info, err := dir.Lstat(claudeCacheName); err == nil {
		if err := checkPrivateFile(info, 0600); err != nil {
			return fmt.Errorf("unsafe Claude cache file: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if info, err := dir.Lstat(claudeCacheTemp); err == nil {
		if err := checkPrivateFile(info, 0600); err != nil {
			return fmt.Errorf("unsafe Claude cache temporary file: %w", err)
		}
		if err := dir.Remove(claudeCacheTemp); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := dir.OpenFile(claudeCacheTemp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	created := true
	defer func() {
		if created {
			_ = dir.Remove(claudeCacheTemp)
		}
	}()
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	n, writeErr := file.Write(data)
	if writeErr == nil && n != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if closeErr := file.Close(); writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		return writeErr
	}
	if info, err := dir.Lstat(claudeCacheName); err == nil {
		if err := checkPrivateFile(info, 0600); err != nil {
			return fmt.Errorf("unsafe Claude cache file: %w", err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := dir.Rename(claudeCacheTemp, claudeCacheName); err != nil {
		return err
	}
	created = false
	directory, err := dir.Open(".")
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func sanitizeClaudeQuotaPayload(payload []byte, observedAt time.Time) (json.RawMessage, bool, uint8, bool, []claudeCachedDenial, error) {
	if len(payload) == 0 || len(payload) > claudeMaxFrame {
		return nil, false, 0, false, nil, errors.New("Claude usage response is missing or too large")
	}
	top, err := rawJSONObject(payload)
	if err != nil {
		return nil, false, 0, false, nil, errors.New("Claude usage response is malformed")
	}
	plan, subscription, planKnown := projectClaudePlan(top["subscription_type"])
	usage := claudeUsageResponse{SubscriptionType: plan}
	if raw := top["rate_limits_available"]; len(raw) != 0 && !isClaudeJSONNull(raw) {
		var available bool
		if json.Unmarshal(raw, &available) == nil {
			usage.RateLimitsAvailable = &available
		}
	}
	var rateComplete bool
	usage.RateLimits, rateComplete, err = projectClaudeRateLimits(top["rate_limits"], observedAt)
	if err != nil {
		return nil, false, 0, false, nil, err
	}
	snapshot, err := json.Marshal(usage)
	if err != nil || len(snapshot) > claudeCacheMaxBytes/2 {
		return nil, false, 0, false, nil, errors.New("Claude usage snapshot is too large")
	}
	denials := claudeSnapshotDenials(usage.RateLimits, observedAt)
	var models uint8
	for _, item := range []struct {
		model claudeModelFamily
		mask  uint8
	}{
		{"", claudeAllModelMask},
		{claudeModelOpus, claudeOpusModelMask},
		{claudeModelSonnet, claudeSonnetModelMask},
		{claudeModelHaiku, claudeHaikuModelMask},
		{claudeModelFable, claudeFableModelMask},
	} {
		q, err := parseClaudeQuotaForModel(snapshot, observedAt, item.model)
		if err == nil && q.Known {
			models |= item.mask
		}
	}
	available := usage.RateLimitsAvailable != nil && *usage.RateLimitsAvailable
	usable := planKnown && available && rateComplete && models == claudeAllMasks
	return snapshot, subscription, models, usable, denials, nil
}

func projectClaudePlan(raw json.RawMessage) (string, bool, bool) {
	var value string
	if json.Unmarshal(raw, &value) != nil || len(value) > 32 {
		return "unknown", false, false
	}
	value = strings.ToLower(strings.TrimSpace(value))
	if knownClaudeSubscription(value) {
		return "pro", true, true
	}
	if value == "free" {
		return "free", false, true
	}
	return "unknown", false, false
}

func projectClaudeRateLimits(raw json.RawMessage, observedAt time.Time) (*claudeRateLimits, bool, error) {
	if len(raw) == 0 || isClaudeJSONNull(raw) {
		return nil, false, nil
	}
	fields, err := rawJSONObject(raw)
	if err != nil {
		window := malformedClaudeWindow(false)
		return &claudeRateLimits{FiveHour: &window}, false, nil
	}
	limits := &claudeRateLimits{}
	complete := true
	for _, item := range []struct {
		name   string
		target **claudeUsageWindow
	}{
		{"five_hour", &limits.FiveHour},
		{"seven_day", &limits.SevenDay},
		{"seven_day_oauth_apps", &limits.SevenDayOAuthApps},
		{"seven_day_opus", &limits.SevenDayOpus},
		{"seven_day_sonnet", &limits.SevenDaySonnet},
	} {
		value, ok := fields[item.name]
		if !ok || isClaudeJSONNull(value) {
			continue
		}
		window, usable := projectClaudeWindow(value, false, observedAt)
		*item.target = &window
		complete = complete && usable
	}
	if value, ok := fields["model_scoped"]; ok && !isClaudeJSONNull(value) {
		var rows []json.RawMessage
		if json.Unmarshal(value, &rows) != nil {
			rows = []json.RawMessage{nil}
			complete = false
		}
		if len(rows) > claudeCacheMaxWindows {
			return nil, false, errors.New("Claude model usage windows are too numerous")
		}
		windows := make([]claudeUsageWindow, 0, len(rows))
		for _, row := range rows {
			window, usable := projectClaudeWindow(row, true, observedAt)
			windows = append(windows, window)
			complete = complete && usable
		}
		limits.ModelScoped = &windows
	}
	if value, ok := fields["limits"]; ok && !isClaudeJSONNull(value) {
		var rows []json.RawMessage
		if json.Unmarshal(value, &rows) == nil && len(rows) > claudeCacheMaxWindows {
			return nil, false, errors.New("Claude raw usage limits are too numerous")
		}
		rawWindows, reason := claudeRawWeeklyModelWindows(value)
		if len(rawWindows) > claudeCacheMaxWindows {
			return nil, false, errors.New("Claude raw usage limits are too numerous")
		}
		projectedCount := 0
		if limits.ModelScoped != nil {
			projectedCount = len(*limits.ModelScoped)
		}
		rawCount := len(rawWindows)
		if reason != "" {
			rawCount++
		}
		if projectedCount+rawCount > claudeCacheMaxWindows {
			return nil, false, errors.New("Claude usage windows are too numerous")
		}
		if limits.ModelScoped == nil {
			windows := []claudeUsageWindow{}
			limits.ModelScoped = &windows
		}
		for _, row := range rawWindows {
			encoded, err := json.Marshal(row)
			if err != nil {
				return nil, false, err
			}
			window, usable := projectClaudeWindow(encoded, true, observedAt)
			*limits.ModelScoped = append(*limits.ModelScoped, window)
			complete = complete && usable
		}
		if reason != "" {
			*limits.ModelScoped = append(*limits.ModelScoped, malformedClaudeWindow(true))
			complete = false
		}
	}
	if limits.ModelScoped != nil && len(*limits.ModelScoped) > claudeCacheMaxWindows {
		return nil, false, errors.New("Claude usage windows are too numerous")
	}
	return limits, complete, nil
}

func projectClaudeWindow(raw json.RawMessage, includeName bool, observedAt time.Time) (claudeUsageWindow, bool) {
	fields, err := rawJSONObject(raw)
	if err != nil {
		return malformedClaudeWindow(includeName), false
	}
	window := claudeUsageWindow{}
	validName := true
	if includeName {
		window.DisplayName, validName = projectClaudeModelName(fields["display_name"])
	}
	validUtilization := false
	if value := fields["utilization"]; len(value) != 0 && !isClaudeJSONNull(value) {
		var used float64
		if json.Unmarshal(value, &used) == nil && !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0 && used <= 100 {
			window.Utilization = &used
			validUtilization = true
		} else {
			invalid := 101.0
			window.Utilization = &invalid
		}
	}
	resetsAt, resetValid, resetAt := projectClaudeReset(fields["resets_at"])
	window.ResetsAt = resetsAt
	usable := validName && validUtilization && resetValid
	if resetAt == nil {
		if window.Utilization == nil || *window.Utilization != 0 {
			usable = false
		}
	} else if !resetAt.After(observedAt) {
		usable = false
	}
	return window, usable
}

func malformedClaudeWindow(includeName bool) claudeUsageWindow {
	window := claudeUsageWindow{ResetsAt: json.RawMessage(`"invalid"`)}
	if includeName {
		window.DisplayName = "unknown"
	}
	return window
}

func projectClaudeModelName(raw json.RawMessage) (string, bool) {
	var name string
	if json.Unmarshal(raw, &name) != nil || name == "" || len(name) > 128 {
		return "unknown", true
	}
	return canonicalClaudeModelLabel(name), true
}

func canonicalClaudeModelLabel(name string) string {
	if _, ok := claudeModelBucket(name); ok {
		return name
	}
	return "unknown"
}

func projectClaudeReset(raw json.RawMessage) (json.RawMessage, bool, *time.Time) {
	if len(raw) == 0 || isClaudeJSONNull(raw) {
		return json.RawMessage("null"), true, nil
	}
	reset, valid := claudeResetTime(raw)
	if !valid || reset == nil {
		return json.RawMessage(`"invalid"`), false, nil
	}
	encoded, _ := json.Marshal(reset.UTC().Format(time.RFC3339Nano))
	return encoded, true, reset
}

func claudeSnapshotDenials(limits *claudeRateLimits, observedAt time.Time) []claudeCachedDenial {
	if limits == nil {
		return nil
	}
	var out []claudeCachedDenial
	add := func(window *claudeUsageWindow, label string) {
		if window == nil || window.Utilization == nil || *window.Utilization != 100 {
			return
		}
		reset, valid := claudeResetTime(window.ResetsAt)
		if valid && reset != nil && reset.After(observedAt) {
			out = append(out, claudeCachedDenial{DisplayName: label, ResetsAt: reset.UTC(), ObservedAt: observedAt})
		}
	}
	add(limits.FiveHour, "")
	add(limits.SevenDay, "")
	add(limits.SevenDayOAuthApps, "")
	add(limits.SevenDayOpus, "Opus")
	add(limits.SevenDaySonnet, "Sonnet")
	if limits.ModelScoped != nil {
		for i := range *limits.ModelScoped {
			window := &(*limits.ModelScoped)[i]
			add(window, window.DisplayName)
		}
	}
	return compactClaudeDenials(out)
}

func claudeUncacheableDenials(payload []byte, observedAt time.Time) []claudeCachedDenial {
	var usage claudeUsageResponse
	if json.Unmarshal(payload, &usage) != nil || usage.RateLimits == nil {
		return nil
	}
	limits := *usage.RateLimits
	if limits.ModelScoped != nil {
		windows := append([]claudeUsageWindow(nil), (*limits.ModelScoped)...)
		for i := range windows {
			windows[i].DisplayName = canonicalClaudeModelLabel(windows[i].DisplayName)
		}
		limits.ModelScoped = &windows
	}
	denials := claudeSnapshotDenials(&limits, observedAt)
	rawWindows, _ := claudeRawWeeklyModelWindows(limits.Limits)
	for i := range rawWindows {
		rawWindows[i].DisplayName = canonicalClaudeModelLabel(rawWindows[i].DisplayName)
	}
	return compactClaudeDenials(append(denials, claudeSnapshotDenials(&claudeRateLimits{ModelScoped: &rawWindows}, observedAt)...))
}

func rawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || object == nil {
		return nil, errors.New("expected JSON object")
	}
	return object, nil
}

func isClaudeJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}
