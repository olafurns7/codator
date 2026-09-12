package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	claudeProbeTimeout = 20 * time.Second
	claudeMaxFrame     = 1 << 20
)

var claudeBlockedEnv = map[string]bool{
	"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true,
	"CLAUDE_CODE_OAUTH_TOKEN": true, "CLAUDE_CODE_OAUTH_REFRESH_TOKEN": true,
	"CLAUDE_CODE_OAUTH_SCOPES": true, "ANTHROPIC_PROFILE": true,
	"ANTHROPIC_ORGANIZATION_ID": true, "ANTHROPIC_FEDERATION_RULE_ID": true,
	"ANTHROPIC_WORKSPACE_ID": true, "AWS_BEARER_TOKEN_BEDROCK": true,
	"CLAUDE_CODE_USE_BEDROCK": true, "CLAUDE_CODE_USE_VERTEX": true,
	"CLAUDE_CODE_USE_FOUNDRY": true, "CLAUDE_CODE_USE_MANTLE": true,
	"CLAUDE_CODE_USE_ANTHROPIC_AWS": true, "ANTHROPIC_BASE_URL": true,
	"ANTHROPIC_CUSTOM_HEADERS": true, "ANTHROPIC_AWS_API_KEY": true,
	"ANTHROPIC_AWS_BASE_URL": true, "ANTHROPIC_AWS_WORKSPACE_ID": true,
	"ANTHROPIC_BEDROCK_BASE_URL": true, "ANTHROPIC_MANTLE_BASE_URL": true,
	"ANTHROPIC_VERTEX_BASE_URL": true, "ANTHROPIC_VERTEX_PROJECT_ID": true,
	"CLAUDE_CODE_CUSTOM_OAUTH_URL": true,
}

func claudeEnv(nativeDir string, base []string) []string {
	overrides := map[string]string{}
	if nativeDir != "" {
		if absolute, err := filepath.Abs(nativeDir); err == nil {
			nativeDir = absolute
		}
		overrides["CLAUDE_CONFIG_DIR"] = nativeDir
	}
	filtered := make([]string, 0, len(base))
	for _, item := range base {
		key, _, ok := cutEnv(item)
		if ok && !claudeEnvDenied(key) {
			filtered = append(filtered, item)
		}
	}
	return buildEnv(filtered, nil, overrides)
}

func claudeEnvDenied(key string) bool {
	key = strings.ToUpper(key)
	if claudeBlockedEnv[key] {
		return true
	}
	if strings.HasPrefix(key, "ANTHROPIC_") {
		return hasAny(key, "API_KEY", "AUTH_TOKEN", "OAUTH", "BASE_URL", "CUSTOM_HEADERS",
			"ORGANIZATION_ID", "FEDERATION_RULE_ID", "WORKSPACE_ID", "PROFILE",
			"BEARER_TOKEN", "ACCESS_TOKEN", "REFRESH_TOKEN", "CREDENTIAL", "SECRET", "BILLING", "SPEND")
	}
	if strings.HasPrefix(key, "CLAUDE_CODE_") {
		return strings.HasPrefix(key, "CLAUDE_CODE_USE_") ||
			hasAny(key, "OAUTH", "API_KEY", "AUTH_TOKEN", "BASE_URL", "CUSTOM_HEADERS", "BILLING", "SPEND", "EXTRA_USAGE")
	}
	if strings.HasPrefix(key, "AWS_") {
		return hasAny(key, "ACCESS_KEY", "SECRET", "SESSION_TOKEN", "BEARER_TOKEN", "CREDENTIAL") ||
			strings.HasSuffix(key, "_PROFILE")
	}
	return key == "GOOGLE_APPLICATION_CREDENTIALS" ||
		key == "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE" ||
		key == "CLOUDSDK_AUTH_ACCESS_TOKEN"
}

func hasAny(value string, parts ...string) bool {
	for _, part := range parts {
		if strings.Contains(value, part) {
			return true
		}
	}
	return false
}

