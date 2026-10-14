package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
)

const watchHistoryCap = 128
const watchSlots = 72

type watchSeriesKey struct {
	id     watchAccountID
	window string
}
type watchObservation struct {
	kind   byte
	used   float64
	source time.Time
}
type watchSeries struct {
	key             watchSeriesKey
	slots           [watchSlots]watchObservation
	count           int
	observed, reset time.Time
	used            float64
	watermark, gap  bool
	seen, updated   uint64
}
type watchSemantic struct {
	id                                   watchAccountID
	provider                             bool
	state, login                         string
	launchable, valid, present, complete bool
	seen, updated                        uint64
}
type watchChange struct {
	id         watchAccountID
	text       string
	at, source time.Time
	uncertain  bool
}
type watchHistory struct {
	series                                                []watchSeries
	summaries                                             []watchSemantic
	feed                                                  []watchChange
	serial, order                                         uint64
	now, wall                                             time.Time
	started, failed, clock, complete, omittedBaseline     bool
	omitted, evictedSeries, evictedSummaries, evictedFeed uint64
}

// Clone retained strings: byte bounds must also bound their backing storage.
func watchBoundText(text string) string {
	text = watchClean(text)
	g := uniseg.NewGraphemes(text)
	end := 0
	for g.Next() {
		_, next := g.Positions()
		if next > 509 {
			break
		}
		end = next
	}
	if len(text) > 512 {
		return strings.Clone(text[:end]) + "…"
	}
	return strings.Clone(text)
}
func watchHistoryID(id watchAccountID) bool {
	return len(id.provider) <= 256 && len(id.account) <= 256 && utf8.ValidString(id.provider) && utf8.ValidString(id.account)
}
func watchCloneID(id watchAccountID) watchAccountID {
	return watchAccountID{strings.Clone(id.provider), strings.Clone(id.account)}
}
func (h *watchHistory) touch() uint64 { h.order++; return h.order }
func (h *watchHistory) tick(now, wall time.Time) {
	if h.series == nil {
		h.series = make([]watchSeries, 0, watchHistoryCap)
	}
	if h.summaries == nil {
		h.summaries = make([]watchSemantic, 0, watchHistoryCap)
	}
	if h.feed == nil {
		h.feed = make([]watchChange, 0, watchHistoryCap)
	}
	h.clock = false
	if !h.now.IsZero() {
		drift := wall.Sub(h.wall.Add(now.Sub(h.now)))
		h.clock = now.Before(h.now) || drift > 5*time.Second || drift < -5*time.Second
	}
	h.now, h.wall = now, wall
	h.serial++
}
func (h *watchHistory) add(id watchAccountID, text string, wall, source time.Time) {
	entry := watchChange{watchCloneID(id), watchBoundText(text), wall, source, h.clock}
	if len(h.feed) == watchHistoryCap {
		copy(h.feed, h.feed[1:])
		h.feed[len(h.feed)-1] = entry
		h.evictedFeed++
	} else {
		h.feed = append(h.feed, entry)
	}
}
func (s *watchSeries) append(slot watchObservation) {
	if s.count == watchSlots {
		copy(s.slots[:], s.slots[1:])
		s.count--
	}
	s.slots[s.count] = slot
	s.count++
}
func (s *watchSeries) breakLine(serial uint64) {
	if !s.gap {
		s.append(watchObservation{kind: ' '})
		s.updated = serial
	}
	s.gap = true
}
func (h *watchHistory) failure(now, wall time.Time) {
	h.tick(now, wall)
	if !h.failed {
		h.add(watchAccountID{}, "Whole collection failed; retained data, no samples", wall, time.Time{})
	}
	h.failed = true
	for i := range h.series {
		h.series[i].breakLine(h.touch())
	}
	for i := range h.summaries {
		h.summaries[i].valid = false
	}
	h.complete = false
}
func (h *watchHistory) summary(id watchAccountID, provider bool) int {
	for i := range h.summaries {
		if h.summaries[i].id == id && h.summaries[i].provider == provider {
			return i
		}
	}
	return -1
}
func (h *watchHistory) retainSummary(value watchSemantic) {
	value.id = watchCloneID(value.id)
	value.state, value.login = watchBoundText(value.state), watchBoundText(value.login)
	value.seen, value.updated = h.serial, h.touch()
	if i := h.summary(value.id, value.provider); i >= 0 {
		h.summaries[i] = value
		return
	}
	if len(h.summaries) < watchHistoryCap {
		h.summaries = append(h.summaries, value)
		return
	}
	oldest := 0
	for i := range h.summaries {
		if h.summaries[i].updated < h.summaries[oldest].updated {
			oldest = i
		}
	}
	h.summaries[oldest] = value
	h.evictedSummaries++
	h.omittedBaseline = true
}
func (h *watchHistory) seriesAt(key watchSeriesKey) int {
	for i := range h.series {
		if h.series[i].key == key {
			return i
		}
	}
	return -1
}
func (h *watchHistory) retainSeries(key watchSeriesKey) int {
	if i := h.seriesAt(key); i >= 0 {
		return i
	}
	value := watchSeries{key: watchSeriesKey{watchCloneID(key.id), strings.Clone(key.window)}}
	if len(h.series) < watchHistoryCap {
		h.series = append(h.series, value)
		return len(h.series) - 1
	}
	oldest := 0
	for i := range h.series {
		if h.series[i].updated < h.series[oldest].updated {
			oldest = i
		}
	}
	h.series[oldest] = value
	h.evictedSeries++
	return oldest
}
func watchLoginBand(a statusAccount, wall time.Time) string {
	if a.Provider != "claude" || a.LoginExpiresAt == nil {
		return "unknown"
	}
	days := loginDaysLeft(*a.LoginExpiresAt, wall)
	if days <= 0 {
		return "expired"
	}
	if days <= claudeLoginWarnDays {
		return "warning (≤3 days)"
	}
	return "healthy (>3 days)"
}
func watchHistoryReset(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return watchStamp(t)
}
func watchProviderGood(status string) bool { return status == "ok" || status == "no_accounts" }
func watchAccountUnique(r statusReport, id watchAccountID) bool {
	count := 0
	for _, a := range r.Accounts {
		if watchID(a) == id {
			count++
		}
	}
	return count == 1
}
func watchProviderUnique(r statusReport, name string) bool {
	count := 0
	for _, p := range r.Providers {
		if p.Provider == name {
			count++
		}
	}
	return count == 1
}
func watchProviderComplete(r statusReport, p statusProvider) bool {
	if !watchProviderGood(p.Status) || !watchProviderUnique(r, p.Provider) {
		return false
	}
	for _, a := range r.Accounts {
		if a.Provider == p.Provider && (a.State == "unknown" || !watchHistoryID(watchID(a)) || !watchAccountUnique(r, watchID(a))) {
			return false
		}
	}
	return true
}
func (h *watchHistory) observe(s *watchState, now, wall time.Time) {
	h.tick(now, wall)
	if h.failed {
		h.add(watchAccountID{}, "Whole collection recovered; snapshot received", wall, time.Time{})
	}
	h.failed = false
	r := s.snapshot.report
	usable := !h.clock && watchFresh(r.GeneratedAt, wall)
	previousComplete := h.complete
	h.omitted = 0
	h.complete = usable && len(r.Accounts)+len(r.Providers) <= watchHistoryCap
	// Oversized/ambiguous evidence can never establish a complete baseline.
	for _, p := range r.Providers {
		h.complete = h.complete && watchHistoryID(watchAccountID{provider: p.Provider}) && watchProviderComplete(r, p)
	}
	for _, a := range r.Accounts {
		known := false
		for _, p := range r.Providers {
			known = known || p.Provider == a.Provider
		}
		h.complete = h.complete && known && watchHistoryID(watchID(a)) && watchAccountUnique(r, watchID(a))
	}
	if !h.complete {
		h.omittedBaseline = h.omittedBaseline || len(r.Accounts)+len(r.Providers) > watchHistoryCap
	}
	for _, p := range r.Providers {
		id := watchAccountID{provider: p.Provider}
		if !watchHistoryID(id) || !watchProviderUnique(r, p.Provider) {
			h.omitted++
			h.omittedBaseline = true
			continue
		}
		if i := h.summary(id, true); i >= 0 {
			old := h.summaries[i]
			if old.valid && usable && watchProviderGood(old.state) != watchProviderGood(p.Status) {
				text := "Provider failure: " + p.Status
				if watchProviderGood(p.Status) {
					text = "Provider recovered"
				}
				h.add(id, text, wall, time.Time{})
			}
		}
		// Account processing needs the previous provider baseline, before replacing it.
		previousProviderComplete := false
		if i := h.summary(id, true); i >= 0 {
			previousProviderComplete = h.summaries[i].complete && h.summaries[i].valid
		}
		currentProviderComplete := usable && watchProviderComplete(r, p)
		for _, a := range r.Accounts {
			if a.Provider != p.Provider {
				continue
			}
			aid := watchID(a)
			if !watchHistoryID(aid) || !watchAccountUnique(r, aid) {
				h.omitted++
				h.omittedBaseline = true
				continue
			}
			valid := usable && p.Status == "ok"
			value := watchSemantic{id: aid, state: a.State, launchable: a.Launchable, login: watchLoginBand(a, wall), valid: valid, present: true}
			if i := h.summary(aid, false); i >= 0 {
				old := h.summaries[i]
				if old.present && old.valid && valid {
					if old.state != value.state || old.launchable != value.launchable {
						h.add(aid, fmt.Sprintf("State %s → %s; launchable %t → %t (busy = profile-lock contention)", old.state, value.state, old.launchable, value.launchable), wall, time.Time{})
					}
					if old.login != value.login && a.Provider == "claude" && old.login != "unknown" && value.login != "unknown" {
						h.add(aid, "Login warning band "+old.login+" → "+value.login, wall, time.Time{})
					}
				}
			} else if h.started && previousComplete && h.complete && previousProviderComplete && currentProviderComplete && !h.omittedBaseline {
				h.add(aid, "Account added (recorded snapshot)", wall, time.Time{})
			}
			h.retainSummary(value)
			observed := r.GeneratedAt
			if a.Provider == "claude" {
				observed = time.Time{}
				if a.ObservedAt != nil {
					observed = *a.ObservedAt
				}
			}
			for _, w := range a.Windows {
				key := watchSeriesKey{aid, w.Label}
				duplicates := 0
				for _, other := range a.Windows {
					if other.Label == w.Label {
						duplicates++
					}
				}
				if len(w.Label) > 256 || !utf8.ValidString(w.Label) || duplicates != 1 {
					h.omitted++
					continue
				}
				i := h.retainSeries(key)
				series := &h.series[i]
				series.seen, series.updated = h.serial, h.touch()
				reset := time.Time{}
				if w.ResetsAt != nil {
					reset = *w.ResetsAt
				}
				fresh := !h.clock && s.view.fresh(s, a, now, wall) && watchPercent(w.UsedPercent) && w.Unavailable == ""
				if !fresh || series.watermark && observed.Before(series.observed) {
					series.breakLine(h.touch())
					continue
				}
				if series.watermark && observed.Equal(series.observed) {
					if *w.UsedPercent != series.used || !reset.Equal(series.reset) {
						series.breakLine(h.touch())
					}
					continue
				}
				changed := series.watermark && (*w.UsedPercent != series.used || !reset.Equal(series.reset))
				if series.watermark && (series.gap || !reset.Equal(series.reset) || !series.reset.IsZero() && !series.reset.After(wall) || *w.UsedPercent < series.used) {
					series.append(watchObservation{kind: '|'})
				}
				if changed && !series.gap {
					h.add(aid, fmt.Sprintf("Quota %s: used %g%% → %g%%; reset %s → %s", w.Label, series.used, *w.UsedPercent, watchHistoryReset(series.reset), watchHistoryReset(reset)), wall, observed)
				}
				series.append(watchObservation{kind: '.', used: *w.UsedPercent, source: observed})
				series.observed, series.reset, series.used, series.watermark, series.gap = observed, reset, *w.UsedPercent, true, false
			}
		}
		for i := range h.summaries {
			old := &h.summaries[i]
			if !old.provider && old.id.provider == p.Provider && old.seen != h.serial && old.present {
				if previousComplete && h.complete && previousProviderComplete && currentProviderComplete && old.valid {
					h.add(old.id, "Account removed (complete recorded snapshot)", wall, time.Time{})
				}
				old.present, old.valid = false, false
			}
		}
		h.retainSummary(watchSemantic{id: id, provider: true, state: p.Status, valid: usable, present: true, complete: currentProviderComplete})
	}
	for i := range h.series {
		if h.series[i].seen != h.serial {
			h.series[i].breakLine(h.touch())
		}
	}
	// An absent provider is unknown, so it cannot establish account removals.
	for i := range h.summaries {
		if h.summaries[i].seen != h.serial {
			h.summaries[i].valid = false
		}
	}
	h.started = true
}

