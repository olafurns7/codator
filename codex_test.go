package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	envFile := filepath.Join(binDir, "probe-env")
	argsFile := filepath.Join(binDir, "probe-args")
	for key, value := range map[string]string{
		"HOME":                             filepath.Join(binDir, "home"),
		"DBUS_SESSION_BUS_ADDRESS":         "unix:path=" + filepath.Join(binDir, "session-bus"),
		"XDG_RUNTIME_DIR":                  filepath.Join(binDir, "runtime"),
		"LANG":                             "synthetic-lang",
		"LC_ALL":                           "synthetic-locale",
		"LC_CTYPE":                         "synthetic-ctype",
		"TMPDIR":                           filepath.Join(binDir, "tmpdir"),
		"TMP":                              filepath.Join(binDir, "tmp"),
		"TEMP":                             filepath.Join(binDir, "temp"),
		"HTTP_PROXY":                       "http://upper-http-proxy.invalid",
		"HTTPS_PROXY":                      "http://upper-https-proxy.invalid",
		"ALL_PROXY":                        "socks5://upper-all-proxy.invalid",
		"NO_PROXY":                         "upper-no-proxy.invalid",
		"http_proxy":                       "http://lower-http-proxy.invalid",
		"https_proxy":                      "http://lower-https-proxy.invalid",
		"all_proxy":                        "socks5://lower-all-proxy.invalid",
		"no_proxy":                         "lower-no-proxy.invalid",
		"SSL_CERT_FILE":                    filepath.Join(binDir, "cert.pem"),
		"CODEX_CA_CERTIFICATE":             filepath.Join(binDir, "codex-ca.pem"),
		"SSL_CERT_DIR":                     filepath.Join(binDir, "certs"),
		"NODE_EXTRA_CA_CERTS":              filepath.Join(binDir, "node-extra-ca.pem"),
		"GITHUB_TOKEN":                     "synthetic-github-token",
		"AWS_SECRET_ACCESS_KEY":            "synthetic-aws-secret",
		"AWS_ACCESS_KEY_ID":                "synthetic-aws-access-key",
		"OPENAI_API_KEY":                   "synthetic-openai-key",
		"OPENAI_BASE_URL":                  "https://attacker.example",
		"OPENAI_FEDERATION_RULE_ID":        "synthetic-openai-rule",
		"OPENAI_IDENTITY_TOKEN_FILE":       filepath.Join(binDir, "identity-token"),
		"CODEX_ACCESS_TOKEN":               "synthetic-codex-token",
		"XDG_DATA_HOME":                    filepath.Join(binDir, "ambient-data"),
		"NODE_OPTIONS":                     "--require=/tmp/attacker.js",
		"LD_PRELOAD":                       filepath.Join(binDir, "attacker.so"),
		"DYLD_INSERT_LIBRARIES":            filepath.Join(binDir, "attacker.dylib"),
		"OPENAI_WORKLOAD_IDENTITY_CONTEXT": "synthetic-workload",
	} {
		t.Setenv(key, value)
	}
	ambientCodexHome := filepath.Join(binDir, "ambient-codex")
	t.Setenv("CODEX_HOME", ambientCodexHome)
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(binDir, "ambient-sqlite"))

	writeStub := func(mode string) {
		script := `#!/bin/sh
pwd -P > __CWD_FILE__
{
  printf 'GITHUB_TOKEN=%s\n' "${GITHUB_TOKEN-<unset>}"
  printf 'AWS_SECRET_ACCESS_KEY=%s\n' "${AWS_SECRET_ACCESS_KEY-<unset>}"
  printf 'AWS_ACCESS_KEY_ID=%s\n' "${AWS_ACCESS_KEY_ID-<unset>}"
  printf 'OPENAI_API_KEY=%s\n' "${OPENAI_API_KEY-<unset>}"
  printf 'OPENAI_BASE_URL=%s\n' "${OPENAI_BASE_URL-<unset>}"
  printf 'OPENAI_FEDERATION_RULE_ID=%s\n' "${OPENAI_FEDERATION_RULE_ID-<unset>}"
  printf 'OPENAI_IDENTITY_TOKEN_FILE=%s\n' "${OPENAI_IDENTITY_TOKEN_FILE-<unset>}"
  printf 'CODEX_ACCESS_TOKEN=%s\n' "${CODEX_ACCESS_TOKEN-<unset>}"
  printf 'OPENAI_WORKLOAD_IDENTITY_CONTEXT=%s\n' "${OPENAI_WORKLOAD_IDENTITY_CONTEXT-<unset>}"
  printf 'NODE_OPTIONS=%s\n' "${NODE_OPTIONS-<unset>}"
  printf 'LD_PRELOAD=%s\n' "${LD_PRELOAD-<unset>}"
  printf 'DYLD_INSERT_LIBRARIES=%s\n' "${DYLD_INSERT_LIBRARIES-<unset>}"
  printf 'XDG_DATA_HOME=%s\n' "${XDG_DATA_HOME-<unset>}"
  printf 'CODEX_HOME=%s\n' "${CODEX_HOME-<unset>}"
  printf 'CODEX_SQLITE_HOME=%s\n' "${CODEX_SQLITE_HOME-<unset>}"
  printf 'HOME=%s\n' "${HOME-<unset>}"
  printf 'DBUS_SESSION_BUS_ADDRESS=%s\n' "${DBUS_SESSION_BUS_ADDRESS-<unset>}"
  printf 'XDG_RUNTIME_DIR=%s\n' "${XDG_RUNTIME_DIR-<unset>}"
  printf 'PATH=%s\n' "${PATH-<unset>}"
  printf 'LANG=%s\n' "${LANG-<unset>}"
  printf 'LC_ALL=%s\n' "${LC_ALL-<unset>}"
  printf 'LC_CTYPE=%s\n' "${LC_CTYPE-<unset>}"
  printf 'TMPDIR=%s\n' "${TMPDIR-<unset>}"
  printf 'TMP=%s\n' "${TMP-<unset>}"
  printf 'TEMP=%s\n' "${TEMP-<unset>}"
  printf 'HTTP_PROXY=%s\n' "${HTTP_PROXY-<unset>}"
  printf 'HTTPS_PROXY=%s\n' "${HTTPS_PROXY-<unset>}"
  printf 'ALL_PROXY=%s\n' "${ALL_PROXY-<unset>}"
  printf 'NO_PROXY=%s\n' "${NO_PROXY-<unset>}"
  printf 'http_proxy=%s\n' "${http_proxy-<unset>}"
  printf 'https_proxy=%s\n' "${https_proxy-<unset>}"
  printf 'all_proxy=%s\n' "${all_proxy-<unset>}"
  printf 'no_proxy=%s\n' "${no_proxy-<unset>}"
  printf 'SSL_CERT_FILE=%s\n' "${SSL_CERT_FILE-<unset>}"
  printf 'CODEX_CA_CERTIFICATE=%s\n' "${CODEX_CA_CERTIFICATE-<unset>}"
  printf 'SSL_CERT_DIR=%s\n' "${SSL_CERT_DIR-<unset>}"
  printf 'NODE_EXTRA_CA_CERTS=%s\n' "${NODE_EXTRA_CA_CERTS-<unset>}"
} > __ENV_FILE__
printf '%s\n' "$@" > __ARGS_FILE__
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"id":1,"result":{}}' ;;
    *'"method":"account/read"'*) printf '%s\n' '{"id":2,"result":{"account":{"type":"chatgpt","email":null,"planType":"plus"},"requiresOpenaiAuth":true}}' ;;
    *'"method":"account/rateLimits/read"'*)
      if [ "__MODE__" = error ]; then
        printf '%s\n' '{"id":3,"error":{"code":-1,"message":"fixture failure"}}'
      else
        response='{"id":3,"result":{"ordinaryUsageAllowed":true,"rateLimits":{"limitId":"codex","primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false},"rateLimitsByLimitId":{"codex":{"limitId":"codex","primary":{"usedPercent":20,"windowDurationMins":300,"resetsAt":4102444800},"secondary":{"usedPercent":60,"windowDurationMins":10080,"resetsAt":4102444800},"spendControlReached":false}}}}'
        if [ "__MODE__" = unknown ]; then
          response=$(printf '%s\n' "$response" | sed 's/"spendControlReached":false/"spendControlReached":null/g')
        fi
        if [ "__MODE__" = exhausted ]; then
          response='{"id":3,"result":{"ordinaryUsageAllowed":false,"rateLimits":{"limitId":"codex","limitName":null,"primary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":4102444800},"secondary":null,"spendControlReached":false,"rateLimitReachedType":"rate_limit_reached"},"rateLimitsByLimitId":{"codex":{"limitId":"codex","limitName":null,"primary":{"usedPercent":100,"windowDurationMins":10080,"resetsAt":4102444800},"secondary":null,"spendControlReached":false},"codex_spark":{"limitId":"codex_spark","limitName":"GPT-5.3-Codex-Spark","primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":4102444800},"secondary":null,"spendControlReached":false}},"rateLimitResetCredits":{"availableCount":1,"credits":[]}}}'
        fi
        printf '%s\n' "$response"
      fi
      ;;
  esac
done
`
		script = strings.NewReplacer(
			"__CWD_FILE__", shellQuote(cwdFile),
			"__ENV_FILE__", shellQuote(envFile),
			"__ARGS_FILE__", shellQuote(argsFile),
			"__MODE__", mode,
		).Replace(script)
		if err := os.WriteFile(stub, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}

	parentPath := os.Getenv("PATH")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+parentPath)
	writeStub("success")
	quota, err := probeCodex(account)
	if err != nil || !quota.Eligible || quota.Headroom != 40 {
		t.Fatalf("quota=%+v err=%v", quota, err)
	}
	// Status shows every window with its reset, in the viewer's zone.
	now := time.Date(2099, 12, 30, 12, 0, 0, 0, time.UTC)
	want := "40.0% headroom\n" +
		"  5-hour limit: 80.0% remaining, resets Fri Jan 1 00:00 (in 1d 12h)\n" +
		"  Weekly limit: 40.0% remaining, resets Fri Jan 1 00:00 (in 1d 12h)"
	if got := quotaStatus(quota, nil, now); got != want {
		t.Fatalf("status=%q, want %q", got, want)
	}
	// An exhausted account keeps its windows, so status says when it returns.
	writeStub("exhausted")
	quota, err = probeCodex(account)
	if err != nil || !quota.Subscription || !quota.Known || quota.Eligible {
		t.Fatalf("exhausted quota=%+v err=%v", quota, err)
	}
	want = "limit reached until Fri Jan 1 00:00 (in 1d 12h)\n" +
		"  Weekly limit: 0.0% remaining (exhausted), resets Fri Jan 1 00:00 (in 1d 12h)\n" +
		"  GPT-5.3-Codex-Spark 5-hour limit: 75.0% remaining, resets Fri Jan 1 00:00 (in 1d 12h)\n" +
		"  Rate-limit resets available in Codex: 1"
	if got := quotaStatus(quota, nil, now); got != want {
		t.Fatalf("status=%q, want %q", got, want)
	}
	writeStub("error")
	quota, err = probeCodex(account)
	if err != nil || !quota.Subscription || quota.Known || quota.Eligible || quota.Reason == "" {
		t.Fatalf("rate-limit failure lost verified identity: quota=%+v err=%v", quota, err)
	}
	if !canLaunchExplicit(quota) {
		t.Fatalf("verified paid identity with unknown quota cannot launch explicitly: %+v", quota)
	}
	writeStub("unknown")
	quota, err = probeCodex(account)
	if err != nil || !quota.Subscription || quota.Known || quota.Eligible || quota.Reason != "spend control status is unknown" {
		t.Fatalf("unknown spend-control status was treated as eligible: quota=%+v err=%v", quota, err)
	}
	if got, err := os.ReadFile(cwdFile); err != nil || strings.TrimSpace(string(got)) != account.NativeDir {
		t.Fatalf("probe cwd=%q err=%v, want %q", got, err, account.NativeDir)
	}
	gotEnvBytes, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	gotEnv := envMap(strings.Split(strings.TrimSpace(string(gotEnvBytes)), "\n"))
	for key, want := range map[string]string{
		"CODEX_HOME":               account.NativeDir,
		"CODEX_SQLITE_HOME":        account.NativeDir,
		"HOME":                     filepath.Join(binDir, "home"),
		"DBUS_SESSION_BUS_ADDRESS": "unix:path=" + filepath.Join(binDir, "session-bus"),
		"XDG_RUNTIME_DIR":          filepath.Join(binDir, "runtime"),
		"PATH":                     binDir + string(os.PathListSeparator) + parentPath,
		"LANG":                     "synthetic-lang",
		"LC_ALL":                   "synthetic-locale",
		"LC_CTYPE":                 "synthetic-ctype",
		"TMPDIR":                   filepath.Join(binDir, "tmpdir"),
		"TMP":                      filepath.Join(binDir, "tmp"),
		"TEMP":                     filepath.Join(binDir, "temp"),
		"HTTP_PROXY":               "http://upper-http-proxy.invalid",
		"HTTPS_PROXY":              "http://upper-https-proxy.invalid",
		"ALL_PROXY":                "socks5://upper-all-proxy.invalid",
		"NO_PROXY":                 "upper-no-proxy.invalid",
		"http_proxy":               "http://lower-http-proxy.invalid",
		"https_proxy":              "http://lower-https-proxy.invalid",
		"all_proxy":                "socks5://lower-all-proxy.invalid",
		"no_proxy":                 "lower-no-proxy.invalid",
		"SSL_CERT_FILE":            filepath.Join(binDir, "cert.pem"),
		"CODEX_CA_CERTIFICATE":     filepath.Join(binDir, "codex-ca.pem"),
		"SSL_CERT_DIR":             filepath.Join(binDir, "certs"),
		"NODE_EXTRA_CA_CERTS":      filepath.Join(binDir, "node-extra-ca.pem"),
	} {
		if gotEnv[key] != want {
			t.Errorf("probe environment %s=%q, want %q", key, gotEnv[key], want)
		}
	}
	for _, key := range []string{
		"GITHUB_TOKEN", "AWS_SECRET_ACCESS_KEY", "AWS_ACCESS_KEY_ID", "OPENAI_API_KEY",
		"OPENAI_BASE_URL", "OPENAI_FEDERATION_RULE_ID", "OPENAI_IDENTITY_TOKEN_FILE",
		"CODEX_ACCESS_TOKEN", "OPENAI_WORKLOAD_IDENTITY_CONTEXT", "NODE_OPTIONS", "LD_PRELOAD",
		"DYLD_INSERT_LIBRARIES", "XDG_DATA_HOME",
	} {
		if gotEnv[key] != "<unset>" {
			t.Errorf("probe environment retained %s=%q", key, gotEnv[key])
		}
	}
	gotArgs, err := os.ReadFile(argsFile)
	if err != nil || !strings.Contains(string(gotArgs), `model_provider="openai"`) || !strings.Contains(string(gotArgs), `openai_base_url="https://chatgpt.com/backend-api/codex"`) {
		t.Fatalf("probe args=%q err=%v", gotArgs, err)
	}
}

func TestCodexProbeArgsUseBuiltinSubscriptionProvider(t *testing.T) {
	got := strings.Join(codexProbeArgs("app-server", "--listen", "stdio://"), " ")
	for _, value := range []string{
		`model_provider="openai"`,
		`openai_base_url="https://chatgpt.com/backend-api/codex"`,
		`chatgpt_base_url="https://chatgpt.com/backend-api"`,
	} {
		if !strings.Contains(got, value) {
			t.Errorf("probe config is missing %q: %s", value, got)
		}
	}
	if strings.Contains(got, `model_providers.openai.requires_openai_auth`) {
		t.Errorf("probe config overrides the reserved openai provider: %s", got)
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
