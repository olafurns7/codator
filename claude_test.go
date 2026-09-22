package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestClaudeEnvSanitizesInheritedAuth(t *testing.T) {
	env := envMap(claudeEnv("profiles/work", []string{
		"PATH=/usr/bin", "TERM=xterm", "GITHUB_TOKEN=keep",
		"CLAUDE_CONFIG_DIR=/tmp/other", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/tmp/other", "ANTHROPIC_API_KEY=fake",
		"ANTHROPIC_VENDOR_API_KEY=fake", "CLAUDE_CODE_FUTURE_OAUTH_TOKEN=fake",
		"CLAUDE_CODE_USE_FOUNDRY=1", "AWS_SECRET_ACCESS_KEY=fake",
	}))
	if !filepath.IsAbs(env["CLAUDE_CONFIG_DIR"]) || env["CLAUDE_CONFIG_DIR"] == "/tmp/other" {
		t.Fatalf("config dir was not forced absolute: %q", env["CLAUDE_CONFIG_DIR"])
	}
	if env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] != env["CLAUDE_CONFIG_DIR"] {
		t.Fatalf("secure storage dir = %q, want config dir %q", env["CLAUDE_SECURESTORAGE_CONFIG_DIR"], env["CLAUDE_CONFIG_DIR"])
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_VENDOR_API_KEY", "CLAUDE_CODE_FUTURE_OAUTH_TOKEN", "CLAUDE_CODE_USE_FOUNDRY", "AWS_SECRET_ACCESS_KEY"} {
		if _, ok := env[key]; ok {
			t.Fatalf("sensitive environment key %q survived", key)
		}
	}
	if env["PATH"] != "/usr/bin" || env["TERM"] != "xterm" || env["GITHUB_TOKEN"] != "keep" {
		t.Fatalf("ordinary environment was not preserved: %#v", env)
	}
}

func TestClaudeEnvStripsInheritedStorageOverridesWithoutProfile(t *testing.T) {
	env := envMap(claudeEnv("", []string{
		"CLAUDE_CONFIG_DIR=/tmp/other", "CLAUDE_SECURESTORAGE_CONFIG_DIR=/tmp/other",
		"CLAUDE_SECURESTORAGE_CONFIG_DIR=",
	}))
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR"} {
		if _, ok := env[key]; ok {
			t.Fatalf("inherited %s survived without a profile: %#v", key, env)
		}
	}
}

func TestClaudeEnvKeepsAccountsInDistinctConfigAndSecureStorageDirs(t *testing.T) {
	root := tempDataHome(t)
	first := filepath.Join(root, "claude", "work", "native")
	second := filepath.Join(root, "claude", "personal", "native")
	firstEnv := envMap(claudeEnv(first, []string{"CLAUDE_SECURESTORAGE_CONFIG_DIR=/hostile"}))
	secondEnv := envMap(claudeEnv(second, []string{"CLAUDE_SECURESTORAGE_CONFIG_DIR="}))
	for _, env := range []map[string]string{firstEnv, secondEnv} {
		if env["CLAUDE_CONFIG_DIR"] == "" || env["CLAUDE_CONFIG_DIR"] != env["CLAUDE_SECURESTORAGE_CONFIG_DIR"] {
			t.Fatalf("profile was not pinned to one storage directory: %#v", env)
		}
	}
	if firstEnv["CLAUDE_CONFIG_DIR"] == secondEnv["CLAUDE_CONFIG_DIR"] {
		t.Fatalf("distinct accounts share a storage directory: first=%q second=%q", firstEnv["CLAUDE_CONFIG_DIR"], secondEnv["CLAUDE_CONFIG_DIR"])
	}
}

