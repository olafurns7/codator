package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

var codexForcedConfig = []string{
	`model_provider="openai"`,
	`openai_base_url="https://chatgpt.com/backend-api/codex"`,
	`chatgpt_base_url="https://chatgpt.com/backend-api"`,
	`model_providers.openai.requires_openai_auth=true`,
}

var codexAuthEnv = []string{
	"CODEX_ACCESS_TOKEN", "CODEX_API_KEY", "OPENAI_API_KEY", "OPENAI_BASE_URL",
	"OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE",
	"CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE",
	"CODEX_APP_SERVER_LOGIN_CLIENT_ID", "CODEX_APP_SERVER_LOGIN_ISSUER",
}

type rpcRequest struct {
	Method string `json:"method"`
	ID     int    `json:"id,omitempty"`
	Params any    `json:"params,omitempty"`
}

type rpcResponse struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
}

type codexAccountResponse struct {
	Account *struct {
		Type     string `json:"type"`
		PlanType string `json:"planType"`
	} `json:"account"`
}

type codexUsageResponse struct {
	OrdinaryUsageAllowed *bool                              `json:"ordinaryUsageAllowed"`
	RateLimits           *codexRateLimitSnapshot            `json:"rateLimits"`
	ByLimitID            map[string]*codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
}

type codexRateLimitSnapshot struct {
	Primary      *codexRateWindow `json:"primary"`
	Secondary    *codexRateWindow `json:"secondary"`
	SpendReached *bool            `json:"spendControlReached"`
}

type codexRateWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *float64 `json:"windowDurationMins"`
	ResetsAt           *float64 `json:"resetsAt"`
}

func codexEnv(nativeDir string, base []string) []string {
	overrides := map[string]string{}
	if nativeDir != "" {
		overrides["CODEX_HOME"] = nativeDir
		overrides["CODEX_SQLITE_HOME"] = nativeDir
	}
	return buildEnv(base, codexAuthEnv, overrides)
}

func codexProbeArgs(args ...string) []string {
	all := make([]string, 0, 2*len(codexForcedConfig)+len(args))
	for _, config := range codexForcedConfig {
		all = append(all, "--config", config)
	}
	return append(all, args...)
}

func probeCodex(account Account) (quota, error) {
	return probeCodexWithContext(context.Background(), account)
}

func probeCodexWithContext(parent context.Context, account Account) (quota, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	return probeCodexContext(ctx, account)
}

func probeCodexContext(ctx context.Context, account Account) (quota, error) {
	path, err := findNative("codex")
	if err != nil {
		return quota{}, err
	}
	cmd := probeCommand(ctx, path, codexProbeArgs("app-server", "--listen", "stdio://")...)
	cmd.Env = codexEnv(account.NativeDir, os.Environ())
	cmd.Dir = account.NativeDir
	cmd.Stderr = io.Discard
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return quota{}, errors.New("cannot start Codex usage check")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return quota{}, errors.New("cannot start Codex usage check")
	}
	if err := cmd.Start(); err != nil {
		return quota{}, errors.New("cannot start Codex usage check")
	}
	defer finishProbe(cmd)
	defer stdin.Close()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(stdin)
	init := map[string]any{
		"clientInfo":   map[string]string{"name": "codator", "title": "Codator", "version": "0.1.0"},
		"capabilities": map[string]bool{"experimentalApi": false, "requestAttestation": false},
	}
	if err := sendRPC(encoder, "initialize", 1, init); err != nil {
		return quota{}, errors.New("Codex app-server handshake failed")
	}
	if _, err := readRPC(scanner, 1); err != nil {
		return quota{}, errors.New("Codex app-server handshake failed")
	}
	if err := encoder.Encode(rpcRequest{Method: "initialized", Params: struct{}{}}); err != nil {
		return quota{}, errors.New("Codex app-server handshake failed")
	}
	if err := sendRPC(encoder, "account/read", 2, map[string]bool{"refreshToken": false}); err != nil {
		return quota{}, errors.New("Codex account check failed")
	}
	accountResult, err := readRPC(scanner, 2)
	if err != nil {
		return quota{}, errors.New("Codex account check failed")
	}
	var accountInfo codexAccountResponse
	if json.Unmarshal(accountResult, &accountInfo) != nil || accountInfo.Account == nil {
		return quota{Reason: "subscription account is unknown"}, errors.New("Codex account is not a verified subscription")
	}
	if accountInfo.Account.Type != "chatgpt" {
		return quota{Known: true, Reason: "account is not a ChatGPT subscription"}, nil
	}
	if !codexSubscriptionPlan(accountInfo.Account.PlanType) {
		return quota{Reason: "account plan is not a verified subscription"}, nil
	}
	verified := quota{Subscription: true}
	if err := sendRPC(encoder, "account/rateLimits/read", 3, nil); err != nil {
		verified.Reason = "Codex usage check failed"
		return verified, nil
	}
	result, err := readRPC(scanner, 3)
	if err != nil {
		verified.Reason = "Codex usage check failed"
		return verified, nil
	}
	var usage codexUsageResponse
	if json.Unmarshal(result, &usage) != nil {
		verified.Reason = "usage response is malformed"
		return verified, nil
	}
	if usage.OrdinaryUsageAllowed == nil {
		verified.Reason = "ordinary usage permission is unknown"
		return verified, nil
	}
	if !*usage.OrdinaryUsageAllowed {
		result := evaluateUsage(usage.OrdinaryUsageAllowed, nil, nil)
		result.Subscription = true
		return result, nil
	}
	return codexQuotaFromUsage(usage.OrdinaryUsageAllowed, usage), nil
}