func (v *watchView) historyMatches(s *watchState, id watchAccountID) bool {
	if id.provider == "" {
		return true
	}
	if s.snapshot == nil {
		return v.providerFilter == 0 && v.stateFilter == 0 && v.name == ""
	}
	r := s.snapshot.report
	if v.providerFilter > 0 && (v.providerFilter > len(r.Providers) || r.Providers[v.providerFilter-1].Provider != id.provider) {
		return false
	}
	if id.account == "" {
		return v.stateFilter == 0 && v.name == ""
	}
	if !strings.Contains(strings.ToLower(id.account), strings.ToLower(v.name)) {
		return false
	}
	if v.stateFilter == 0 {
		return true
	}
	for _, a := range r.Accounts {
		if watchID(a) == id {
			return a.State == watchStateNames[v.stateFilter]
		}
	}
	return false
}
func (v *watchView) historyLines(s *watchState, feed bool, now, wall time.Time) []cell {
	h := &s.history
	title := "Session sparklines — used percent"
	if feed {
		title = "Snapshot change feed"
	}
	lines := []cell{{{title, ansiBold}}, {{"Sampled between polls; intervening activity unknown. Memory ends on quit.", ansiDim}}}
	if feed {
		lines = append(lines, cell{{"Event UTC = completion/error transition; source UTC shown for quota.", ansiDim}})
	} else {
		lines = append(lines, cell{{"▁▂▃▄▅▆▇█ = used 0–100%; space = gap; | = segment. Uneven observations, no interpolation.", ansiDim}}, cell{{"Codex: collection-start UTC; Claude: ObservedAt (cached points deduplicated).", ansiDim}})
	}
	if h.clock || s.failed(now, wall) {
		lines = append(lines, cell{{"Current data/clock unusable; retained observations only.", ansiYellow}})
	}
	lines = append(lines, cell{{fmt.Sprintf("Retained: %d/128 series ×72 slots; %d/128 summaries; %d/128 changes", len(h.series), len(h.summaries), len(h.feed)), ansiDim}}, cell{{fmt.Sprintf("Untracked this snapshot: %d (duplicate/oversized identities); evicted series/summaries/feed: %d/%d/%d", h.omitted, h.evictedSeries, h.evictedSummaries, h.evictedFeed), ansiDim}})
	baseline := "Complete retained snapshot baseline"
	if !h.complete {
		baseline = "Incomplete baseline; missing evidence cannot establish additions/removals"
	}
	if h.omittedBaseline {
		baseline += "; additions baseline-only after retention/identity omissions"
	}
	lines = append(lines, cell{{baseline, ansiDim}})
	shown := 0
	if feed {
		for i := len(h.feed) - 1; i >= 0; i-- {
			e := h.feed[i]
			if !v.historyMatches(s, e.id) {
				continue
			}
			at := watchStamp(e.at)
			if e.uncertain {
				at += " (clock uncertain)"
			}
			lines = append(lines, cell{{at + " | " + watchClean(e.id.provider+"/"+e.id.account), ansiDim}}, cell{{e.text, ""}})
			if !e.source.IsZero() {
				lines = append(lines, cell{{"Source observation UTC: " + watchStamp(e.source), ansiDim}})
			}
			shown++
		}
	} else {
		glyphs := []rune("▁▂▃▄▅▆▇█")
		for _, series := range h.series {
			if !v.historyMatches(s, series.key.id) {
				continue
			}
			var graph strings.Builder
			count := 0
			var first, last time.Time
			for _, slot := range series.slots[:series.count] {
				if slot.kind == '.' {
					graph.WriteRune(glyphs[min(7, int(slot.used*8/100))])
					count++
					if first.IsZero() {
						first = slot.source
					}
					last = slot.source
				} else {
					graph.WriteByte(slot.kind)
				}
			}
			lines = append(lines, cell{{watchClean(series.key.id.provider + "/" + series.key.id.account + " | " + series.key.window), ansiBold}}, cell{{graph.String(), ansiGreen}}, cell{{fmt.Sprintf("%d retained observations | source UTC %s — %s | reset %s", count, watchStamp(first), watchStamp(last), watchHistoryReset(series.reset)), ansiDim}})
			shown++
		}
	}
	if shown == 0 {
		lines = append(lines, cell{{"No tracked observations/changes match this view; first observations are baseline-only.", ansiDim}})
	}
	return lines
}
