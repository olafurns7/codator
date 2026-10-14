package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rivo/uniseg"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

func validateWatchTerminal(in, out *os.File) error {
	if !term.IsTerminal(int(in.Fd())) || !term.IsTerminal(int(out.Fd())) || os.Getenv("TERM") == "" || os.Getenv("TERM") == "dumb" {
		return errors.New("--watch requires input/output terminals and a supported TERM; use codator status")
	}
	return nil
}

func enterWatchTerminal(in, out *os.File) (func() error, error) {
	state, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return nil, errors.New("cannot enter terminal raw mode")
	}
	restore := func() error {
		_, screenErr := io.WriteString(out, "\x1b[?25h\x1b[?1049l")
		modeErr := term.Restore(int(in.Fd()), state)
		if screenErr != nil || modeErr != nil {
			return errors.New("cannot fully restore terminal")
		}
		return nil
	}
	if _, err := io.WriteString(out, "\x1b[?1049h\x1b[?25l"); err != nil {
		return nil, errors.Join(errors.New("cannot enter terminal screen"), restore())
	}
	return restore, nil
}

func watchTerminalSize(out *os.File) (int, int, error) {
	w, h, err := term.GetSize(int(out.Fd()))
	if err != nil {
		return 0, 0, errors.New("cannot read terminal size")
	}
	return w, h, nil
}

func readWatchInput(in *os.File, timeout int) ([]byte, error) {
	fds := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
	if _, err := unix.Poll(fds, timeout); err != nil {
		if errors.Is(err, unix.EINTR) {
			return nil, nil
		}
		return nil, errors.New("cannot poll terminal input")
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return nil, errors.New("terminal input closed")
	}
	if fds[0].Revents&unix.POLLIN == 0 {
		return nil, nil
	}
	buf := make([]byte, 128)
	n, err := unix.Read(int(in.Fd()), buf)
	if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
		return nil, nil
	}
	if err != nil || n == 0 {
		return nil, errors.New("terminal input closed")
	}
	return buf[:n], nil
}

type watchKeys struct {
	pending []byte
	since   time.Time
}

// Unsupported controls are skipped as units, leaving later independent keys intact.
func watchEscapeLength(input []byte) int {
	if len(input) < 2 {
		return 0
	}
	switch input[1] {
	case ']', 'P', 'X', '^', '_':
		for i := 2; i < len(input); i++ {
			if input[i] == 7 {
				return i + 1
			}
			if input[i] == 27 && i+1 < len(input) && input[i+1] == '\\' {
				return i + 2
			}
			if input[i] == 3 {
				return i
			}
		}
		return 0
	}
	start, final := 1, byte(0x30)
	if input[1] == '[' || input[1] == 'O' {
		start, final = 2, 0x40
	}
	for i := start; i < len(input); i++ {
		if input[i] >= final && input[i] <= 0x7e {
			return i + 1
		}
		if input[i] < 0x20 || input[i] > 0x7e {
			return i
		}
	}
	return 0
}