func TestParseClaudeQuotaUnknownZeroAndStale(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	future := "2026-09-13T12:00:00Z"
	tests := []struct {
		name         string
		rates        string
		wantEligible bool
		wantKnown    bool
		wantHeadroom float64
	}{
		{"zero with unstarted reset", "\"rate_limits\":{\"five_hour\":{\"utilization\":0,\"resets_at\":null}}", true, true, 100},
		{"zero with stale reset", "\"rate_limits\":{\"five_hour\":{\"utilization\":0,\"resets_at\":\"2026-09-12T11:00:00Z\"}}", false, false, 0},
		{"missing utilization", "\"rate_limits\":{\"five_hour\":{}}", false, false, 0},
		{"missing reset with nonzero use", "\"rate_limits\":{\"five_hour\":{\"utilization\":20,\"resets_at\":null}}", false, false, 0},
		{"stale nonzero window", "\"rate_limits\":{\"five_hour\":{\"utilization\":20,\"resets_at\":\"2026-09-12T11:00:00Z\"}}", false, false, 0},
		{"exhausted window", "\"rate_limits\":{\"five_hour\":{\"utilization\":100,\"resets_at\":\"" + future + "\"}}", false, true, 0},
		{"absent windows", "\"rate_limits\":{\"five_hour\":null,\"seven_day\":null,\"model_scoped\":[]}", false, false, 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := json.RawMessage("{\"subscription_type\":\"max\",\"rate_limits_available\":true," + test.rates + "}")
			got, err := parseClaudeQuota(raw, now)
			if err != nil {
				t.Fatal(err)
			}
			if got.Eligible != test.wantEligible || got.Known != test.wantKnown || got.Headroom != test.wantHeadroom {
				t.Fatalf("quota = %#v", got)
			}
			if !got.Subscription {
				t.Fatalf("verified plan was not retained: %#v", got)
			}
		})
	}

	malformed, err := parseClaudeQuota(json.RawMessage("{\"subscription_type\":\"team\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":\"bad\"}}}"), now)
	if err != nil || !malformed.Subscription || malformed.Eligible || malformed.Known {
		t.Fatalf("malformed usage did not preserve subscription: %#v, %v", malformed, err)
	}
	unavailable, err := parseClaudeQuota(json.RawMessage("{\"subscription_type\":\"team\",\"rate_limits_available\":false,\"rate_limits\":null}"), now)
	if err != nil || !unavailable.Subscription || unavailable.Eligible || unavailable.Known {
		t.Fatalf("unavailable quota lost verified subscription: %#v, %v", unavailable, err)
	}
	unknownPlan, err := parseClaudeQuota(json.RawMessage("{\"subscription_type\":\"free\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":0}}}"), now)
	if err != nil || unknownPlan.Subscription || unknownPlan.Eligible {
		t.Fatalf("unknown plan was accepted: %#v, %v", unknownPlan, err)
	}
}

