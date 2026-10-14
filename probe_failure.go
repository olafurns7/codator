package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const probeRetryInterval = time.Minute
const probeRefreshInterval = 10 * time.Second

type probeFailure struct {
	Kind    string
	RetryAt time.Time
}

func (e *probeFailure) Error() string { return "usage probe failed: " + e.Kind }

// Native error bodies may contain credentials; retain only the class and retry deadline.
func nativeProbeFailure(raw json.RawMessage) error {
	failure := &probeFailure{Kind: "malformed"}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return failure
	}
	var auth, limited, transport bool
	var messages []string
	record := func(kind string) {
		auth = auth || kind == "auth"
		limited = limited || kind == "rate_limit"
		transport = transport || kind == "transport"
	}
	var inspect func(any)
	inspect = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				switch strings.ToLower(key) {
				case "error", "data":
					inspect(child)
				case "code", "type", "status", "statuscode", "status_code", "http_status", "httpstatuscode", "http_status_code":
					switch code := child.(type) {
					case string:
						record(nativeProbeCode(code))
					case float64:
						record(nativeProbeCode(strconv.FormatFloat(code, 'f', -1, 64)))
					}
				case "message":
					if message, ok := child.(string); ok {
						messages = append(messages, message)
					}
				case "headers":
					if headers, ok := child.(map[string]any); ok {
						for header, value := range headers {
							if strings.EqualFold(header, "Retry-After") {
								inspect(map[string]any{"retry_after": value})
							}
						}
					}
				case "retry-after", "retry_after", "retryafter":
					switch retry := child.(type) {
					case float64:
						setProbeRetry(failure, retry)
					case string:
						if seconds, err := strconv.ParseFloat(retry, 64); err == nil {
							setProbeRetry(failure, seconds)
						} else if at, err := http.ParseTime(retry); err == nil && at.After(time.Now()) && at.Before(time.Now().Add(365*24*time.Hour)) && at.After(failure.RetryAt) {
							failure.RetryAt = at
						}
					}
				}
			}
		case string:
			messages = append(messages, v)
		}
	}
	inspect(value)
	// Explicit error fields take precedence over diagnostic text and unrelated metadata.
	if !auth && !limited && !transport {
		for _, message := range messages {
			record(nativeProbeMessage(message))
		}
	}
	switch {
	case auth:
		failure.Kind = "auth"
	case limited:
		failure.Kind = "rate_limit"
	case transport:
		failure.Kind = "transport"
	}
	return failure
}

func nativeProbeCode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "401", "403", "authentication_error", "unauthorized", "invalid_grant", "token_expired", "refresh_token_reused", "invalid_token", "invalid_api_key":
		return "auth"
	case "429", "rate_limit_error", "rate_limit_exceeded", "too_many_requests":
		return "rate_limit"
	case "enotfound", "econnreset", "econnrefused", "etimedout", "eai_again", "network_error", "timeout":
		return "transport"
	}
	if status, err := strconv.Atoi(value); err == nil && status >= 500 && status <= 599 {
		return "transport"
	}
	return ""
}

// Only native HTTP-error prefixes or complete auth messages can establish a denial.
func nativeProbeMessage(value string) string {
	text := strings.ToLower(strings.TrimSpace(value))
	if match := nativeHTTPErrorPattern.FindStringSubmatch(text); match != nil {
		return nativeProbeCode(match[1])
	}
	if _, err := strconv.Atoi(text); err != nil {
		if kind := nativeProbeCode(text); kind != "" {
			return kind
		}
	}
	prefix, _, _ := strings.Cut(text, ":")
	switch prefix {
	case "token expired", "login expired", "not logged in", "please log in", "please login", "authentication failed", "invalid access token", "invalid authentication", "refresh token has expired", "invalid token":
		return "auth"
	case "rate limit exceeded", "rate limited":
		return "rate_limit"
	}
	if strings.HasPrefix(text, "usage api error:") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "usage api error:"))
	}
	if nativeTransportErrorPattern.MatchString(text) {
		return "transport"
	}
	return ""
}

var nativeHTTPErrorPattern = regexp.MustCompile(`^(?:usage api error:|http(?: error)?[: ]+)\s*(401|403|429|5[0-9]{2})(?:$|[ :])`)
var nativeTransportErrorPattern = regexp.MustCompile(`^(?:fetch failed|network error|connection error|request timed out|timed out|timeout|enotfound|econnreset|econnrefused|etimedout|eai_again)(?:$|[ :;])`)

func setProbeRetry(failure *probeFailure, seconds float64) {
	// Bound arithmetic before converting an untrusted Retry-After to a duration.
	if seconds >= 0 && seconds <= float64((365*24*time.Hour)/time.Second) {
		at := time.Now().UTC().Add(time.Duration(seconds * float64(time.Second)))
		if at.After(failure.RetryAt) {
			failure.RetryAt = at
		}
	}
}

func probeFailureOf(err error) *probeFailure {
	var failure *probeFailure
	if errors.As(err, &failure) {
		return failure
	}
	return &probeFailure{Kind: "transport"}
}

func probeFailureReason(account Account, kind string) string {
	switch kind {
	case "auth":
		return fmt.Sprintf("%s login expired or invalid; run: codator login %s %s", account.Provider, account.Provider, account.Name)
	case "rate_limit":
		return account.Provider + " usage probe was rate limited"
	case "malformed":
		return account.Provider + " usage response is malformed; run: codator status --refresh " + account.Provider
	default:
		return account.Provider + " usage probe transport/network/timeout failure; check connectivity and run: codator status --refresh " + account.Provider
	}
}
