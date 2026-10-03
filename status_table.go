package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ansiBold   = "1"
	ansiDim    = "2"
	ansiRed    = "31"
	ansiGreen  = "32"
	ansiYellow = "33"
)

// span is a run of text in one style; widths count the text only, never escapes.
type span struct{ text, style string }

type cell []span

func (c cell) width() int {
	n := 0
	for _, s := range c {
		n += utf8.RuneCountInString(s.text)
	}
	return n
}

func (c cell) paint(color bool) string {
	var b strings.Builder
	for _, s := range c {
		b.WriteString(paint(s.text, s.style, color))
	}
	return b.String()
}

func paint(text, style string, color bool) string {
	if !color || style == "" || text == "" {
		return text
	}
	return "\x1b[" + style + "m" + text + "\x1b[0m"
}

type statusColumn struct {
	header string
	right  bool
	cell   func(statusAccount) cell
}

// statusColor reports whether status may color out: a terminal, NO_COLOR unset, TERM not dumb.
func statusColor(out io.Writer) bool {
	file, ok := out.(*os.File)
	if !ok || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// renderStatusPanel draws one provider as a boxed table with one row per account.
func renderStatusPanel(out io.Writer, provider statusProvider, rows []statusAccount, color bool, now time.Time) {
	title := provider.Provider
	var lines [][]cell // each line is a list of cells; a single-cell line is a note
	switch provider.Status {
	case "unavailable":
		lines = append(lines, []cell{{{"unavailable (" + provider.Error + ")", ansiDim}}})
	case "no_accounts":
		lines = append(lines, []cell{{{"no accounts enrolled", ansiDim}}})
	}
	var oldest time.Time
	for _, row := range rows {
		if row.ObservedAt != nil && (oldest.IsZero() || row.ObservedAt.Before(oldest)) {
			oldest = *row.ObservedAt
		}
	}
	if !oldest.IsZero() {
		title += " · observed " + durationText(now.Sub(oldest)) + " ago"
	}

	columns := statusColumns(rows, now)
	widths := make([]int, len(columns))
	header := make([]cell, len(columns))
	for i, column := range columns {
		header[i] = cell{{column.header, ansiBold}}
		widths[i] = header[i].width()
	}
	type tableRow struct {
		cells []cell
		notes []string
	}
	table := make([]tableRow, 0, len(rows))
	for _, row := range rows {
		cells := make([]cell, len(columns))
		for i, column := range columns {
			cells[i] = column.cell(row)
			widths[i] = max(widths[i], cells[i].width())
		}
		table = append(table, tableRow{cells, statusNotes(row, now)})
	}
	join := func(cells []cell) cell {
		var line cell
		for i, c := range cells {
			pad := span{strings.Repeat(" ", widths[i]-c.width()), ""}
			if i > 0 {
				line = append(line, span{"  ", ""})
			}
			if columns[i].right {
				line = append(append(line, pad), c...)
			} else {
				line = append(append(line, c...), pad)
			}
		}
		return line
	}
	var body []cell
	for _, line := range lines {
		body = append(body, line[0])
	}
	if len(rows) > 0 {
		body = append(body, join(header))
	}
	for _, row := range table {
		body = append(body, join(row.cells))
		for _, note := range row.notes {
			body = append(body, cell{{"  " + note, ansiDim}})
		}
	}

	inner := utf8.RuneCountInString(title) + 4
	for _, line := range body {
		inner = max(inner, line.width()+2)
	}
	border := func(text string) string { return paint(text, ansiDim, color) }
	fmt.Fprintln(out, border("╭─ ")+title+border(" "+strings.Repeat("─", inner-utf8.RuneCountInString(title)-3)+"╮"))
	for _, line := range body {
		fmt.Fprintln(out, border("│")+" "+line.paint(color)+strings.Repeat(" ", inner-line.width()-1)+border("│"))
	}
	fmt.Fprintln(out, border("╰"+strings.Repeat("─", inner)+"╯"))
}

func statusColumns(rows []statusAccount, now time.Time) []statusColumn {
	columns := []statusColumn{{header: "ACCOUNT", cell: statusAccountCell}}
	var labels []string
	seen := map[string]bool{}
	email, credits, resets, placeholder := false, false, false, false
	for _, row := range rows {
		email = email || row.Email != ""
		credits = credits || row.CreditsBalance != ""
		resets = resets || row.ResetCredits > 0
		placeholder = placeholder || statusPlaceholder(row) != ""
		for _, window := range row.Windows {
			if !seen[window.Label] {
				seen[window.Label] = true
				labels = append(labels, window.Label)
			}
		}
	}
	if email {
		columns = append(columns, statusColumn{header: "EMAIL", cell: func(row statusAccount) cell { return cell{{row.Email, ""}} }})
	}
	// A row without windows to show puts its placeholder word in the first window column.
	if len(labels) == 0 && placeholder {
		labels = []string{""}
	}
	for i, label := range labels {
		columns = append(columns, statusColumn{header: statusWindowHeader(label), cell: func(row statusAccount) cell {
			if word := statusPlaceholder(row); word != "" {
				if i > 0 {
					return nil
				}
				return cell{{word, ansiDim}}
			}
			for _, window := range row.Windows {
				if window.Label == label {
					return statusWindowCell(window, now)
				}
			}
			return cell{{"—", ansiDim}}
		}})
	}
	if credits {
		columns = append(columns, statusColumn{header: "CREDITS", right: true, cell: func(row statusAccount) cell { return cell{{row.CreditsBalance, ""}} }})
	}
	if resets {
		columns = append(columns, statusColumn{header: "RST", right: true, cell: func(row statusAccount) cell {
			if row.ResetCredits == 0 {
				return nil
			}
			return cell{{fmt.Sprint(row.ResetCredits), ""}}
		}})
	}
	return columns
}

func statusWindowHeader(label string) string {
	switch label {
	case "Weekly limit", "Weekly (all models)":
		return "WEEKLY"
	case "Current session":
		return "SESSION"
	case "5-hour limit":
		return "5H"
	}
	if strings.HasPrefix(label, "Weekly (") && strings.HasSuffix(label, ")") {
		label = label[len("Weekly (") : len(label)-1]
	}
	return strings.ToUpper(label)
}

// statusAccountCell puts a state dot before the name: ● launchable, ○ not, ◐ busy.
func statusAccountCell(row statusAccount) cell {
	glyph, style := "○", ansiDim
	switch row.State {
	case "available":
		glyph, style = "●", ansiGreen
		if row.OnCredits {
			style = ansiYellow
		}
	case "exhausted":
		style = ansiRed
	case "busy":
		glyph, style = "◐", ansiYellow
	}
	return cell{{glyph, style}, {" " + row.Account, ""}}
}

// statusWord names a state the dot cannot tell apart, or "".
func statusWord(row statusAccount) string {
	switch {
	case row.State == "busy", row.State == "unknown":
		return row.State
	case row.State == "ineligible":
		return "skipped"
	case row.OnCredits:
		return "on credits"
	}
	return ""
}

// statusPlaceholder is the word a row shows instead of window cells when it has none to show.
func statusPlaceholder(row statusAccount) string {
	if statusHasWindow(row) {
		return ""
	}
	return statusWord(row)
}

func statusHasWindow(row statusAccount) bool {
	for _, window := range row.Windows {
		if window.Unavailable == "" && window.RemainingPercent != nil {
			return true
		}
	}
	return false
}

func statusWindowCell(window statusWindow, now time.Time) cell {
	if window.Unavailable != "" || window.RemainingPercent == nil {
		return cell{{"—", ansiDim}}
	}
	remaining := math.Min(100, math.Max(0, *window.RemainingPercent))
	style := ansiGreen
	switch {
	case remaining < 20:
		style = ansiRed
	case remaining <= 50:
		style = ansiYellow
	}
	result := cell{{statusBar(remaining) + fmt.Sprintf(" %3d%%", int(math.Round(remaining))), style}}
	if window.ResetsAt != nil && window.ResetsAt.After(now) {
		result = append(result, span{" " + durationText(window.ResetsAt.Sub(now)), ""})
	}
	return result
}

// statusBar draws remaining percent as ten cells in eighths.
func statusBar(remaining float64) string {
	eighths := int(remaining * 80 / 100)
	full, part := eighths/8, eighths%8
	bar := strings.Repeat("█", full)
	if part > 0 {
		bar += string([]rune("▏▎▍▌▋▊▉")[part-1])
		full++
	}
	return bar + strings.Repeat("░", 10-full)
}

// statusNotes keeps what does not fit a cell: reasons, unavailable data, denials, cooldowns.
func statusNotes(row statusAccount, now time.Time) []string {
	var notes []string
	// The dot and the bars already say an exhausted row is out; keep only specific reasons.
	if row.Reason != "" && !(row.State == "exhausted" && strings.HasPrefix(row.Reason, "all applicable usage headroom is exhausted")) {
		notes = append(notes, row.Reason)
	}
	if word := statusWord(row); word != "" && statusHasWindow(row) {
		notes = append(notes, word)
	}
	if row.claudeUnavailable != "" {
		notes = append(notes, "usage unavailable ("+row.claudeUnavailable+")")
	}
	seen := map[string]bool{}
	for _, window := range row.Windows {
		if window.Unavailable != "" && window.Unavailable != "not reported" {
			notes = append(notes, window.Label+": unavailable ("+window.Unavailable+")")
		}
		// A column shows a label's first window; later ones with that label go here.
		if seen[window.Label] && window.Unavailable == "" && window.RemainingPercent != nil {
			note := fmt.Sprintf("%s: %d%% remaining", statusWindowHeader(window.Label), int(math.Round(math.Min(100, math.Max(0, *window.RemainingPercent)))))
			if window.ResetsAt != nil && window.ResetsAt.After(now) {
				note += ", resets in " + durationText(window.ResetsAt.Sub(now))
			}
			notes = append(notes, note)
		}
		seen[window.Label] = true
	}
	for _, denial := range row.KnownExhausted {
		notes = append(notes, fmt.Sprintf("known exhausted: %s for %s (observed %s ago)",
			denial.Scope, durationText(denial.Until.Sub(now)), durationText(now.Sub(denial.ObservedAt))))
	}
	if row.claudeUnavailable != "" && row.NextProbeAt != nil && row.NextProbeAt.After(now) &&
		!strings.Contains(strings.Join(notes, "\n"), row.NextProbeAt.UTC().Format(time.RFC3339)) {
		notes = append(notes, "probe cooldown for "+durationText(row.NextProbeAt.Sub(now)))
	}
	return notes
}