func TestClaudeQuotaIncludesAllModelWindows(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	raw := json.RawMessage("{\"subscription_type\":\"max\",\"rate_limits_available\":true,\"rate_limits\":{" +
		"\"five_hour\":{\"utilization\":10,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"seven_day_opus\":{\"utilization\":100,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"seven_day_sonnet\":{\"utilization\":30,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"model_scoped\":[" +
		"{\"display_name\":\"Opus\",\"utilization\":100,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"{\"display_name\":\"Sonnet\",\"utilization\":25,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"{\"display_name\":\"Haiku\",\"utilization\":15,\"resets_at\":\"2026-09-13T12:00:00Z\"}]}}")
	allModels, err := parseClaudeQuota(raw, now)
	if err != nil || allModels.Eligible || !allModels.Known {
		t.Fatalf("quota ignored an exhausted model window: %#v, %v", allModels, err)
	}
	sonnet, err := parseClaudeQuotaForModel(raw, now, claudeModelSonnet)
	if err != nil || !sonnet.Eligible || sonnet.Headroom != 70 {
		t.Fatalf("Sonnet quota included irrelevant model windows: %#v, %v", sonnet, err)
	}
	opus, err := parseClaudeQuotaForModel(raw, now, claudeModelOpus)
	if err != nil || opus.Eligible || !opus.Known {
		t.Fatalf("Opus quota ignored its exhausted windows: %#v, %v", opus, err)
	}

	ambiguous := json.RawMessage("{\"subscription_type\":\"pro\",\"rate_limits_available\":true,\"rate_limits\":{" +
		"\"five_hour\":{\"utilization\":10,\"resets_at\":\"2026-09-13T12:00:00Z\"}," +
		"\"model_scoped\":[{\"display_name\":\"weekly\",\"utilization\":80,\"resets_at\":\"2026-09-13T12:00:00Z\"}]}}")
	conservative, err := parseClaudeQuota(ambiguous, now)
	if err != nil || !conservative.Eligible || conservative.Headroom != 20 {
		t.Fatalf("ambiguous model window was ignored: %#v, %v", conservative, err)
	}
}

func TestClaudeQuotaModelBucketsKeepSharedAndUnknownLimits(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	reset := "2026-09-13T12:00:00Z"
	payload := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{` +
		`"five_hour":{"utilization":15,"resets_at":"` + reset + `"},` +
		`"seven_day":{"utilization":20,"resets_at":"` + reset + `"},` +
		`"seven_day_oauth_apps":{"utilization":5,"resets_at":"` + reset + `"},` +
		`"seven_day_opus":{"utilization":100,"resets_at":"` + reset + `"},` +
		`"seven_day_sonnet":{"utilization":30,"resets_at":"` + reset + `"},` +
		`"model_scoped":[` +
		`{"display_name":"Fable","utilization":100,"resets_at":"` + reset + `"},` +
		`{"display_name":"Sonnet","utilization":25,"resets_at":"` + reset + `"},` +
		`{"display_name":"Haiku","utilization":25,"resets_at":"` + reset + `"},` +
		`{"display_name":"Opus","utilization":100,"resets_at":"` + reset + `"},` +
		`{"display_name":"future bucket","utilization":10,"resets_at":"` + reset + `"}]}}`)
	sonnet, err := parseClaudeQuotaForModel(payload, now, claudeModelSonnet)
	if err != nil || !sonnet.Eligible || sonnet.Headroom != 70 {
		t.Fatalf("Sonnet did not score shared + Sonnet + unknown windows: %#v, %v", sonnet, err)
	}
	fable, err := parseClaudeQuotaForModel(payload, now, claudeModelFable)
	if err != nil || fable.Eligible || !fable.Known {
		t.Fatalf("Fable exhaustion was ignored: %#v, %v", fable, err)
	}
	haikuHint := claudeModelHint([]string{"--model", "haiku"})
	if haikuHint != claudeModelHaiku {
		t.Fatalf("Haiku model hint = %q, want %q", haikuHint, claudeModelHaiku)
	}
	haiku, err := parseClaudeQuotaForModel(payload, now, haikuHint)
	if err != nil || !haiku.Eligible || haiku.Headroom != 75 {
		t.Fatalf("Haiku was blocked by an irrelevant Fable bucket or ignored its own bucket: %#v, %v", haiku, err)
	}

	unknownBucket := json.RawMessage(strings.Replace(string(payload),
		`{"display_name":"future bucket","utilization":10`,
		`{"display_name":"future bucket","utilization":100`, 1))
	for _, model := range []claudeModelFamily{claudeModelSonnet, claudeModelHaiku} {
		blockedByUnknown, err := parseClaudeQuotaForModel(unknownBucket, now, model)
		if err != nil || blockedByUnknown.Eligible || !blockedByUnknown.Known {
			t.Fatalf("unknown bucket was ignored for %s: %#v, %v", model, blockedByUnknown, err)
		}
	}
	sharedExhaustion := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":100,"resets_at":"` + reset + `"},"model_scoped":[{"display_name":"Sonnet","utilization":0,"resets_at":"` + reset + `"},{"display_name":"Fable","utilization":0,"resets_at":"` + reset + `"},{"display_name":"Haiku","utilization":0,"resets_at":"` + reset + `"}]}}`)
	for _, model := range []claudeModelFamily{claudeModelFable, claudeModelOpus, claudeModelSonnet, claudeModelHaiku} {
		blockedByShared, err := parseClaudeQuotaForModel(sharedExhaustion, now, model)
		if err != nil || blockedByShared.Eligible || !blockedByShared.Known {
			t.Fatalf("shared exhaustion did not block %s: %#v, %v", model, blockedByShared, err)
		}
	}
	unknownModel, err := parseClaudeQuotaForModel(payload, now, claudeModelFamily("future"))
	if err != nil || unknownModel.Eligible || !unknownModel.Known {
		t.Fatalf("unknown model was not scored conservatively: %#v, %v", unknownModel, err)
	}

	malformedIrrelevant := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":20,"resets_at":"` + reset + `"},"model_scoped":[{"display_name":"Fable","utilization":101,"resets_at":"bad"},{"display_name":"Sonnet","utilization":25,"resets_at":"` + reset + `"}]}}`)
	knownSonnet, err := parseClaudeQuotaForModel(malformedIrrelevant, now, claudeModelSonnet)
	if err != nil || !knownSonnet.Eligible || knownSonnet.Headroom != 75 {
		t.Fatalf("irrelevant malformed model window affected Sonnet: %#v, %v", knownSonnet, err)
	}
}

func TestClaudeQuotaReadsRawWeeklyModelLimitsBesidePartialProjection(t *testing.T) {
	now := time.Date(2026, 9, 12, 20, 30, 0, 0, time.UTC)
	reset := "2026-09-12T23:59:59Z"
	numericReset := strconv.FormatInt(time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC).Unix(), 10)
	payload := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{` +
		`"five_hour":{"utilization":0,"resets_at":"` + reset + `"},` +
		`"seven_day":{"utilization":10,"resets_at":"` + reset + `"},` +
		`"model_scoped":[{"display_name":"Sonnet","utilization":20,"resets_at":` + numericReset + `}],` +
		`"limits":[` +
		`{"kind":"other","percent":100,"resets_at":` + numericReset + `},` +
		`{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}},"resets_at":"` + reset + `"},` +
		`{"kind":"weekly_scoped","percent":35,"scope":{"model":{"display_name":"Sonnet"}},"resets_at":` + numericReset + `},` +
		`{"kind":"weekly_scoped","percent":60,"scope":{"model":{"display_name":"Opus"}},"resets_at":` + numericReset + `}]}}`)
	sonnetModel, sonnetKnown := claudeModelFamilyForName("claude-sonnet-5")
	opusModel, opusKnown := claudeModelFamilyForName("claude-opus-5")
	fableModel, fableKnown := claudeModelFamilyForName("claude-fable-5-1")
	if !sonnetKnown || !opusKnown || !fableKnown {
		t.Fatal("known Claude model aliases were not recognized")
	}
	sonnet, err := parseClaudeQuotaForModel(payload, now, sonnetModel)
	if err != nil || !sonnet.Eligible || sonnet.Headroom != 65 {
		t.Fatalf("Sonnet ignored its raw cap or included Fable's: %+v, %v", sonnet, err)
	}
	opus, err := parseClaudeQuotaForModel(payload, now, opusModel)
	if err != nil || !opus.Eligible || opus.Headroom != 40 {
		t.Fatalf("Opus did not use its numeric-reset raw cap: %+v, %v", opus, err)
	}
	fable, err := parseClaudeQuotaForModel(payload, now, fableModel)
	if err != nil || !fable.Subscription || !fable.Known || fable.Eligible || fable.Headroom != 0 {
		t.Fatalf("Fable raw exhaustion was lost beside a partial projection: %+v, %v", fable, err)
	}
}

func TestClaudeRawWeeklyModelLimitsRequireFableScopeAndValidateFields(t *testing.T) {
	now := time.Date(2026, 9, 12, 20, 30, 0, 0, time.UTC)
	reset := "2026-09-12T23:59:59Z"
	makePayload := func(limits, projection string) json.RawMessage {
		projected := ""
		if projection != "" {
			projected = `"model_scoped":` + projection + ","
		}
		return json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{` +
			`"five_hour":{"utilization":0,"resets_at":"` + reset + `"},` +
			`"seven_day":{"utilization":10,"resets_at":"` + reset + `"},` +
			projected + `"limits":` + limits + `}}`)
	}
	tests := []struct {
		name         string
		limits       string
		projection   string
		model        claudeModelFamily
		wantEligible bool
		wantKnown    bool
		wantHeadroom float64
	}{
		{
			name:       "missing Fable scope stays unknown despite shared windows",
			limits:     `[]`,
			projection: `[{"display_name":"Sonnet","utilization":20,"resets_at":"` + reset + `"}]`,
			model:      claudeModelFable,
		},
		{
			name:   "stale numeric reset",
			limits: `[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}},"resets_at":1}]`,
			model:  claudeModelFable,
		},
		{
			name:   "missing reset with nonzero usage",
			limits: `[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}}}]`,
			model:  claudeModelFable,
		},
		{
			name:   "malformed numeric utilization",
			limits: `[{"kind":"weekly_scoped","percent":"bad","scope":{"model":{"display_name":"Fable"}},"resets_at":"` + reset + `"}]`,
			model:  claudeModelFable,
		},
		{
			name:   "malformed reset type",
			limits: `[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}},"resets_at":true}]`,
			model:  claudeModelFable,
		},
		{
			name:         "zero usage accepts missing reset",
			limits:       `[{"kind":"weekly_scoped","percent":0,"scope":{"model":{"display_name":"Fable"}}}]`,
			model:        claudeModelFable,
			wantEligible: true,
			wantKnown:    true,
			wantHeadroom: 90,
		},
		{
			name:      "unknown raw scope conservatively applies to Sonnet",
			limits:    `[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Future"}},"resets_at":"` + reset + `"}]`,
			model:     claudeModelSonnet,
			wantKnown: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseClaudeQuotaForModel(makePayload(test.limits, test.projection), now, test.model)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Subscription || got.Eligible != test.wantEligible || got.Known != test.wantKnown || got.Headroom != test.wantHeadroom {
				t.Fatalf("quota = %+v", got)
			}
		})
	}

	for _, limits := range []string{
		`[{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}},"resets_at":"` + reset + `"},{"kind":"weekly_scoped","percent":"bad","scope":{"model":{"display_name":"Fable"}},"resets_at":"` + reset + `"}]`,
		`[{"kind":"weekly_scoped","percent":"bad","scope":{"model":{"display_name":"Fable"}},"resets_at":"` + reset + `"},{"kind":"weekly_scoped","percent":100,"scope":{"model":{"display_name":"Fable"}},"resets_at":"` + reset + `"}]`,
	} {
		got, err := parseClaudeQuotaForModel(makePayload(limits, ""), now, claudeModelFable)
		if err != nil || !got.Subscription || !got.Known || got.Eligible || got.Headroom != 0 {
			t.Fatalf("fresh raw exhaustion was lost beside a malformed sibling: %+v, %v", got, err)
		}
	}
}

