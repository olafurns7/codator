package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

const codexFileConfig = `cli_auth_credentials_store="file"`

var codexForcedConfig = []string{
	codexFileConfig,
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

var codexUnsafeFlags = map[string]bool{
	"--remote": true, "--remote-auth-token-env": true, "--profile": true, "-p": true,
	"--config": true, "-c": true, "--oss": true, "--local-provider": true,
	"--with-api-key": true, "--with-access-token": true,
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
		Type string `json:"type"`
	} `json:"account"`
}

type codexUsageResponse struct {
	OrdinaryUsageAllowed *bool                              `json:"ordinaryUsageAllowed"`
	RateLimits           *codexRateLimitSnapshot            `json:"rateLimits"`
	ByLimitID            map[string]*codexRateLimitSnapshot `json:"rateLimitsByLimitId"`
}

type codexRateLimitSnapshot struct {
	LimitID         *string          `json:"limitId"`
	NormalModelSlug *string          `json:"normalModelSlug"`
	Primary         *codexRateWindow `json:"primary"`
	Secondary       *codexRateWindow `json:"secondary"`
	SpendReached    *bool            `json:"spendControlReached"`
}

type codexRateWindow struct {
	UsedPercent        *float64 `json:"usedPercent"`
	WindowDurationMins *float64 `json:"windowDurationMins"`
	ResetsAt           *float64 `json:"resetsAt"`
}

func codexEnv(nativeDir string, base []string) []string {
	return buildEnv(base, codexAuthEnv, map[string]string{
		"CODEX_HOME":        nativeDir,
		"CODEX_SQLITE_HOME": nativeDir,
	})
}

func validateCodexLoginArgs(args []string) error {
	return rejectOptions(args, codexUnsafeFlags)
}

func validateCodexLaunchArgs(args []string) error {
	return rejectOptions(args, codexUnsafeFlags)
}

func codexCLIArgs(args ...string) []string {
	all := make([]string, 0, 2*len(codexForcedConfig)+len(args))
	for _, config := range codexForcedConfig {
		all = append(all, "--config", config)
	}
	return append(all, args...)
}

func probeCodex(account Account, model string, explicitModel bool) (quota, error) {
	return probeCodexWithContext(context.Background(), account, model, explicitModel)
}

func probeCodexWithContext(parent context.Context, account Account, model string, explicitModel bool) (quota, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	return probeCodexContext(ctx, account, model, explicitModel)
}

func probeCodexContext(ctx context.Context, account Account, model string, explicitModel bool) (quota, error) {
	path, err := findNative("codex")
	if err != nil {
		return quota{}, err
	}
	cmd := probeCommand(ctx, path, codexCLIArgs("app-server", "--listen", "stdio://")...)
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
	snapshot, err := codexSnapshotForModel(model, explicitModel, usage)
	if err != nil {
		verified.Reason = err.Error()
		return verified, nil
	}
	if snapshot.SpendReached == nil {
		verified.Reason = "spend control status is unknown"
		return verified, nil
	}
	if *snapshot.SpendReached {
		result := evaluateUsage(usage.OrdinaryUsageAllowed, snapshot.SpendReached, nil)
		result.Subscription = true
		return result, nil
	}
	used, ok := codexWindowPercentages(snapshot)
	if !ok {
		verified.Reason = "usage windows are missing, malformed, or stale"
		return verified, nil
	}
	q := evaluateUsage(usage.OrdinaryUsageAllowed, snapshot.SpendReached, used)
	q.Subscription = true
	return q, nil
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

func codexSnapshotForModel(model string, explicit bool, usage codexUsageResponse) (*codexRateLimitSnapshot, error) {
	if !explicit || model == "default" {
		if usage.ByLimitID != nil {
			if snapshot := usage.ByLimitID["codex"]; snapshot != nil {
				return snapshot, nil
			}
			return nil, errors.New("no unambiguous standard Codex usage bucket")
		}
		if usage.RateLimits != nil && usage.RateLimits.LimitID != nil && *usage.RateLimits.LimitID == "codex" {
			return usage.RateLimits, nil
		}
		return nil, errors.New("no unambiguous standard Codex usage bucket")
	}
	if usage.ByLimitID == nil {
		return nil, errors.New("no usage bucket for the requested model")
	}
	var match *codexRateLimitSnapshot
	for id, snapshot := range usage.ByLimitID {
		if snapshot == nil {
			continue
		}
		if id == model || snapshot.NormalModelSlug != nil && *snapshot.NormalModelSlug == model {
			if match != nil {
				return nil, errors.New("model matches multiple usage buckets")
			}
			match = snapshot
		}
	}
	if match == nil {
		return nil, errors.New("no unique usage bucket for the requested model")
	}
	return match, nil
}

func codexWindowPercentages(snapshot *codexRateLimitSnapshot) ([]float64, bool) {
	var used []float64
	for _, window := range []*codexRateWindow{snapshot.Primary, snapshot.Secondary} {
		if window == nil {
			continue
		}
		if window.UsedPercent == nil {
			return nil, false
		}
		percent := *window.UsedPercent
		if window.WindowDurationMins != nil && (*window.WindowDurationMins < 0 || *window.WindowDurationMins == 0 && percent != 0) {
			return nil, false
		}
		if window.ResetsAt != nil && *window.ResetsAt < 0 || window.ResetsAt != nil && *window.ResetsAt > 0 && *window.ResetsAt <= float64(time.Now().Unix()) {
			return nil, false
		}
		used = append(used, percent)
	}
	return used, len(used) > 0
}
