package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"

	"github.com/rivo/uniseg"
)

func historyFixture(at time.Time, provider string, used float64) statusReport {
	reset := at.Truncate(24 * time.Hour).Add(24 * time.Hour)
	remaining := 100 - used
	a := statusAccount{Provider: provider, Account: "work", State: "available", Launchable: true, HeadroomPercent: &remaining, Windows: []statusWindow{{Label: "Weekly", UsedPercent: &used, RemainingPercent: &remaining, ResetsAt: &reset}}}
	if provider == "claude" {
		a.ObservedAt = &at
	}
	return statusReport{GeneratedAt: at, Providers: []statusProvider{{Provider: provider, Status: "ok"}}, Accounts: []statusAccount{a}}
}
func historyAdvance(s *watchState, r statusReport, at time.Time) { s.finish(r, nil, at, at) }
func historyText(h *watchHistory) string {
	var b strings.Builder
	for _, e := range h.feed {
		b.WriteString(e.text)
		b.WriteByte('\n')
	}
	return b.String()
}
func historySamples(series watchSeries) int {
	n := 0
	for _, slot := range series.slots[:series.count] {
		if slot.kind == '.' {
			n++
		}
	}
	return n
}
func TestWatchHistoryCachedAndUnchangedQuota(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	r := historyFixture(at, "claude", 25)
	historyAdvance(&s, r, at)
	if len(s.history.feed) != 0 || historySamples(s.history.series[0]) != 1 {
		t.Fatal("first snapshot must establish baseline")
	}
	for i := 1; i < 4; i++ {
		r.GeneratedAt = at.Add(time.Duration(i) * time.Minute)
		r.Accounts[0].NextProbeAt = &r.GeneratedAt
		historyAdvance(&s, r, r.GeneratedAt)
	}
	if historySamples(s.history.series[0]) != 1 || len(s.history.feed) != 0 {
		t.Fatal("cached observation/countdown/probe churn appended", s.history)
	}
	r.Accounts[0].State = "busy"
	r.Accounts[0].Launchable = false
	r.GeneratedAt = at.Add(4 * time.Minute)
	historyAdvance(&s, r, r.GeneratedAt)
	if len(s.history.feed) != 1 || !strings.Contains(historyText(&s.history), "profile-lock contention") || historySamples(s.history.series[0]) != 1 {
		t.Fatal("local lock event must not resample cache")
	}
	later := at.Add(5 * time.Minute)
	r.GeneratedAt = later
	r.Accounts[0].ObservedAt = &later
	historyAdvance(&s, r, later)
	if historySamples(s.history.series[0]) != 2 || len(s.history.feed) != 1 {
		t.Fatal("new unchanged quota observation must sample without a quota event")
	}
	used := 40.0
	r.Accounts[0].Windows[0].UsedPercent = &used
	later = at.Add(6 * time.Minute)
	r.GeneratedAt = later
	r.Accounts[0].ObservedAt = &later
	historyAdvance(&s, r, later)
	if historySamples(s.history.series[0]) != 3 || len(s.history.feed) != 2 || !strings.Contains(historyText(&s.history), "Quota Weekly") {
		t.Fatal(historyText(&s.history))
	}
}
func TestWatchHistoryWatermarksGapsAndSegments(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	r := historyFixture(at, "claude", 50)
	historyAdvance(&s, r, at)
	used := 60.0
	r.Accounts[0].Windows[0].UsedPercent = &used
	r.GeneratedAt = at.Add(time.Minute)
	historyAdvance(&s, r, r.GeneratedAt)
	series := &s.history.series[0]
	if !series.gap || !series.observed.Equal(at) || historySamples(*series) != 1 {
		t.Fatal("same-timestamp conflict replaced watermark/sample")
	}
	count := series.count
	historyAdvance(&s, r, r.GeneratedAt.Add(time.Minute))
	if series.count != count {
		t.Fatal("repeated gaps not coalesced")
	}
	used = 50
	later := at.Add(2 * time.Minute)
	r.GeneratedAt = later
	r.Accounts[0].ObservedAt = &later
	historyAdvance(&s, r, later)
	if series.slots[series.count-2].kind != '|' || historySamples(*series) != 2 || len(s.history.feed) != 0 {
		t.Fatal("gap return must segment/baseline", historyText(&s.history))
	}
	used = 20
	later = at.Add(3 * time.Minute)
	r.GeneratedAt = later
	r.Accounts[0].ObservedAt = &later
	historyAdvance(&s, r, later)
	if series.slots[series.count-2].kind != '|' || len(s.history.feed) != 1 {
		t.Fatal("unexpected decrease must segment and retain real change")
	}
	reset := at.Add(48 * time.Hour)
	r.Accounts[0].Windows[0].ResetsAt = &reset
	later = at.Add(4 * time.Minute)
	r.GeneratedAt = later
	r.Accounts[0].ObservedAt = &later
	historyAdvance(&s, r, later)
	if series.slots[series.count-2].kind != '|' || len(s.history.feed) != 2 {
		t.Fatal("changed reset cycle must segment/event")
	}
	windows := r.Accounts[0].Windows
	r.Accounts[0].Windows = nil
	later = at.Add(5 * time.Minute)
	r.GeneratedAt = later
	historyAdvance(&s, r, later)
	if !series.gap {
		t.Fatal("missing window did not gap")
	}
	r.Accounts[0].Windows = windows
	r.Accounts[0].ObservedAt = &later
	r.GeneratedAt = later.Add(time.Minute)
	historyAdvance(&s, r, r.GeneratedAt)
	if series.gap || series.slots[series.count-2].kind != '|' || len(s.history.feed) != 2 {
		t.Fatal("missing-return baseline/segment")
	}
	old := at
	r.Accounts[0].ObservedAt = &old
	r.GeneratedAt = at.Add(7 * time.Minute)
	historyAdvance(&s, r, r.GeneratedAt)
	if !series.gap || !series.observed.Equal(later) {
		t.Fatal("nonmonotonic observation changed watermark")
	}
}
func TestWatchHistoryQualificationFailuresAndRecovery(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	for _, bad := range []string{"nan", "infinite", "negative", "over100", "missing", "unavailable", "stale", "future", "passed", "report-stale", "provider"} {
		t.Run(bad, func(t *testing.T) {
			var s watchState
			r := historyFixture(at, "codex", 50)
			historyAdvance(&s, r, at)
			later := at.Add(time.Minute)
			r.GeneratedAt = later
			switch bad {
			case "nan":
				x := math.NaN()
				r.Accounts[0].Windows[0].UsedPercent = &x
			case "infinite":
				x := math.Inf(1)
				r.Accounts[0].Windows[0].UsedPercent = &x
			case "negative":
				x := -1.0
				r.Accounts[0].Windows[0].UsedPercent = &x
			case "over100":
				x := 101.0
				r.Accounts[0].Windows[0].UsedPercent = &x
			case "missing":
				r.Accounts[0].Windows[0].UsedPercent = nil
			case "unavailable":
				r.Accounts[0].Windows[0].Unavailable = "missing evidence"
			case "stale":
				r.Accounts[0].Provider = "claude"
				r.Providers[0].Provider = "claude"
				old := at.Add(-10 * time.Minute)
				r.Accounts[0].ObservedAt = &old
			case "future":
				r.GeneratedAt = later.Add(time.Minute)
			case "passed":
				r.Accounts[0].Windows[0].ResetsAt = &later
			case "report-stale":
				r.GeneratedAt = at
				later = at.Add(5 * time.Minute)
			case "provider":
				r.Providers[0].Status = "unavailable"
			}
			historyAdvance(&s, r, later)
			if !s.history.series[0].gap || historySamples(s.history.series[0]) != 1 {
				t.Fatal("unusable evidence sampled", s.history.series[0])
			}
		})
	}
	var s watchState
	r := historyFixture(at, "codex", 50)
	historyAdvance(&s, r, at)
	s.begin(at.Add(5*time.Minute), at.Add(5*time.Minute))
	s.checkCleanup(at.Add(7*time.Minute), context.DeadlineExceeded)
	if len(s.history.feed) != 1 || !s.history.failed || !s.history.series[0].gap {
		t.Fatal("deadline transition missing")
	}
	s.finish(statusReport{}, context.DeadlineExceeded, at.Add(7*time.Minute+time.Second), at.Add(7*time.Minute+time.Second))
	if len(s.history.feed) != 1 {
		t.Fatal("deadline completion duplicated failure")
	}
	later := at.Add(8 * time.Minute)
	r = historyFixture(later, "codex", 60)
	historyAdvance(&s, r, later)
	if len(s.history.feed) != 2 || !strings.Contains(historyText(&s.history), "recovered") || !strings.Contains(historyText(&s.history), "failed") {
		t.Fatal("whole recovery emitted quota/removal", historyText(&s.history))
	}
	// A collector that never completes still records its failure before disabling.
	var stuck watchState
	historyAdvance(&stuck, historyFixture(at, "codex", 50), at)
	stuck.begin(at.Add(5*time.Minute), at.Add(5*time.Minute))
	stuck.checkCleanup(at.Add(7*time.Minute), context.DeadlineExceeded)
	stuck.checkCleanup(at.Add(7*time.Minute+watchCleanupGrace), context.DeadlineExceeded)
	if !stuck.disabled || len(stuck.history.feed) != 1 {
		t.Fatal("unreturned cleanup lost failure")
	}
	stuck.finish(r, nil, later, later)
	if len(stuck.history.feed) != 1 {
		t.Fatal("disabled completion altered feed")
	}
}
func TestWatchHistoryClockAndProviderTransitions(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	r := historyFixture(at, "codex", 50)
	historyAdvance(&s, r, at)
	later := at.Add(time.Minute)
	r.GeneratedAt = later.Add(time.Hour)
	s.finish(r, nil, later, r.GeneratedAt)
	if !s.history.clock || !s.history.series[0].gap || len(s.history.feed) != 0 {
		t.Fatal("clock jump manufactured samples/diffs")
	}
	if !strings.Contains(paintLines(s.view.historyLines(&s, false, later, r.GeneratedAt), false), "clock unusable") {
		t.Fatal("clock uncertainty hidden")
	}
	var p watchState
	r = historyFixture(at, "codex", 50)
	historyAdvance(&p, r, at)
	later = at.Add(time.Minute)
	r.GeneratedAt = later
	r.Providers[0].Status = "unavailable"
	r.Accounts = nil
	historyAdvance(&p, r, later)
	later = at.Add(2 * time.Minute)
	r.GeneratedAt = later
	r.Providers[0].Status = "error"
	historyAdvance(&p, r, later)
	if len(p.history.feed) != 1 || strings.Contains(historyText(&p.history), "removed") {
		t.Fatal("provider error churn/absence manufactured events", historyText(&p.history))
	}
	later = at.Add(3 * time.Minute)
	r = historyFixture(later, "codex", 50)
	historyAdvance(&p, r, later)
	if len(p.history.feed) != 2 || !strings.Contains(historyText(&p.history), "Provider recovered") {
		t.Fatal(historyText(&p.history))
	}
}
func TestWatchHistoryCompleteBaselinesAndLoginBands(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	r := historyFixture(at, "claude", 50)
	expiry := at.Add(4 * 24 * time.Hour)
	r.Accounts[0].LoginExpiresAt = &expiry
	historyAdvance(&s, r, at)
	later := at.Add(time.Minute)
	r.GeneratedAt = later
	extra := r.Accounts[0]
	extra.Account = "new"
	r.Accounts = append(r.Accounts, extra)
	expiry = at.Add(2 * 24 * time.Hour)
	r.Accounts[0].LoginExpiresAt = &expiry
	historyAdvance(&s, r, later)
	if len(s.history.feed) != 2 || !strings.Contains(historyText(&s.history), "Account added") || !strings.Contains(historyText(&s.history), "Login warning band") {
		t.Fatal(historyText(&s.history))
	}
	later = at.Add(2 * time.Minute)
	r.GeneratedAt = later
	r.Accounts = r.Accounts[:1]
	historyAdvance(&s, r, later)
	if !strings.Contains(historyText(&s.history), "Account removed") {
		t.Fatal(historyText(&s.history))
	}
	count := len(s.history.feed)
	r.Accounts = append(r.Accounts, extra)
	later = at.Add(3 * time.Minute)
	r.GeneratedAt = later
	historyAdvance(&s, r, later)
	if len(s.history.feed) != count {
		t.Fatal("missing-return must establish baseline")
	}
	// Unknown snapshots cannot prove removal or an addition on their next return.
	r.Accounts = r.Accounts[:1]
	r.Accounts[0].State = "unknown"
	later = at.Add(4 * time.Minute)
	r.GeneratedAt = later
	historyAdvance(&s, r, later)
	count = len(s.history.feed)
	r.Accounts[0].State = "available"
	extra.Account = "after-unknown"
	r.Accounts = append(r.Accounts, extra)
	later = at.Add(5 * time.Minute)
	r.GeneratedAt = later
	historyAdvance(&s, r, later)
	if strings.Count(historyText(&s.history), "Account added") != 1 || strings.Count(historyText(&s.history), "Account removed") != 1 || len(s.history.feed) != count+1 {
		t.Fatal("incomplete baseline manufactured absence", historyText(&s.history))
	}
}
func TestWatchHistoryCapsOmissionsAndEviction(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	r := historyFixture(at, "codex", 50)
	prototype := r.Accounts[0]
	r.Accounts = nil
	for i := 0; i < 140; i++ {
		a := prototype
		a.Account = fmt.Sprintf("account-%03d", i)
		r.Accounts = append(r.Accounts, a)
	}
	historyAdvance(&s, r, at)
	if len(s.history.series) != 128 || len(s.history.summaries) != 128 || cap(s.history.series) != 128 || cap(s.history.summaries) != 128 || s.history.complete || !s.history.omittedBaseline || len(s.history.feed) != 0 {
		t.Fatal("caps/incomplete baseline", s.history)
	}
	if s.history.seriesAt(watchSeriesKey{watchID(r.Accounts[0]), "Weekly"}) >= 0 || s.history.seriesAt(watchSeriesKey{watchID(r.Accounts[139]), "Weekly"}) < 0 {
		t.Fatal("oldest-updated eviction wrong")
	}
	for i := 0; i < 160; i++ {
		later := at.Add(time.Duration(i+1) * time.Second)
		r = historyFixture(later, "codex", float64(i%100))
		r.Accounts[0].Account = "bounded"
		historyAdvance(&s, r, later)
	}
	if len(s.history.feed) != 128 || cap(s.history.feed) != 128 || s.history.evictedFeed == 0 || !strings.Contains(paintLines(s.view.historyLines(&s, true, s.history.now, s.history.wall), false), "baseline-only") {
		t.Fatal("feed caps/omission indication")
	}
	i := s.history.seriesAt(watchSeriesKey{watchAccountID{"codex", "bounded"}, "Weekly"})
	if s.history.series[i].count != 72 || historySamples(s.history.series[i]) > 72 {
		t.Fatal("slot cap")
	}
	if strings.Contains(historyText(&s.history), "Account added") {
		t.Fatal("evicted/omitted return falsely added")
	}
	var untracked watchState
	r = historyFixture(at, "codex", 50)
	huge := strings.Repeat("x", 1<<20)
	r.Accounts[0].Account = huge
	historyAdvance(&untracked, r, at)
	if len(untracked.history.series) != 0 || untracked.history.omitted == 0 {
		t.Fatal("oversized raw identity retained")
	}
	r = historyFixture(at.Add(time.Minute), "codex", 50)
	r.Accounts[0].Windows = append(r.Accounts[0].Windows, r.Accounts[0].Windows[0])
	historyAdvance(&untracked, r, r.GeneratedAt)
	if len(untracked.history.series) != 0 || untracked.history.omitted != 2 {
		t.Fatal("duplicate labels tracked")
	}
	r.Accounts[0].Windows = r.Accounts[0].Windows[:1]
	r.Accounts[0].Windows[0].Label = huge
	r.GeneratedAt = at.Add(2 * time.Minute)
	historyAdvance(&untracked, r, r.GeneratedAt)
	if len(untracked.history.series) != 0 {
		t.Fatal("oversized window retained")
	}
	// UTF-8 caps preserve whole graphemes, and Clone prevents a slice retaining huge input backing.
	text := strings.Repeat("e\u0301", 200) + huge
	bounded := watchBoundText(text)
	if len(bounded) > 512 || !utf8.ValidString(bounded) || !strings.HasSuffix(bounded, "…") || strings.HasSuffix(strings.TrimSuffix(bounded, "…"), "e") {
		t.Fatal("grapheme/display cap", len(bounded))
	}
	untracked.history.add(watchAccountID{}, text, at, time.Time{})
	if len(untracked.history.feed[0].text) > 512 {
		t.Fatal("retained display text unbounded")
	}
	// Bounded slices of huge source strings must be cloned before retention.
	var backing watchState
	r = historyFixture(at, "codex", 50)
	r.Accounts[0].Account = huge[:256]
	r.Accounts[0].Windows[0].Label = huge[:256]
	historyAdvance(&backing, r, at)
	start := uintptr(unsafe.Pointer(unsafe.StringData(huge)))
	end := start + uintptr(len(huge))
	retained := []string{backing.history.series[0].key.id.account, backing.history.series[0].key.window, backing.history.summaries[0].id.account, watchBoundText(huge[:500])}
	for _, text := range retained {
		pointer := uintptr(unsafe.Pointer(unsafe.StringData(text)))
		if pointer >= start && pointer < end {
			t.Fatal("bounded text retained huge backing string")
		}
	}
	// Duplicate raw accounts and oversized providers are also not aliased into tracked identities.
	var ambiguous watchState
	r = historyFixture(at, "codex", 50)
	r.Accounts = append(r.Accounts, r.Accounts[0])
	historyAdvance(&ambiguous, r, at)
	if len(ambiguous.history.series) != 0 || ambiguous.history.complete {
		t.Fatal("duplicate account identity tracked")
	}
}
func TestWatchHistoryViewsDoNotMutateOrProbe(t *testing.T) {
	s, at, wall := viewState()
	before := s.history
	before.series = append([]watchSeries(nil), before.series...)
	before.summaries = append([]watchSemantic(nil), before.summaries...)
	before.feed = append([]watchChange{}, before.feed...)
	report := s.snapshot.report
	next := s.nextAccount("codex", at, wall)
	var table watchViewport
	for _, key := range []string{"text:g", "text:c", "text:t", "text:a", "text:g", "text:g", "text:p", "text:f"} {
		s.view.handle(key, &s, &table, 40, 10, at, wall)
		for i := 0; i < 10; i++ {
			watchFrame(&s, &table, 40, 10, at.Add(time.Duration(i)*time.Minute), wall.Add(time.Duration(i)*time.Minute), false)
		}
	}
	if !reflect.DeepEqual(before, s.history) || !reflect.DeepEqual(report, s.snapshot.report) || next != s.nextAccount("codex", at, wall) {
		t.Fatal("redraw/filter mutated retained history/report/selector")
	}
	s.view.editing = true
	s.view.draft = ""
	s.view.handle("text:g", &s, &table, 40, 10, at, wall)
	s.view.handle("text:c", &s, &table, 40, 10, at, wall)
	if s.view.draft != "gc" {
		t.Fatal("view keys are not literal in entry")
	}
}
func TestWatchHistoryNarrowProseAndCaptures(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	for i, used := range []float64{12, 20, 20, 48, 8, 15} {
		later := at.Add(time.Duration(i) * time.Minute)
		r := historyFixture(later, "codex", used)
		if i == 3 {
			r.Accounts[0].State = "busy"
			r.Accounts[0].Launchable = false
		}
		if i >= 4 {
			reset := later.Add(48 * time.Hour)
			r.Accounts[0].Windows[0].ResetsAt = &reset
		}
		historyAdvance(&s, r, later)
		if i == 2 {
			s.finish(statusReport{}, context.DeadlineExceeded, later.Add(10*time.Second), later.Add(10*time.Second))
		}
	}
	now := at.Add(5 * time.Minute)
	if historySamples(s.history.series[0]) != 6 || !strings.Contains(historyText(&s.history), "Quota Weekly") {
		t.Fatal("captures must use real history progression")
	}
	ansi := regexp.MustCompile(`\x1b\[[0-9;]*m`)
	for _, body := range []string{"sparklines", "feed", "", "timeline"} {
		for _, size := range [][2]int{{180, 40}, {40, 18}} {
			s.view.body = body
			s.view.detail = false
			s.view.detailViewport = watchViewport{}
			var table watchViewport
			plain := watchFrame(&s, &table, size[0], size[1], now, now, false)
			s.view.detailViewport = watchViewport{}
			table = watchViewport{}
			colored := watchFrame(&s, &table, size[0], size[1], now, now, true)
			if ansi.ReplaceAllString(colored, "") != plain {
				t.Fatal("color changes frame text")
			}
			if size[0] == 40 {
				parts := regexp.MustCompile(`\x1b\[\d+;1H\x1b\[2K`).Split(plain, -1)
				for _, line := range parts {
					if uniseg.StringWidth(line) > 39 {
						t.Fatal("reserved column violated")
					}
				}
				if !strings.Contains(plain, "…") {
					t.Fatal("clipped narrow prose silently truncated")
				}
			}
			if dir := os.Getenv("CODATOR_RICHER_S3_CAPTURE_DIR"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				mode := body
				if mode == "" {
					mode = "tables"
				}
				width := "wide"
				if size[0] == 40 {
					width = "narrow"
				}
				for label, data := range map[string]string{"plain": plain, "ansi": colored} {
					name := filepath.Join(dir, "richer-s3-"+mode+"-"+width+"-"+label+".txt")
					if err := os.WriteFile(name, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	// Timestamp-bearing table prose clips explicitly, with its complete tail reachable.
	line := cell{{"Last successful completion: " + watchStamp(at), ""}}
	clipped := watchDetailClip(line, 0, 39).paint(false)
	if !strings.HasSuffix(clipped, "…") {
		t.Fatal("partial UTC timestamp lacks ellipsis", clipped)
	}
	tail := watchDetailClip(line, watchLineWidth(line)-37, 39).paint(false)
	if !strings.HasSuffix(tail, "Z") || !strings.HasPrefix(tail, "…") {
		t.Fatal("timestamp tail not reachable", tail)
	}
}

func TestWatchHistoryRawIdentityMissingReturnsAndUnknownReset(t *testing.T) {
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	var s watchState
	r := historyFixture(at, "codex", 25)
	a := r.Accounts[0]
	a.Account = "same\n"
	a.Windows[0].ResetsAt = nil
	b := a
	b.Account = "same\r"
	c := a
	c.Provider = "claude"
	c.ObservedAt = &at
	r.Providers = append(r.Providers, statusProvider{Provider: "claude", Status: "ok"})
	r.Accounts = []statusAccount{a, b, c}
	historyAdvance(&s, r, at)
	if len(s.history.series) != 3 {
		t.Fatal("raw identity/sanitized collision aliased histories")
	}
	if !strings.Contains(paintLines(s.view.historyLines(&s, false, at, at), false), "reset unknown") {
		t.Fatal("nil reset not labeled unknown")
	}
	key := watchSeriesKey{watchID(a), "Weekly"}
	i := s.history.seriesAt(key)
	r.Accounts = []statusAccount{b, c}
	later := at.Add(time.Minute)
	r.GeneratedAt = later
	historyAdvance(&s, r, later)
	if !s.history.series[i].gap || !s.history.series[i].observed.Equal(at) {
		t.Fatal("missing account did not preserve/gap watermark")
	}
	r.Accounts = append(r.Accounts, a)
	later = at.Add(2 * time.Minute)
	r.GeneratedAt = later
	historyAdvance(&s, r, later)
	series := s.history.series[i]
	if historySamples(series) != 2 || series.slots[series.count-2].kind != '|' {
		t.Fatal("missing account return did not start segment")
	}
	if strings.Contains(historyText(&s.history), "Account added") || strings.Contains(historyText(&s.history), "Quota Weekly") {
		t.Fatal("return compared missing evidence", historyText(&s.history))
	}
	before := s.history
	// A provider filter hides only display, including distinct raw providers with equal labels.
	s.view.providerFilter = 2
	s.view.rebuild(&s, later, later)
	text := paintLines(s.view.historyLines(&s, false, later, later), false)
	if strings.Contains(text, "codex/same") || !strings.Contains(text, "claude/same") || !reflect.DeepEqual(before, s.history) {
		t.Fatal("display provider filter altered history", text)
	}
}

func TestWatchHistoryHeaderFooterAndBodyEllipsis(t *testing.T) {
	s, now, wall := viewState()
	s.errorText = "status collection failed; retained data"
	frame := watchFrame(&s, &watchViewport{}, 40, 18, now, wall, false)
	lines := regexp.MustCompile(`\x1b\[\d+;1H\x1b\[2K`).Split(frame, -1)[1:]
	if len(lines) != 18 || !strings.HasSuffix(lines[2], "…") || !strings.HasSuffix(lines[16], "…") || !strings.HasSuffix(lines[17], "…") {
		t.Fatal("narrow header/footer hard-clipped", lines)
	}
	s.errorText = ""
	// Scroll from the summary into timestamp-bearing body prose and reach its UTC tail.
	table := watchViewport{y: 2}
	s.view.reveal = false
	frame = watchFrame(&s, &table, 40, 18, now, wall, false)
	if !strings.Contains(frame, "…") {
		t.Fatal("body prose missing ellipsis")
	}
	line := cell{{"Source generated (collection start): " + watchStamp(wall), ansiDim}}
	full := watchDetailClip(line, 0, 200)
	if full.paint(true) != line.paint(true) {
		t.Fatal("unclipped prose bytes changed")
	}
	clipped := watchDetailClip(line, 0, 39).paint(false)
	if !strings.HasSuffix(clipped, "…") {
		t.Fatal("timestamp hard clipped", clipped)
	}
	v := watchViewport{x: 1000}
	v.clamp([]cell{line}, 37, 1)
	tail := watchDetailClip(line, v.x, 39).paint(false)
	if !strings.Contains(tail, watchStamp(wall)) {
		t.Fatal("full timestamp no longer reachable", tail)
	}
}