func TestClaudeControlResponseMatchingAndErrors(t *testing.T) {
	other := []byte("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"other\"}}")
	if _, matched, err := decodeClaudeControlFrame(other, "init-1"); err != nil || matched {
		t.Fatalf("unmatched response = matched %v, err %v", matched, err)
	}
	ok := []byte("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"usage-1\",\"response\":{\"subscription_type\":\"pro\"}}}")
	payload, matched, err := decodeClaudeControlFrame(ok, "usage-1")
	if err != nil || !matched || string(payload) != "{\"subscription_type\":\"pro\"}" {
		t.Fatalf("matching response = %s, matched %v, err %v", payload, matched, err)
	}
	failed := []byte("{\"type\":\"control_response\",\"response\":{\"subtype\":\"error\",\"request_id\":\"usage-1\",\"error\":\"secret-token\"}}")
	_, matched, err = decodeClaudeControlFrame(failed, "usage-1")
	if !matched || err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("error response leaked native details: matched %v, err %v", matched, err)
	}
}

func TestClaudeFreshExhaustionSurvivesMalformedSibling(t *testing.T) {
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		rates string
	}{
		{
			name: "exhausted window before malformed sibling",
			rates: `{"five_hour":{"utilization":100,"resets_at":"2026-09-13T12:00:00Z"},` +
				`"seven_day":{"utilization":101,"resets_at":"2026-09-13T12:00:00Z"}}`,
		},
		{
			name: "malformed sibling before exhausted window",
			rates: `{"five_hour":{"utilization":101,"resets_at":"2026-09-13T12:00:00Z"},` +
				`"seven_day":{"utilization":100,"resets_at":"2026-09-13T12:00:00Z"}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload := json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":` + test.rates + `}`)
			got, err := parseClaudeQuota(payload, now)
			if err != nil {
				t.Fatal(err)
			}
			if !got.Subscription || !got.Known || got.Eligible || canLaunchExplicit(got) {
				t.Fatalf("fresh exhaustion was lost beside malformed data: %+v", got)
			}
		})
	}

	staleOnly, err := parseClaudeQuota(json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":100,"resets_at":"2026-09-12T11:00:00Z"}}}`), now)
	if err != nil || !staleOnly.Subscription || staleOnly.Known || staleOnly.Eligible || !canLaunchExplicit(staleOnly) {
		t.Fatalf("stale-only exhaustion was not kept unknown: %+v, %v", staleOnly, err)
	}

	modelExhaustion, err := parseClaudeQuotaForModel(json.RawMessage(`{"subscription_type":"max","rate_limits_available":true,"rate_limits":{"five_hour":{"utilization":10,"resets_at":"2026-09-13T12:00:00Z"},"model_scoped":[{"display_name":"Fable","utilization":100,"resets_at":"2026-09-13T12:00:00Z"},{"display_name":"future","utilization":101,"resets_at":"bad"}]}}`), now, claudeModelFable)
	if err != nil || !modelExhaustion.Subscription || !modelExhaustion.Known || modelExhaustion.Eligible || canLaunchExplicit(modelExhaustion) {
		t.Fatalf("fresh model exhaustion was lost beside malformed unknown sibling: %+v, %v", modelExhaustion, err)
	}
}

