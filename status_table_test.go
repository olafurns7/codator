package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;]*m")

func statusPanelText(now time.Time, color bool, rows ...statusAccount) string {
	var out strings.Builder
	renderStatusPanel(&out, statusProvider{Provider: rows[0].Provider, Status: "ok"}, rows, color, now)
	return out.String()
}

func codexStatusRow(name string, q quota, now time.Time) statusAccount {
	return quotaStatusAccount("codex", name, q, nil, now, statusDetails{Windows: codexStatusWindows(q.Windows)})
}

func assertStatusPanelAligned(t *testing.T, panel string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(panel, "\n"), "\n")
	for _, line := range lines {
		if got, want := utf8.RuneCountInString(line), utf8.RuneCountInString(lines[0]); got != want {
			t.Fatalf("line %q is %d runes wide, want %d:\n%s", line, got, want, panel)
		}
	}
	last := len(lines) - 1
	if !strings.HasPrefix(lines[0], "╭─ ") || !strings.HasSuffix(lines[0], "╮") || !strings.HasPrefix(lines[last], "╰") || !strings.HasSuffix(lines[last], "╯") {
		t.Fatalf("panel corners are wrong:\n%s", panel)
	}
	for _, line := range lines[1:last] {
		if !strings.HasPrefix(line, "│") || !strings.HasSuffix(line, "│") {
			t.Fatalf("panel side is wrong in %q:\n%s", line, panel)
		}
	}
}

func TestStatusPanelCodexLayout(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	reset := now.Add(6*24*time.Hour + 21*time.Hour)
	rows := []statusAccount{
		codexStatusRow("codex1", quota{Known: true, Eligible: true, Headroom: 100, Email: "a@example.com", Credits: "41366.80",
			Windows: []usageWindow{{Label: "Weekly limit", Used: 0, ResetsAt: reset}}}, now),
		codexStatusRow("codex2", quota{Known: true, Eligible: true, Headroom: 85, Email: "bb@example.com", Credits: "2974.40",
			Windows: []usageWindow{{Label: "Weekly limit", Used: 15, ResetsAt: reset}}}, now),
		codexStatusRow("codex3", quota{Known: true, Eligible: true, Headroom: 53, Credits: "unlimited", ResetCredits: 1,
			Windows: []usageWindow{{Label: "Weekly limit", Used: 47, ResetsAt: reset}}}, now),
	}
	got := statusPanelText(now, false, rows...)
	want := "" +
		"╭─ codex ──────────────────────────────────────────────────────────╮\n" +
		"│ ACCOUNT   EMAIL           WEEKLY                    CREDITS  RST │\n" +
		"│ ● codex1  a@example.com   ██████████ 100% 6d 21h   41366.80      │\n" +
		"│ ● codex2  bb@example.com  ████████▌░  85% 6d 21h    2974.40      │\n" +
		"│ ● codex3                  █████▎░░░░  53% 6d 21h  unlimited    1 │\n" +
		"╰──────────────────────────────────────────────────────────────────╯\n"
	if got != want {
		t.Fatalf("panel:\n%s\nwant:\n%s", got, want)
	}
	assertStatusPanelAligned(t, got)
}

func TestStatusPanelOmitsEmptyColumns(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	got := statusPanelText(now, false, codexStatusRow("work", quota{Known: true, Eligible: true, Headroom: 25}, now))
	if strings.Contains(got, "EMAIL") || strings.Contains(got, "CREDITS") || strings.Contains(got, "RST") {
		t.Fatalf("empty columns were drawn:\n%s", got)
	}
	assertStatusPanelAligned(t, got)
}

func TestStatusWindowHeaders(t *testing.T) {
	for label, want := range map[string]string{
		"Weekly limit":                       "WEEKLY",
		"Weekly (all models)":                "WEEKLY",
		"Current session":                    "SESSION",
		"5-hour limit":                       "5H",
		"Weekly (Fable)":                     "FABLE",
		"Weekly (Opus #2)":                   "OPUS #2",
		"Weekly (OAuth apps)":                "OAUTH APPS",
		"GPT-5.3-Codex-Spark 5-hour limit":   "GPT-5.3-CODEX-SPARK 5-HOUR LIMIT",
		"Weekly (other reported model 1)":    "OTHER REPORTED MODEL 1",
		"2-day limit":                        "2-DAY LIMIT",
		"Weekly (Sonnet) and something else": "WEEKLY (SONNET) AND SOMETHING ELSE",
	} {
		if got := statusWindowHeader(label); got != want {
			t.Errorf("header for %q = %q, want %q", label, got, want)
		}
	}
}

func TestStatusBar(t *testing.T) {
	for remaining, want := range map[float64]string{
		0:   "░░░░░░░░░░",
		47:  "████▋░░░░░",
		85:  "████████▌░",
		100: "██████████",
	} {
		if got := statusBar(remaining); got != want {
			t.Errorf("bar(%v) = %q, want %q", remaining, got, want)
		}
	}
}