func claudeProbeEnv(nativeDir string) []string {
	allowed := map[string]bool{"HOME": true, "PATH": true, "LANG": true, "LC_ALL": true, "TMPDIR": true}
	base := make([]string, 0, len(allowed)+2)
	for _, item := range os.Environ() {
		key, _, ok := cutEnv(item)
		if ok && allowed[key] {
			base = append(base, item)
		}
	}
	base = append(base, "DO_NOT_TRACK=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	return claudeEnv(nativeDir, base)
}

type claudeControlRequest struct {
	Type      string "json:\"type\""
	RequestID string "json:\"request_id\""
	Request   struct {
		Subtype       string "json:\"subtype\""
		SkipBehaviors *bool  "json:\"skip_behaviors,omitempty\""
	} "json:\"request\""
}

func writeClaudeControl(w io.Writer, id, subtype string) error {
	skip := subtype == "get_usage"
	request := claudeControlRequest{Type: "control_request", RequestID: id}
	request.Request.Subtype = subtype
	if skip {
		request.Request.SkipBehaviors = &skip
	}
	if err := json.NewEncoder(w).Encode(request); err != nil {
		return errors.New("could not write Claude control request")
	}
	return nil
}

func decodeClaudeControlFrame(frame []byte, id string) (json.RawMessage, bool, error) {
	var response struct {
		Type     string "json:\"type\""
		Response struct {
			Subtype   string          "json:\"subtype\""
			RequestID string          "json:\"request_id\""
			Response  json.RawMessage "json:\"response\""
		} "json:\"response\""
	}
	if err := json.Unmarshal(frame, &response); err != nil {
		return nil, false, errors.New("Claude control response is malformed")
	}
	if response.Type != "control_response" || response.Response.RequestID != id {
		return nil, false, nil
	}
	if response.Response.Subtype == "error" {
		return nil, true, errors.New("Claude rejected a control request")
	}
	if response.Response.Subtype != "success" {
		return nil, true, errors.New("Claude control response is malformed")
	}
	return response.Response.Response, true, nil
}

func awaitClaudeResponse(frames <-chan []byte, streamDone, deadline <-chan struct{}, id string) (json.RawMessage, error) {
	for {
		select {
		case <-deadline:
			return nil, errors.New("Claude usage probe timed out")
		case <-streamDone:
			return nil, errors.New("Claude exited before returning a control response")
		case frame := <-frames:
			payload, matched, err := decodeClaudeControlFrame(frame, id)
			if err != nil {
				return nil, err
			}
			if matched {
				return payload, nil
			}
		}
	}
}

func probeClaude(account Account) (quota, error) {
	return probeClaudeWithContext(context.Background(), account)
}

func probeClaudeWithContext(parent context.Context, account Account) (quota, error) {
	return probeClaudeWithModelContext(parent, account, "")
}

func probeClaudeWithModelContext(parent context.Context, account Account, model claudeModelFamily) (quota, error) {
	if account.Provider != "claude" || account.Err != nil || !filepath.IsAbs(account.NativeDir) {
		return quota{}, errors.New("Claude account profile is unavailable")
	}
	path, err := findNative("claude")
	if err != nil {
		return quota{}, err
	}
	ctx, cancel := context.WithTimeout(parent, claudeProbeTimeout)
	defer cancel()

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return quota{}, errors.New("cannot create Claude probe pipes")
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		return quota{}, errors.New("cannot create Claude probe pipes")
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		return quota{}, errors.New("cannot create Claude probe pipes")
	}

	args := []string{"--print", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--safe-mode", "--no-session-persistence", "--tools", ""}
	cmd := probeCommand(ctx, path, args...)
	cmd.Dir, cmd.Env = account.NativeDir, claudeProbeEnv(account.NativeDir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdinR, stdoutW, stderrW
	if err := cmd.Start(); err != nil {
		stdinR.Close()
		stdinW.Close()
		stdoutR.Close()
		stdoutW.Close()
		stderrR.Close()
		stderrW.Close()
		return quota{}, errors.New("cannot start Claude usage probe")
	}
	stdinR.Close()
	stdoutW.Close()
	stderrW.Close()

	frames := make(chan []byte)
	stopReader := make(chan struct{})
	streamDone, stderrDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(streamDone)
		scanner := bufio.NewScanner(stdoutR)
		scanner.Buffer(make([]byte, 64*1024), claudeMaxFrame)
		for scanner.Scan() {
			frame := append([]byte(nil), scanner.Bytes()...)
			select {
			case frames <- frame:
			case <-stopReader:
				return
			}
		}
	}()
	go func() {
		defer close(stderrDone)
		_, _ = io.Copy(io.Discard, stderrR)
	}()
	defer func() {
		close(stopReader)
		_ = stdinW.Close()
		finishProbe(cmd)
		_ = stdoutR.Close()
		_ = stderrR.Close()
		<-streamDone
		<-stderrDone
	}()

	if err := writeClaudeControl(stdinW, "init-1", "initialize"); err != nil {
		return quota{}, err
	}
	if _, err := awaitClaudeResponse(frames, streamDone, ctx.Done(), "init-1"); err != nil {
		return quota{}, err
	}
	if err := writeClaudeControl(stdinW, "usage-1", "get_usage"); err != nil {
		return quota{}, err
	}
	payload, err := awaitClaudeResponse(frames, streamDone, ctx.Done(), "usage-1")
	if err != nil {
		return quota{}, err
	}
	return parseClaudeQuotaForModel(payload, time.Now(), model)
}

type claudeModelFamily string

const (
	claudeModelOpus   claudeModelFamily = "opus"
	claudeModelSonnet claudeModelFamily = "sonnet"
	claudeModelHaiku  claudeModelFamily = "haiku"
	claudeModelFable  claudeModelFamily = "fable"
)

func claudeModelFamilyForName(name string) (claudeModelFamily, bool) {
	switch name {
	case "opus", "claude-opus-5":
		return claudeModelOpus, true
	case "sonnet", "claude-sonnet-5":
		return claudeModelSonnet, true
	case "haiku":
		return claudeModelHaiku, true
	case "fable", "claude-fable-5-1":
		return claudeModelFable, true
	default:
		return "", false
	}
}

type claudeUsageResponse struct {
	SubscriptionType    string            "json:\"subscription_type\""
	RateLimitsAvailable *bool             "json:\"rate_limits_available\""
	RateLimits          *claudeRateLimits "json:\"rate_limits\""
}

type claudeRateLimits struct {
	FiveHour          *claudeUsageWindow   "json:\"five_hour\""
	SevenDay          *claudeUsageWindow   "json:\"seven_day\""
	SevenDayOAuthApps *claudeUsageWindow   "json:\"seven_day_oauth_apps\""
	SevenDayOpus      *claudeUsageWindow   "json:\"seven_day_opus\""
	SevenDaySonnet    *claudeUsageWindow   "json:\"seven_day_sonnet\""
	ModelScoped       *[]claudeUsageWindow "json:\"model_scoped\""
}

type claudeUsageWindow struct {
	DisplayName string   "json:\"display_name\""
	Utilization *float64 "json:\"utilization\""
	ResetsAt    *string  "json:\"resets_at\""
}

func parseClaudeQuota(payload json.RawMessage, now time.Time) (quota, error) {
	return parseClaudeQuotaForModel(payload, now, "")
}

func parseClaudeQuotaForModel(payload json.RawMessage, now time.Time, model claudeModelFamily) (quota, error) {
	if len(payload) == 0 || string(payload) == "null" {
		return quota{}, errors.New("Claude usage response is missing")
	}
	var identity struct {
		SubscriptionType json.RawMessage "json:\"subscription_type\""
	}
	if err := json.Unmarshal(payload, &identity); err != nil {
		return quota{}, errors.New("Claude usage response is malformed")
	}
	var subscriptionType string
	_ = json.Unmarshal(identity.SubscriptionType, &subscriptionType)
	subscription := knownClaudeSubscription(subscriptionType)

	var usage claudeUsageResponse
	if err := json.Unmarshal(payload, &usage); err != nil {
		return quota{Subscription: subscription, Reason: "Claude usage response is malformed"}, nil
	}
	if usage.RateLimitsAvailable == nil {
		return quota{Subscription: subscription, Reason: "Claude rate limit availability is unknown"}, nil
	}
	if !subscription {
		allowed := false
		result := evaluateUsage(&allowed, nil, nil)
		return result, nil
	}
	if !*usage.RateLimitsAvailable {
		return quota{Subscription: true, Reason: "Claude rate limits are unavailable"}, nil
	}
	values, reason := claudeUsageWindows(usage.RateLimits, now, model)
	allowed := true
	result := evaluateUsage(&allowed, nil, values)
	if result.Known && !result.Eligible {
		result.Subscription = true
		return result, nil
	}
	if reason != "" {
		return quota{Subscription: true, Reason: reason}, nil
	}
	result.Subscription = true
	return result, nil
}

func knownClaudeSubscription(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "pro", "max", "team", "enterprise":
		return true
	default:
		return false
	}
}

