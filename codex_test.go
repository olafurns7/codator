package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexQuotaUsesAllBucketsAndLegacyFallback(t *testing.T) {
	ordinary, notSpent, spent := true, false, true
	codexPrimary, codexSecondary := 20.0, 40.0
	modelPrimary, modelSecondary := 80.0, 60.0
	legacyExhausted := 100.0
	standard := &codexRateLimitSnapshot{
		Primary:      &codexRateWindow{UsedPercent: &codexPrimary},
		Secondary:    &codexRateWindow{UsedPercent: &codexSecondary},
		SpendReached: &notSpent,
	}
	model := &codexRateLimitSnapshot{
		Primary:      &codexRateWindow{UsedPercent: &modelPrimary},
		Secondary:    &codexRateWindow{UsedPercent: &modelSecondary},
		SpendReached: &notSpent,
	}
	usage := codexUsageResponse{
		RateLimits: &codexRateLimitSnapshot{
			Primary:      &codexRateWindow{UsedPercent: &legacyExhausted},
			SpendReached: &notSpent,
		},
		ByLimitID: map[string]*codexRateLimitSnapshot{"codex": standard, "gpt-5-codex": model},
	}
	got := codexQuotaFromUsage(&ordinary, usage)
	if !got.Eligible || got.Headroom != 20 {
		t.Fatalf("quota ignored the tightest reported bucket: %+v", got)
	}

	modelPrimary = 100
	got = codexQuotaFromUsage(&ordinary, usage)
	if got.Eligible || !got.Known || got.Headroom != 0 {
		t.Fatalf("quota ignored an exhausted model bucket: %+v", got)
	}

	legacyPrimary, legacySecondary := 25.0, 50.0
	legacy := &codexRateLimitSnapshot{
		Primary:      &codexRateWindow{UsedPercent: &legacyPrimary},
		Secondary:    &codexRateWindow{UsedPercent: &legacySecondary},
		SpendReached: &notSpent,
	}
	got = codexQuotaFromUsage(&ordinary, codexUsageResponse{RateLimits: legacy})
	if !got.Eligible || got.Headroom != 50 {
		t.Fatalf("legacy quota fallback = %+v", got)
	}

	if got := codexQuotaFromUsage(&ordinary, codexUsageResponse{}); got.Eligible || got.Known {
		t.Fatalf("missing quota was not unknown: %+v", got)
	}
	unknownSpend := &codexRateLimitSnapshot{Primary: &codexRateWindow{UsedPercent: &codexPrimary}}
	unknownPrimary := &codexRateLimitSnapshot{Primary: &codexRateWindow{UsedPercent: &codexPrimary}}
	if got := codexQuotaFromUsage(&ordinary, codexUsageResponse{ByLimitID: map[string]*codexRateLimitSnapshot{"codex": unknownPrimary, "unknown": unknownSpend}}); got.Eligible || got.Known {
		t.Fatalf("unknown primary spend status was treated as eligible: %+v", got)
	}
	spentBucket := &codexRateLimitSnapshot{SpendReached: &spent}
	if got := codexQuotaFromUsage(&ordinary, codexUsageResponse{ByLimitID: map[string]*codexRateLimitSnapshot{"codex": standard, "spent": spentBucket}}); got.Eligible || !got.Known {
		t.Fatalf("spent bucket was ignored: %+v", got)
	}
}

func TestCodexAdditionalBucketNullSpendUsesPrimaryStatus(t *testing.T) {
	ordinary, notSpent := true, false
	primaryUsed, additionalUsed := 20.0, 80.0
	futureReset := 4102444800.0
	primary := &codexRateLimitSnapshot{
		Primary:      &codexRateWindow{UsedPercent: &primaryUsed, ResetsAt: &futureReset},
		SpendReached: &notSpent,
	}
	additional := &codexRateLimitSnapshot{
		Primary: &codexRateWindow{UsedPercent: &additionalUsed, ResetsAt: &futureReset},
	}
	usage := codexUsageResponse{ByLimitID: map[string]*codexRateLimitSnapshot{
		"codex": primary, "codex_other": additional,
	}}
	if got := codexQuotaFromUsage(&ordinary, usage); !got.Eligible || got.Headroom != 20 {
		t.Fatalf("native-shaped null additional spend status blocked ranking: %+v", got)
	}

	additionalUsed = 100
	if got := codexQuotaFromUsage(&ordinary, usage); got.Eligible || !got.Known || got.Headroom != 0 {
		t.Fatalf("exhausted additional window was not blocked: %+v", got)
	}

	additionalUsed = 80
	primary.SpendReached = nil
	if got := codexQuotaFromUsage(&ordinary, usage); got.Eligible || got.Known {
		t.Fatalf("unknown primary spend status was treated as eligible: %+v", got)
	}
}