func (k *watchKeys) feed(input []byte, now time.Time) []string {
	if len(k.pending) == 0 && len(input) > 0 {
		k.since = now
	}
	k.pending = append(k.pending, input...)
	var keys []string
	sequences := map[string]string{"\x1b[A": "up", "\x1b[B": "down", "\x1b[C": "right", "\x1b[D": "left", "\x1b[5~": "pageup", "\x1b[6~": "pagedown", "\x1b[H": "home", "\x1b[F": "end", "\x1b[1~": "home", "\x1b[4~": "end", "\x1bOH": "home", "\x1bOF": "end"}
	for len(k.pending) > 0 {
		if k.pending[0] == 27 {
			pending := string(k.pending)
			matched := false
			for sequence, key := range sequences {
				if strings.HasPrefix(pending, sequence) {
					keys = append(keys, key)
					k.pending = k.pending[len(sequence):]
					matched = true
					break
				}
			}
			if matched {
				k.since = now
				continue
			}
			n := watchEscapeLength(k.pending)
			if n == 0 && len(k.pending) < 128 && now.Sub(k.since) < 150*time.Millisecond {
				break
			}
			if len(k.pending) == 1 {
				keys = append(keys, "quit")
			}
			if n == 0 {
				n = len(k.pending)
			}
			k.pending = k.pending[n:]
			k.since = now
			continue
		}
		consumed := 1
		switch k.pending[0] {
		case 3:
			keys = append(keys, "quit")
		case 13, 10:
			keys = append(keys, "enter")
		case 9:
			keys = append(keys, "tab")
		case 7:
			keys = append(keys, "cancel")
		case 21:
			keys = append(keys, "clear")
		case 8, 127:
			keys = append(keys, "backspace")
		default:
			if k.pending[0] >= 32 {
				if !utf8.FullRune(k.pending) && now.Sub(k.since) < 150*time.Millisecond {
					return keys
				}
				r, n := utf8.DecodeRune(k.pending)
				consumed = n
				if r != utf8.RuneError || n != 1 {
					keys = append(keys, "text:"+string(r))
				}
			}
		}
		k.pending = k.pending[consumed:]
		k.since = now
	}
	return keys
}

func watchLineWidth(line cell) int { return uniseg.StringWidth(line.paint(false)) }

// Pin the incumbent frame edges while its interior keeps scrolling.
func watchBoxClip(line cell, left, width int) cell {
	text := line.paint(false)
	whole := watchLineWidth(line)
	border := strings.HasPrefix(text, "│") && strings.HasSuffix(text, "│")
	horizontal := strings.HasPrefix(text, "╭") && strings.HasSuffix(text, "╮") || strings.HasPrefix(text, "╰") && strings.HasSuffix(text, "╯")
	if width < 2 || !border && !horizontal || left == 0 && whole <= width {
		return watchClip(line, left, width)
	}
	inner := watchClip(watchClip(line, 1, whole-2), left, width-2)
	fill := " "
	if horizontal {
		fill = "─"
	}
	result := watchClip(line, 0, 1)
	result = append(result, inner...)
	result = append(result, span{strings.Repeat(fill, max(0, width-2-watchLineWidth(inner))), ansiDim})
	return append(result, watchClip(line, whole-1, 1)...)
}

func watchDetailClip(line cell, left, width int) cell {
	if width <= 0 {
		return nil
	}
	whole := watchLineWidth(line)
	if left == 0 && whole <= width {
		return line
	}
	if width == 1 {
		return cell{{"…", ansiDim}}
	}
	var result cell
	room := width
	if left > 0 {
		result = append(result, span{"…", ansiDim})
		room--
	}
	right := whole > left+room
	if right {
		room--
	}
	result = append(result, watchClip(line, left, max(0, room))...)
	if right && width > 1 {
		result = append(result, span{"…", ansiDim})
	}
	return result
}

func watchClip(line cell, left, width int) cell {
	if width <= 0 {
		return nil
	}
	text := line.paint(false)
	if left == 0 && uniseg.StringWidth(text) <= width {
		return line
	}
	var result cell
	graphemes := uniseg.NewGraphemes(text)
	column, spanIndex, spanEnd, emitted := 0, 0, 0, 0
	if len(line) > 0 {
		spanEnd = len(line[0].text)
	}
	for graphemes.Next() {
		start, _ := graphemes.Positions()
		for spanIndex+1 < len(line) && start >= spanEnd {
			spanIndex++
			spanEnd += len(line[spanIndex].text)
		}
		w := graphemes.Width()
		end := column + w
		if column >= left+width {
			break
		}
		if column >= left && end <= left+width {
			if gap := column - left - emitted; gap > 0 {
				result = append(result, span{strings.Repeat(" ", gap), ""})
				emitted += gap
			}
			result = append(result, span{graphemes.Str(), line[spanIndex].style})
			emitted += w
		} else if end > left && column < left+width {
			visible := min(end, left+width) - max(column, left)
			result = append(result, span{strings.Repeat(" ", visible), ""})
			emitted += visible
		}
		column = end
	}
	return result
}