func TestStatusAccountDots(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	remaining := 60.0
	window := []statusWindow{{Label: "Weekly limit", RemainingPercent: &remaining}}
	for _, test := range []struct {
		row                     statusAccount
		plain, colored, related string
	}{
		{statusAccount{State: "available", Windows: window}, "● work", "\x1b[32m●\x1b[0m work", ""},
		{statusAccount{State: "available", OnCredits: true, Windows: window}, "● work", "\x1b[33m●\x1b[0m work", "│   on credits"},
		{statusAccount{State: "available", OnCredits: true}, "● work", "\x1b[33m●\x1b[0m work", "│ ● work   on credits │"},
		{statusAccount{State: "exhausted", Windows: window}, "○ work", "\x1b[31m○\x1b[0m work", ""},
		{statusAccount{State: "busy"}, "◐ work", "\x1b[33m◐\x1b[0m work", "│ ◐ work   busy │"},
		{statusAccount{State: "unknown", Windows: window}, "○ work", "\x1b[2m○\x1b[0m work", "│ ○ work   ██████░░░░  60% │\n│   unknown                │"},
		{statusAccount{State: "unknown"}, "○ work", "\x1b[2m○\x1b[0m work", "│ ○ work   unknown │"},
		{statusAccount{State: "ineligible"}, "○ work", "\x1b[2m○\x1b[0m work", "│ ○ work   skipped │"},
	} {
		test.row.Provider, test.row.Account = "codex", "work"
		if got := statusAccountCell(test.row).paint(false); got != test.plain {
			t.Errorf("plain account %+v = %q, want %q", test.row, got, test.plain)
		}
		if got := statusAccountCell(test.row).paint(true); got != test.colored {
			t.Errorf("colored account %+v = %q, want %q", test.row, got, test.colored)
		}
		got := statusPanelText(now, false, test.row)
		if strings.Contains(got, "STATE") || !strings.Contains(got, test.related) || strings.Contains(got, "—") {
			t.Errorf("panel for %+v lacks %q or still has STATE or dashes:\n%s", test.row, test.related, got)
		}
		assertStatusPanelAligned(t, got)
	}
}

func TestStatusNotesKeepReasonsDenialsAndCooldown(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	observed, until, next := now.Add(-10*time.Minute), now.Add(2*time.Hour), now.Add(3*time.Minute)
	unknown := codexStatusRow("work", quota{Reason: "usage windows are missing, malformed, or stale"}, now)
	claude := statusAccount{
		Provider: "claude", Account: "max", State: "unknown", Reason: "usage is unknown",
		ObservedAt: &observed, NextProbeAt: &next, claudeUnavailable: "snapshot is stale",
		Windows:        []statusWindow{{Label: "Weekly (Fable)", Unavailable: "malformed utilization"}, {Label: "Current session", Unavailable: "not reported"}},
		KnownExhausted: []statusKnownExhausted{{Scope: "Weekly (Fable)", Until: until, ObservedAt: observed}},
	}
	got := statusPanelText(now, false, unknown) + statusPanelText(now, false, claude)
	for _, want := range []string{
		"│ ○ work   unknown",
		"│   usage windows are missing, malformed, or stale",
		"╭─ claude · observed 10m ago ─",
		"│   usage is unknown",
		"│   usage unavailable (snapshot is stale)",
		"│   Weekly (Fable): unavailable (malformed utilization)",
		"│   known exhausted: Weekly (Fable) for 2h 0m (observed 10m ago)",
		"│   probe cooldown for 3m",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "not reported") {
		t.Errorf("a not-reported window got a note:\n%s", got)
	}
	claude.Reason = claudeRetryPrefix + "2026-09-25T12:03:00Z"
	if got := statusPanelText(now, false, claude); strings.Contains(got, "probe cooldown") || !strings.Contains(got, claude.Reason) {
		t.Errorf("cooldown was repeated or the retry reason was lost:\n%s", got)
	}
	for _, panel := range strings.SplitAfter(got, "╯\n") {
		if panel != "" {
			assertStatusPanelAligned(t, panel)
		}
	}
}

func TestStatusProviderPanels(t *testing.T) {
	var out strings.Builder
	renderStatusPanel(&out, statusProvider{Provider: "codex", Status: "no_accounts"}, nil, false, time.Now())
	renderStatusPanel(&out, statusProvider{Provider: "x", Status: "unavailable", Error: "unknown provider"}, nil, false, time.Now())
	want := "" +
		"╭─ codex ──────────────╮\n" +
		"│ no accounts enrolled │\n" +
		"╰──────────────────────╯\n" +
		"╭─ x ────────────────────────────╮\n" +
		"│ unavailable (unknown provider) │\n" +
		"╰────────────────────────────────╯\n"
	if got := out.String(); got != want {
		t.Fatalf("provider panels:\n%s\nwant:\n%s", got, want)
	}
}