func claudeUsageWindows(limits *claudeRateLimits, now time.Time, model claudeModelFamily) ([]float64, string) {
	if limits == nil {
		return nil, ""
	}
	values := make([]float64, 0, 8)
	var firstReason string
	add := func(window *claudeUsageWindow) string {
		if window == nil {
			return ""
		}
		if window.Utilization == nil {
			return "Claude usage window utilization is unknown"
		}
		used := *window.Utilization
		if used < 0 || used > 100 {
			return "Claude usage window utilization is malformed"
		}
		if window.ResetsAt != nil && *window.ResetsAt != "" {
			reset, err := time.Parse(time.RFC3339Nano, *window.ResetsAt)
			if err != nil {
				return "Claude usage window reset time is malformed"
			}
			if !reset.After(now) {
				return "Claude usage window is stale"
			}
		} else if used > 0 {
			return "Claude usage window reset time is unknown"
		}
		values = append(values, used)
		return ""
	}
	record := func(window *claudeUsageWindow) {
		if reason := add(window); firstReason == "" && reason != "" {
			firstReason = reason
		}
	}
	for _, window := range []*claudeUsageWindow{limits.FiveHour, limits.SevenDay, limits.SevenDayOAuthApps} {
		record(window)
	}
	knownModel := model == claudeModelOpus || model == claudeModelSonnet || model == claudeModelHaiku || model == claudeModelFable
	if !knownModel || model == claudeModelOpus {
		record(limits.SevenDayOpus)
	}
	if !knownModel || model == claudeModelSonnet {
		record(limits.SevenDaySonnet)
	}
	if limits.ModelScoped != nil {
		for _, window := range *limits.ModelScoped {
			family, known := claudeModelBucket(window.DisplayName)
			if !known || !knownModel || family == model {
				record(&window)
			}
		}
	}
	return values, firstReason
}

func claudeModelBucket(label string) (claudeModelFamily, bool) {
	switch label {
	case "Opus":
		return claudeModelOpus, true
	case "Sonnet":
		return claudeModelSonnet, true
	case "Haiku":
		return claudeModelHaiku, true
	case "Fable":
		return claudeModelFable, true
	default:
		return "", false
	}
}