type watchViewport struct{ x, y int }

func (v *watchViewport) clamp(lines []cell, width, height int) int {
	widest := 0
	for _, line := range lines {
		widest = max(widest, watchLineWidth(line))
	}
	v.x = max(0, min(v.x, max(0, widest-width)))
	v.y = max(0, min(v.y, max(0, len(lines)-height)))
	return widest
}

func watchFrame(s *watchState, v *watchViewport, width, height int, now, wall time.Time, color bool) string {
	width = max(0, width-1)
	if width == 0 || height <= 0 {
		return ""
	}
	var frame strings.Builder
	write := func(row int, line cell, left int) {
		fmt.Fprintf(&frame, "\x1b[%d;1H\x1b[2K", row+1)
		frame.WriteString(watchDetailClip(line, left, width).paint(color))
	}
	if width < 19 || height < 8 {
		for row := 0; row < height; row++ {
			write(row, nil, 0)
		}
		write(0, cell{{"Widen to 20x8; q quits.", ""}}, 0)
		return frame.String()
	}
	bodyHeight := height - 5
	var lines []cell
	var locations map[watchAccountID]int
	if s.view.body != "" {
		v = &s.view.detailViewport
		if s.view.body == "timeline" {
			lines = s.view.timelineLines(s, now, wall)
		} else if s.view.body == "sparklines" || s.view.body == "feed" {
			lines = s.view.historyLines(s, s.view.body == "feed", now, wall)
		} else {
			var row int
			lines, row = s.view.overviewLines(s, now, wall)
			if s.view.bucketReveal && row >= 0 {
				if row < v.y {
					v.y = row
				} else if row >= v.y+bodyHeight {
					v.y = row - bodyHeight + 1
				}
			}
			s.view.bucketReveal = false
		}
	} else {
		lines, locations = s.tableLayout(now, wall, s.view.visible, true)
		lines = append([]cell{{{s.view.summary(s), ansiDim}}}, lines...)
		lines = append(lines, s.view.help(s)[1:]...)
	}
	split := s.view.detail && watchSplit(width+1, height)
	tableWidth := width
	if split {
		tableWidth = max(80, width*2/3)
		tableWidth = min(tableWidth, width-41)
	}
	if s.view.body == "" && s.view.reveal {
		if row, ok := locations[s.view.selected]; ok {
			row++
			if row < v.y {
				v.y = row
			} else if row >= v.y+bodyHeight {
				v.y = row - bodyHeight + 1
			}
			if s.view.revealAccount {
				v.x = 0
			}
		}
		s.view.reveal = false
		s.view.revealAccount = false
	}
	// Leave room for both ellipsis edges so the full prose tail remains reachable.
	scrollWidth := max(1, tableWidth-2)
	widest := v.clamp(lines, scrollWidth, bodyHeight)
	var details []cell
	if s.view.detail {
		details = s.view.detailLines(s, now, wall)
	}
	detailWidth := width
	if split {
		detailWidth = width - tableWidth - 1
	}
	if s.view.detail {
		s.view.detailViewport.clamp(details, max(1, detailWidth-2), bodyHeight)
	}
	status, style := "Unknown", ansiDim
	if s.snapshot != nil {
		status, style = "Snapshot", ansiGreen
	}
	if s.snapshot != nil && s.stale(now, wall) {
		status, style = "STALE / retained data", ansiYellow
	}
	write(0, cell{{"Codator | ", ansiBold}, {status, style}}, 0)
	next := fmt.Sprintf("%ds", max(0, int(s.next.Sub(now).Seconds())))
	if s.inflight {
		next = "collecting (120s limit)"
	}
	if s.disabled {
		next = "disabled: cleanup uncertain"
	}
	write(1, cell{{"Next refresh: " + next, ""}}, 0)
	message := "Last OK: never"
	style = ansiDim
	if !s.lastSuccess.IsZero() {
		message = "Last OK: " + s.lastSuccess.UTC().Format("15:04:05Z")
	}
	if s.manualNotice {
		message = "r gated; wait for next"
	}
	if s.errorText != "" {
		message, style = "ERROR: "+s.errorText, ansiRed
	}
	if s.disabled {
		message, style = "Cleanup uncertain", ansiRed
	}
	write(2, cell{{message, style}}, 0)
	for row := 0; row < bodyHeight; row++ {
		var line cell
		if i := v.y + row; i < len(lines) {
			if s.view.body == "" && (strings.HasPrefix(lines[i].paint(false), "│") || strings.HasPrefix(lines[i].paint(false), "╭") || strings.HasPrefix(lines[i].paint(false), "╰")) {
				line = watchBoxClip(lines[i], v.x, tableWidth)
			} else {
				line = watchDetailClip(lines[i], v.x, tableWidth)
			}
		}
		if s.view.detail {
			var detail cell
			if i := s.view.detailViewport.y + row; i < len(details) {
				detail = watchDetailClip(details[i], s.view.detailViewport.x, detailWidth)
			}
			if split {
				line = append(line, span{strings.Repeat(" ", tableWidth-watchLineWidth(line)), ""}, span{"│", ansiDim})
				line = append(line, detail...)
			} else {
				line = detail
			}
		}
		write(row+3, line, 0)
	}
	controls := "r refresh | q/Esc/Ctrl-C quit | [ ] select Enter detail Tab focus s sort p/f filters / name"
	controls += " | t timeline a overview g sparklines c feed"
	if s.view.body != "" {
		controls = "g graphs c feed t/a views | r gated | q/Esc/Ctrl-C quit"
		if s.view.body == "overview" {
			controls = "[ ] bucket Enter filter | " + controls
		}
	}
	if width < 28 {
		controls = "r gated | q/Esc quit"
		if s.view.body != "" {
			controls = "g/c/t/a views | q/Esc quit"
		}
	}
	if s.view.editing {
		hint := " | Enter apply, Ctrl-G cancel; Esc/Ctrl-C quit"
		if width < 70 {
			hint = " | Enter/^G"
		}
		draft := cell{{watchClean(s.view.draft), ""}}
		room := max(0, width-7-len(hint))
		controls = "Name > " + watchClip(draft, max(0, watchLineWidth(draft)-room), room).paint(false) + hint
	}
	write(height-2, cell{{controls, ansiDim}}, 0)
	footer := fmt.Sprintf("arrows/h/l PgUp/PgDn Home/End | rows %d-%d/%d cols %d-%d/%d", v.y+1, min(v.y+bodyHeight, len(lines)), len(lines), v.x+1, min(v.x+tableWidth, widest), widest)
	if width < 70 {
		footer = fmt.Sprintf("arrows/h/l PgUp/PgDn | y:%d x:%d", v.y, v.x)
	}
	if width < 35 {
		footer = fmt.Sprintf("arrows h/l | y%d x%d", v.y, v.x)
	}
	selected := "none"
	if s.view.hasSelection {
		selected = watchClean(s.view.selected.provider + "/" + s.view.selected.account)
	}
	focus := "table"
	if s.view.body != "" {
		focus = s.view.body
		selected = "none"
		if s.view.body == "overview" {
			pi, state := s.view.bucket(s)
			if pi >= 0 {
				selected = watchClean(s.snapshot.report.Providers[pi].Provider + "/" + watchStateNames[state])
			}
		}
	}
	if s.view.detail && (!split || s.view.detailFocus) {
		focus = "detail"
		footer = fmt.Sprintf("y:%d x:%d", s.view.detailViewport.y, s.view.detailViewport.x)
	}
	footer = "Selected " + selected + " | " + focus + " | " + footer
	write(height-1, cell{{footer, ansiDim}}, 0)
	return frame.String()
}

func drawWatch(out *os.File, s *watchState, v *watchViewport, width, height int, now, wall time.Time) error {
	frame := watchFrame(s, v, width, height, now, wall, statusColor(out))
	n, err := io.WriteString(out, frame)
	if err != nil || n != len(frame) {
		return errors.New("cannot draw terminal screen")
	}
	return nil
}