func TestProbeClaudeHandshakeAndNoUserFrame(t *testing.T) {
	root := tempDataHome(t)
	profile, bin := filepath.Join(root, "profile"), filepath.Join(root, "bin")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"set -e\n" +
		"[ \"$#\" -eq 10 ] && [ \"$1\" = \"--print\" ] && [ \"$6\" = \"--verbose\" ]\n" +
		"[ \"$7\" = \"--safe-mode\" ] && [ \"$8\" = \"--no-session-persistence\" ]\n" +
		"[ \"$9\" = \"--tools\" ]\n" +
		"shift 9\n" +
		"[ -z \"$1\" ]\n" +
		"case \"$CLAUDE_CONFIG_DIR\" in /*) ;; *) exit 91 ;; esac\n" +
		"[ \"$PWD\" = \"$CLAUDE_CONFIG_DIR\" ]\n" +
		"[ -z \"$ANTHROPIC_API_KEY\" ]\n" +
		"[ \"$DO_NOT_TRACK\" = 1 ] && [ -z \"$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC\" ]\n" +
		"[ \"$DISABLE_AUTOUPDATER\" = 1 ] && [ \"$DISABLE_ERROR_REPORTING\" = 1 ]\n" +
		"[ \"$DISABLE_BUG_COMMAND\" = 1 ] && [ \"$DISABLE_TELEMETRY\" = 1 ]\n" +
		"IFS= read -r init\n" +
		"printf '%s\\n' \"$init\" >> \"$CLAUDE_CONFIG_DIR/frames\"\n" +
		"case \"$init\" in *'\"subtype\":\"initialize\"'*) ;; *) exit 92 ;; esac\n" +
		"printf '%s\\n' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"wrong\"}}'\n" +
		"printf '%s\\n' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"init-1\"}}'\n" +
		"IFS= read -r usage\n" +
		"printf '%s\\n' \"$usage\" >> \"$CLAUDE_CONFIG_DIR/frames\"\n" +
		"case \"$usage\" in *'\"subtype\":\"get_usage\"'*'\"skip_behaviors\":true'*) ;; *) exit 93 ;; esac\n" +
		"/bin/sleep 60 &\n" +
		"printf '%s\\n' \"$!\" > \"$CLAUDE_CONFIG_DIR/child.pid\"\n" +
		"printf '%s\\n' '{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":\"usage-1\",\"response\":{\"subscription_type\":\"max\",\"rate_limits_available\":true,\"rate_limits\":{\"five_hour\":{\"utilization\":0,\"resets_at\":null},\"seven_day\":{\"utilization\":15,\"resets_at\":\"2099-01-01T00:00:00Z\"},\"model_scoped\":[]}}}}'\n" +
		"if IFS= read -r extra; then printf '%s\\n' \"$extra\" >> \"$CLAUDE_CONFIG_DIR/frames\"; exit 94; fi\n"
	native := filepath.Join(bin, "claude")
	if err := os.WriteFile(native, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-test-value")
	t.Setenv("CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "1")
	account := Account{Provider: "claude", Name: "synthetic", NativeDir: profile}
	got, err := probeClaude(account)
	if err != nil || !got.Eligible || !got.Subscription || got.Headroom != 85 {
		t.Fatalf("probe result = %#v, err %v", got, err)
	}
	frames, err := os.ReadFile(filepath.Join(profile, "frames"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(frames)), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "\"subtype\":\"initialize\"") ||
		!strings.Contains(lines[1], "\"subtype\":\"get_usage\"") || !strings.Contains(lines[1], "\"skip_behaviors\":true") {
		t.Fatalf("probe sent unexpected protocol frames: %q", frames)
	}
	if strings.Contains(string(frames), "\"type\":\"user\"") {
		t.Fatalf("probe sent a user frame: %q", frames)
	}
	childBytes, err := os.ReadFile(filepath.Join(profile, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(childBytes)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	running, runningErr := processRunning(pid)
	if runningErr != nil {
		t.Fatal(runningErr)
	}
	for running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		running, runningErr = processRunning(pid)
		if runningErr != nil {
			t.Fatal(runningErr)
		}
	}
	if running {
		t.Fatalf("Claude probe descendant %d survived cleanup", pid)
	}
}

func TestClaudeProbeEnvKeepsUser(t *testing.T) {
	t.Setenv("USER", "synthetic-user")
	t.Setenv("ANTHROPIC_API_KEY", "synthetic-secret")
	t.Setenv("UNRELATED_VAR", "x")
	env := strings.Join(claudeProbeEnv(t.TempDir()), "\n")
	if !strings.Contains(env, "USER=synthetic-user") {
		t.Fatalf("probe env dropped USER: %q", env)
	}
	if strings.Contains(env, "synthetic-secret") || strings.Contains(env, "UNRELATED_VAR") {
		t.Fatalf("probe env leaked non-allowlisted values: %q", env)
	}
}