func TestCodexFreshExhaustionSurvivesMalformedSibling(t *testing.T) {
	ordinary, notSpent := true, false
	futureReset, staleReset, exhausted := 4102444800.0, 1.0, 100.0
	tests := []struct {
		name      string
		primary   *codexRateWindow
		secondary *codexRateWindow
	}{
		{
			name:      "exhausted window before malformed sibling",
			primary:   &codexRateWindow{UsedPercent: &exhausted, ResetsAt: &futureReset},
			secondary: &codexRateWindow{},
		},
		{
			name:      "malformed sibling before exhausted window",
			primary:   &codexRateWindow{},
			secondary: &codexRateWindow{UsedPercent: &exhausted, ResetsAt: &futureReset},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := &codexRateLimitSnapshot{
				Primary: test.primary, Secondary: test.secondary, SpendReached: &notSpent,
			}
			got := codexQuotaFromUsage(&ordinary, codexUsageResponse{RateLimits: snapshot})
			if !got.Subscription || !got.Known || got.Eligible || canLaunchExplicit(got) {
				t.Fatalf("fresh exhaustion was lost beside malformed data: %+v", got)
			}
		})
	}

	staleOnly := &codexRateLimitSnapshot{
		Primary:      &codexRateWindow{UsedPercent: &exhausted, ResetsAt: &staleReset},
		SpendReached: &notSpent,
	}
	got := codexQuotaFromUsage(&ordinary, codexUsageResponse{RateLimits: staleOnly})
	if !got.Subscription || got.Known || got.Eligible || !canLaunchExplicit(got) {
		t.Fatalf("stale-only exhaustion was not kept unknown: %+v", got)
	}
}

func TestCodexPlanIdentityGatesAutomaticAndUnknownQuotaSelection(t *testing.T) {
	for _, plan := range []string{
		"go", "plus", "pro", "prolite", "team", "self_serve_business_prolite",
		"self_serve_business_usage_based", "business", "ent26", "enterprise_cbp_automation",
		"enterprise_cbp_usage_based", "enterprise", "edu", "edu_plus", "edu_pro",
	} {
		if !codexSubscriptionPlan(plan) {
			t.Errorf("did not recognize subscription plan %q", plan)
		}
	}
	for _, plan := range []string{"", "free", "unknown-plan"} {
		subscription := codexSubscriptionPlan(plan)
		eligibleWithKnownUsage := quota{Subscription: subscription, Eligible: true}
		eligibleAutomatically := eligibleWithKnownUsage.Subscription && eligibleWithKnownUsage.Eligible
		unknownQuota := quota{Subscription: subscription}
		if eligibleAutomatically || canLaunchExplicit(unknownQuota) {
			t.Errorf("unverified plan %q passed selection: automatic=%v explicit=%v",
				plan, eligibleAutomatically, canLaunchExplicit(unknownQuota))
		}
	}
	if !canLaunchExplicit(quota{Subscription: codexSubscriptionPlan("plus")}) {
		t.Fatal("verified paid identity should allow explicit launch when only quota is unknown")
	}
}

func TestCodexZeroAndStaleWindows(t *testing.T) {
	zero := 0.0
	resetZero := 0.0
	ordinary := true
	for _, resetsAt := range []*float64{nil, &resetZero} {
		valid := &codexRateLimitSnapshot{Primary: &codexRateWindow{UsedPercent: &zero, ResetsAt: resetsAt}}
		used, ok := codexWindowPercentages(valid)
		if !ok || len(used) != 1 || evaluateUsage(&ordinary, nil, used).Headroom != 100 {
			t.Fatalf("valid zero window used=%v ok=%v resetsAt=%v", used, ok, resetsAt)
		}
	}
	missing := &codexRateLimitSnapshot{Primary: &codexRateWindow{ResetsAt: nil}}
	if _, ok := codexWindowPercentages(missing); ok {
		t.Fatal("treated missing utilization as zero")
	}
	usedPercent, past := 10.0, 1.0
	stale := &codexRateLimitSnapshot{Primary: &codexRateWindow{UsedPercent: &usedPercent, ResetsAt: &past}}
	if _, ok := codexWindowPercentages(stale); ok {
		t.Fatal("accepted a stale nonzero usage window")
	}
}