func TestStatusColor(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	row := codexStatusRow("work", quota{Known: true, Eligible: true, Headroom: 10, Windows: []usageWindow{{Label: "Weekly limit", Used: 90}}}, now)
	plain := statusPanelText(now, false, row)
	if strings.Contains(plain, "\x1b") {
		t.Fatalf("plain output has escapes: %q", plain)
	}
	colored := statusPanelText(now, true, row)
	for _, want := range []string{"\x1b[31m█░░░░░░░░░  10%\x1b[0m", "\x1b[32m●\x1b[0m work", "\x1b[1mACCOUNT\x1b[0m", "\x1b[2m│\x1b[0m"} {
		if !strings.Contains(colored, want) {
			t.Errorf("colored output lacks %q: %q", want, colored)
		}
	}
	if stripped := ansiPattern.ReplaceAllString(colored, ""); stripped != plain {
		t.Fatalf("color changed the layout:\n%s\nplain:\n%s", stripped, plain)
	}
	// /dev/null is a character device, so it stands in for a terminal.
	device, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer device.Close()
	for _, test := range []struct {
		noColor, term string
		want          bool
	}{{"", "xterm", true}, {"1", "xterm", false}, {"", "dumb", false}} {
		t.Setenv("NO_COLOR", test.noColor)
		t.Setenv("TERM", test.term)
		if got := statusColor(device); got != test.want {
			t.Errorf("NO_COLOR=%q TERM=%q color=%t, want %t", test.noColor, test.term, got, test.want)
		}
	}
	if statusColor(&strings.Builder{}) {
		t.Fatal("a non-file writer was colored")
	}
}

func TestStatusDuplicateWindowLabelsKeepEveryWindow(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	q := quota{Known: true, Eligible: true, Headroom: 20, Windows: []usageWindow{
		{Label: "Usage limit", Used: 20, ResetsAt: now.Add(5 * time.Hour)},
		{Label: "Usage limit", Used: 80, ResetsAt: now.Add(48 * time.Hour)},
	}}
	got := statusPanelText(now, false, codexStatusRow("work", q, now))
	for _, want := range []string{
		"│ ACCOUNT  USAGE LIMIT                          │",
		"│ ● work   ████████░░  80% 5h 0m                │",
		"│   USAGE LIMIT: 20% remaining, resets in 2d 0h │",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	assertStatusPanelAligned(t, got)
}

func TestStatusExhaustedRowsKeepReasons(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	codex := codexStatusRow("work", quota{Known: true, Reason: "spend control is reached",
		Windows: []usageWindow{{Label: "Weekly limit", Used: 100, ResetsAt: now.Add(24 * time.Hour)}}}, now)
	if codex.State != "exhausted" {
		t.Fatalf("codex state = %q", codex.State)
	}
	got := statusPanelText(now, false, codex)
	if !strings.Contains(got, "│ ○ work   ░░░░░░░░░░   0% 1d 0h │") || !strings.Contains(got, "│   spend control is reached") {
		t.Errorf("exhausted Codex row lost its reason:\n%s", got)
	}

	observed, until, next := now.Add(-time.Minute), now.Add(24*time.Hour), now.Add(4*time.Minute)
	used, remaining := 10.0, 90.0
	claude := statusAccount{
		Provider: "claude", Account: "max", State: "exhausted", AvailableAt: &until, ObservedAt: &observed, NextProbeAt: &next,
		Reason:         "all applicable usage headroom is exhausted; " + claudeRetryPrefix + next.Format(time.RFC3339),
		Windows:        []statusWindow{{Label: "Weekly (all models)", UsedPercent: &used, RemainingPercent: &remaining, ResetsAt: &until}},
		KnownExhausted: []statusKnownExhausted{{Scope: "Weekly (Fable)", Until: until, ObservedAt: observed}},
	}
	got = statusPanelText(now, false, claude)
	for _, want := range []string{"│ ○ max    █████████░  90% 1d 0h ", "│   known exhausted: Weekly (Fable) for 1d 0h (observed 1m ago)"} {
		if !strings.Contains(got, want) {
			t.Errorf("exhausted Claude row lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "headroom") || strings.Contains(got, claudeRetryPrefix) {
		t.Errorf("exhausted Claude row kept the generic reason:\n%s", got)
	}
	// With the generic reason gone, an unavailable snapshot shows its cooldown instead.
	claude.claudeUnavailable = "snapshot is stale"
	if got = statusPanelText(now, false, claude); !strings.Contains(got, "│   probe cooldown for 4m") || strings.Contains(got, "headroom") {
		t.Errorf("cooldown was dropped or the generic reason shown:\n%s", got)
	}
	assertStatusPanelAligned(t, got)
}
