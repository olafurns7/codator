package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
)

type statusReport struct {
	SchemaVersion int              `json:"schema_version"`
	GeneratedAt   time.Time        `json:"generated_at"`
	Providers     []statusProvider `json:"providers"`
	Accounts      []statusAccount  `json:"accounts"`
}

type statusProvider struct {
	Provider string `json:"provider"`
	Status   string `json:"status"`
	Error    string `json:"error,omitempty"`
}

type statusAccount struct {
	Provider        string                 `json:"provider"`
	Account         string                 `json:"account"`
	State           string                 `json:"state"`
	Launchable      bool                   `json:"launchable"`
	HeadroomPercent *float64               `json:"headroom_percent"`
	Reason          string                 `json:"reason,omitempty"`
	AvailableAt     *time.Time             `json:"available_at,omitempty"`
	ObservedAt      *time.Time             `json:"observed_at,omitempty"`
	NextProbeAt     *time.Time             `json:"next_probe_at,omitempty"`
	ResetCredits    int                    `json:"reset_credits,omitempty"`
	Windows         []statusWindow         `json:"windows"`
	KnownExhausted  []statusKnownExhausted `json:"known_exhausted"`

	display    string    `json:"-"`
	quota      quota     `json:"-"`
	probeErr   error     `json:"-"`
	now        time.Time `json:"-"`
	claudeText string    `json:"-"`
}

type statusWindow struct {
	Label            string     `json:"label"`
	UsedPercent      *float64   `json:"used_percent"`
	RemainingPercent *float64   `json:"remaining_percent"`
	Exhausted        bool       `json:"exhausted"`
	ResetsAt         *time.Time `json:"resets_at,omitempty"`
	Unavailable      string     `json:"unavailable,omitempty"`
	text             string
	resetForState    time.Time `json:"-"`
}

type statusKnownExhausted struct {
	Scope         string    `json:"scope"`
	Until         time.Time `json:"until"`
	ObservedAt    time.Time `json:"observed_at"`
	untilForState time.Time `json:"-"`
}

type statusDetails struct {
	Windows        []statusWindow
	KnownExhausted []statusKnownExhausted
	ObservedAt     *time.Time
	NextProbeAt    *time.Time
}

func statusJSON(store *Store, out io.Writer, providers ...string) error {
	report, statusErr := collectStatus(store, providers, nil)
	if err := renderStatusJSON(out, report); err != nil {
		return err
	}
	return statusErr
}

func renderStatusJSON(out io.Writer, report statusReport) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(report)
}

func renderStatusEvent(out io.Writer, provider statusProvider, account *statusAccount) {
	if account == nil {
		switch provider.Status {
		case "unavailable":
			fmt.Fprintf(out, "%s: unavailable (%s)\n", provider.Provider, provider.Error)
		case "no_accounts":
			fmt.Fprintf(out, "%s: no accounts enrolled\n", provider.Provider)
		}
		return
	}
	text := account.display
	if text == "" && account.Provider == "claude" {
		text = account.claudeText
	}
	if text == "" {
		text = quotaStatus(account.quota, account.probeErr, account.now)
	}
	fmt.Fprintf(out, "%s %s: %s\n", account.Provider, account.Account, text)
}

func quotaStatusAccount(provider, account string, q quota, probeErr error, now time.Time, details statusDetails) statusAccount {
	windows := details.Windows
	if windows == nil {
		windows = []statusWindow{}
	}
	knownExhausted := details.KnownExhausted
	if knownExhausted == nil {
		knownExhausted = []statusKnownExhausted{}
	}
	state := "unknown"
	var availableAt *time.Time
	if probeErr == nil {
		switch {
		case q.Eligible:
			state = "available"
		case !q.Known:
			state = "unknown"
		default:
			state = "ineligible"
			availableAt = statusExhaustedUntil(windows, knownExhausted, now)
			if availableAt != nil {
				state = "exhausted"
			}
		}
	}
	var headroom *float64
	if q.Known && !math.IsNaN(q.Headroom) && !math.IsInf(q.Headroom, 0) && q.Headroom >= 0 && q.Headroom <= 100 {
		value := q.Headroom
		headroom = &value
	}
	reason := q.Reason
	if reason == "" && (probeErr != nil || !q.Eligible) {
		reason = "usage is unknown"
		if probeErr != nil {
			reason = "usage check failed"
		}
	}
	row := statusAccount{
		Provider:        provider,
		Account:         account,
		State:           state,
		Launchable:      q.Eligible,
		HeadroomPercent: headroom,
		Reason:          statusReason(reason),
		AvailableAt:     availableAt,
		ObservedAt:      details.ObservedAt,
		NextProbeAt:     details.NextProbeAt,
		Windows:         windows,
		KnownExhausted:  knownExhausted,
		quota:           q,
		probeErr:        probeErr,
		now:             now,
	}
	if provider == "codex" && q.ResetCredits > 0 {
		row.ResetCredits = q.ResetCredits
	}
	return row
}

func unavailableStatusAccount(provider, account, state, reason, display string) statusAccount {
	return statusAccount{
		Provider:       provider,
		Account:        account,
		State:          state,
		Reason:         reason,
		Windows:        []statusWindow{},
		KnownExhausted: []statusKnownExhausted{},
		display:        display,
	}
}

func statusExhaustedUntil(windows []statusWindow, knownExhausted []statusKnownExhausted, now time.Time) *time.Time {
	var latest time.Time
	for _, window := range windows {
		reset := window.resetForState
		if reset.IsZero() && window.ResetsAt != nil {
			reset = *window.ResetsAt
		}
		if window.Exhausted && reset.After(now) && reset.After(latest) {
			latest = reset
		}
	}
	for _, denial := range knownExhausted {
		until := denial.untilForState
		if until.IsZero() {
			until = denial.Until
		}
		if until.After(now) && until.After(latest) {
			latest = until
		}
	}
	return statusTime(latest)
}

func codexStatusWindows(windows []usageWindow) []statusWindow {
	result := make([]statusWindow, 0, len(windows))
	for _, window := range windows {
		item := statusWindow{Label: window.Label, Exhausted: window.Used >= 100}
		if math.IsNaN(window.Used) || math.IsInf(window.Used, 0) || window.Used < 0 || window.Used > 100 {
			item.Exhausted = false
			item.Unavailable = "malformed utilization"
		} else {
			used, remaining := window.Used, 100-window.Used
			item.UsedPercent, item.RemainingPercent = &used, &remaining
		}
		item.ResetsAt = statusTime(window.ResetsAt)
		item.resetForState = window.ResetsAt
		result = append(result, item)
	}
	return result
}

func statusTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	utc := statusUTCSecond(value)
	return &utc
}

func statusUTCSecond(value time.Time) time.Time {
	return value.UTC().Truncate(time.Second)
}

func statusFutureTime(value, now time.Time) *time.Time {
	if !value.After(now) {
		return nil
	}
	return statusTime(value)
}

func statusReason(reason string) string {
	start := strings.LastIndex(reason, claudeRetryPrefix)
	if start < 0 {
		return reason
	}
	stampStart := start + len(claudeRetryPrefix)
	stamp, err := time.Parse(time.RFC3339Nano, reason[stampStart:])
	if err != nil {
		return reason
	}
	return reason[:stampStart] + statusUTCSecond(stamp).Format(time.RFC3339)
}