func TestCodexAppServerProbeUsesLocalProtocolFixture(t *testing.T) {
	dataHome := tempDataHome(t)
	store, err := newStore(dataHome)
	if err != nil {
		t.Fatal(err)
	}
	account, err := store.EnsureAccount("codex", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	if err := os.Chmod(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	stub := filepath.Join(binDir, "codex")
	cwdFile := filepath.Join(binDir, "probe-cwd")
	baseURLFile := filepath.Join(binDir, "inherited-base-url")
	argsFile := filepath.Join(binDir, "probe-args")
	t.Setenv("CODATOR_PROBE_CWD_FILE", cwdFile)
	t.Setenv("CODATOR_BASE_URL_FILE", baseURLFile)
	t.Setenv("CODATOR_PROBE_ARGS_FILE", argsFile)
	t.Setenv("OPENAI_BASE_URL", "https://attacker.example")
	script := `#!/bin/sh
pwd -P > "$CODATOR_PROBE_CWD_FILE"
printf '%s\n' "${OPENAI_BASE_URL-}" > "$CODATOR_BASE_URL_FILE"
printf '%s\n' "$@" > "$CODATOR_PROBE_ARGS_FILE"
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","email":null,"planType":"plus"},"requiresOpenaiAuth":true}}' ;;
	  *'"method":"account/rateLimits/read"'*)
	    if [ "${CODATOR_RATE_LIMITS_ERROR-}" = 1 ]; then
	      printf '%s\n' '{"id":3,"error":{"code":-1,"message":"fixture failure"}}'
	    else
	      response='{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false}}}}'
	      if [ "${CODATOR_SPEND_UNKNOWN-}" = 1 ]; then
	        response=$(printf '%s\n' "$response" | sed 's/"spendControlReached":false/"spendControlReached":null/g')
	      fi
	      printf '%s\n' "$response"
	    fi
	    ;;
  esac
done
`
	if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	quota, err := probeCodex(account)
	if err != nil || !quota.Eligible || quota.Headroom != 40 {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
	t.Setenv("CODATOR_RATE_LIMITS_ERROR", "1")
	quota, err = probeCodex(account)
	if err != nil || !quota.Subscription || quota.Known || quota.Eligible || quota.Reason == "" {
		t.Fatalf("rate-limit failure lost verified identity: quota=%+v err=%v", quota, err)
	}
	if !canLaunchExplicit(quota) {
		t.Fatalf("verified paid identity with unknown quota cannot launch explicitly: %+v", quota)
	}
	t.Setenv("CODATOR_RATE_LIMITS_ERROR", "")
	t.Setenv("CODATOR_SPEND_UNKNOWN", "1")
	quota, err = probeCodex(account)
	if err != nil || !quota.Subscription || quota.Known || quota.Eligible || quota.Reason != "spend control status is unknown" {
		t.Fatalf("unknown spend-control status was treated as eligible: quota=%+v err=%v", quota, err)
	}
	if got, err := os.ReadFile(cwdFile); err != nil || strings.TrimSpace(string(got)) != account.NativeDir {
		t.Fatalf("probe cwd=%q err=%v, want %q", got, err, account.NativeDir)
	}
	if got, err := os.ReadFile(baseURLFile); err != nil || strings.TrimSpace(string(got)) != "" {
		t.Fatalf("probe inherited OPENAI_BASE_URL: %q err=%v", got, err)
	}
	gotArgs, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(gotArgs), `model_provider="openai"`) || !strings.Contains(string(gotArgs), `openai_base_url="https://chatgpt.com/backend-api/codex"`) {
		t.Fatalf("probe args=%q err=%v", gotArgs, err)
	}
}

func TestCodexProbeArgsForceSubscriptionProvider(t *testing.T) {
	got := strings.Join(codexProbeArgs("app-server", "--listen", "stdio://"), " ")
	for _, value := range []string{
		`model_provider="openai"`,
		`openai_base_url="https://chatgpt.com/backend-api/codex"`,
		`chatgpt_base_url="https://chatgpt.com/backend-api"`,
		`requires_openai_auth=true`,
	} {
		if !strings.Contains(got, value) {
			t.Errorf("probe config is missing %q: %s", value, got)
		}
	}
}

func TestCodexEnvRemovesAuthAndEndpointOverrides(t *testing.T) {
	base := []string{
		"OPENAI_BASE_URL=https://attacker.example",
		"CODEX_REFRESH_TOKEN_URL_OVERRIDE=https://attacker.example/token",
		"CODEX_REVOKE_TOKEN_URL_OVERRIDE=https://attacker.example/revoke",
		"CODEX_APP_SERVER_LOGIN_CLIENT_ID=attacker",
		"CODEX_APP_SERVER_LOGIN_ISSUER=https://attacker.example",
		"CODEX_ACCESS_TOKEN=secret",
		"OPENAI_WORKLOAD_IDENTITY_CONTEXT=metadata",
	}
	got := envMap(codexEnv("/private/profile", base))
	for _, key := range []string{"OPENAI_BASE_URL", "CODEX_REFRESH_TOKEN_URL_OVERRIDE", "CODEX_REVOKE_TOKEN_URL_OVERRIDE", "CODEX_APP_SERVER_LOGIN_CLIENT_ID", "CODEX_APP_SERVER_LOGIN_ISSUER", "CODEX_ACCESS_TOKEN"} {
		if _, ok := got[key]; ok {
			t.Errorf("inherited %s", key)
		}
	}
	if got["CODEX_HOME"] != "/private/profile" || got["CODEX_SQLITE_HOME"] != "/private/profile" || got["OPENAI_WORKLOAD_IDENTITY_CONTEXT"] != "metadata" {
		t.Fatalf("forced or allowed environment is wrong: %+v", got)
	}
}

func stringPtr(value string) *string { return &value }