func codexSubscriptionPlan(planType string) bool {
	switch planType {
	case "go", "plus", "pro", "prolite", "team",
		"self_serve_business_prolite", "self_serve_business_usage_based", "business", "ent26",
		"enterprise_cbp_automation", "enterprise_cbp_usage_based", "enterprise",
		"edu", "edu_plus", "edu_pro":
		return true
	default:
		return false
	}
}

func sendRPC(encoder *json.Encoder, method string, id int, params any) error {
	return encoder.Encode(rpcRequest{Method: method, ID: id, Params: params})
}

func readRPC(scanner *bufio.Scanner, expectedID int) (json.RawMessage, error) {
	want := strconv.Itoa(expectedID)
	for scanner.Scan() {
		var response rpcResponse
		if json.Unmarshal(scanner.Bytes(), &response) != nil {
			return nil, errors.New("invalid JSON-RPC response")
		}
		id := strings.TrimSpace(string(response.ID))
		if id == "" || id == "null" {
			continue
		}
		if id != want {
			continue
		}
		if len(response.Error) != 0 && string(response.Error) != "null" {
			return nil, errors.New("JSON-RPC request failed")
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return nil, errors.New("empty JSON-RPC result")
		}
		return response.Result, nil
	}
	if scanner.Err() != nil {
		return nil, errors.New("JSON-RPC response exceeded limits or was unreadable")
	}
	return nil, errors.New("Codex app-server closed before responding")
}

func codexQuotaFromUsage(ordinary *bool, usage codexUsageResponse) quota {
	if ordinary == nil || !*ordinary {
		return evaluateUsage(ordinary, nil, nil)
	}
	buckets := make([]*codexRateLimitSnapshot, 0, len(usage.ByLimitID))
	var primary *codexRateLimitSnapshot
	if usage.ByLimitID != nil {
		primary = usage.ByLimitID["codex"]
		for _, bucket := range usage.ByLimitID {
			buckets = append(buckets, bucket)
		}
	} else if usage.RateLimits != nil {
		primary = usage.RateLimits
		buckets = append(buckets, usage.RateLimits)
	}
	if len(buckets) == 0 {
		return quota{Subscription: true, Reason: "Codex usage buckets are missing"}
	}

	spendUnknown := primary == nil || primary.SpendReached == nil
	spendReached := primary != nil && primary.SpendReached != nil && *primary.SpendReached
	var windowsUnknown bool
	var used []float64
	for _, bucket := range buckets {
		if bucket == nil {
			windowsUnknown = true
			continue
		}
		if bucket.SpendReached != nil && *bucket.SpendReached {
			spendReached = true
		}
		windows, ok := codexWindowPercentages(bucket)
		used = append(used, windows...)
		if !ok {
			windowsUnknown = true
		}
	}
	if spendReached {
		reached := true
		q := evaluateUsage(ordinary, &reached, nil)
		q.Subscription = true
		return q
	}
	spendControl := false
	q := evaluateUsage(ordinary, &spendControl, used)
	if q.Known && !q.Eligible {
		q.Subscription = true
		return q
	}
	if spendUnknown {
		q = quota{Reason: "spend control status is unknown"}
	} else if windowsUnknown {
		q = quota{Reason: "usage windows are missing, malformed, or stale"}
	}
	q.Subscription = true
	return q
}

func codexWindowPercentages(snapshot *codexRateLimitSnapshot) ([]float64, bool) {
	var used []float64
	valid := true
	for _, window := range []*codexRateWindow{snapshot.Primary, snapshot.Secondary} {
		if window == nil {
			continue
		}
		if window.UsedPercent == nil {
			valid = false
			continue
		}
		percent := *window.UsedPercent
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
			valid = false
			continue
		}
		if window.WindowDurationMins != nil && (*window.WindowDurationMins < 0 || *window.WindowDurationMins == 0 && percent != 0) {
			valid = false
			continue
		}
		if window.ResetsAt != nil && (*window.ResetsAt < 0 || *window.ResetsAt > 0 && *window.ResetsAt <= float64(time.Now().Unix())) {
			valid = false
			continue
		}
		used = append(used, percent)
	}
	return used, valid && len(used) > 0
}
