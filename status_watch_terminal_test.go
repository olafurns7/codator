package main

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/rivo/uniseg"
)

func TestWatchKeysSplitAndBounded(t *testing.T) {
	now := time.Now()
	var k watchKeys
	if got := k.feed([]byte{27}, now); len(got) != 0 {
		t.Fatal("split escape quit early")
	}
	if got := k.feed([]byte("["), now.Add(50*time.Millisecond)); len(got) != 0 {
		t.Fatal("split prefix consumed")
	}
	if got := k.feed([]byte("Crhq"), now.Add(100*time.Millisecond)); !reflect.DeepEqual(got, []string{"right", "text:r", "text:h", "text:q"}) {
		t.Fatalf("keys: %v", got)
	}
	k.feed([]byte{27}, now)
	if got := k.feed(nil, now.Add(150*time.Millisecond)); !reflect.DeepEqual(got, []string{"quit"}) {
		t.Fatal("bare Escape unbounded")
	}
	k.feed([]byte("\x1b["), now)
	if got := k.feed(nil, now.Add(time.Second)); len(got) != 0 || len(k.pending) != 0 {
		t.Fatal("incomplete control sequence retained")
	}
	if got := k.feed([]byte{3}, now); !reflect.DeepEqual(got, []string{"quit"}) {
		t.Fatal("raw Ctrl-C ignored")
	}
}

func TestWatchUnknownSequencesKeepFollowingKeys(t *testing.T) {
	now := time.Now()
	for _, test := range []struct {
		input string
		want  []string
	}{
		{"\x1b[15~q", []string{"text:q"}},
		{"\x1b[15~\x03", []string{"quit"}},
		{"\x1bOPq", []string{"text:q"}},
		{"\x1b[31mh lq", []string{"text:h", "text: ", "text:l", "text:q"}},
		{"\x1b[15;\x03q", []string{"quit", "text:q"}},
		{"\x1b]rq-title\a\x03", []string{"quit"}},
		{"\x1bPq-payload\x1b\\q", []string{"text:q"}},
	} {
		var k watchKeys
		if got := k.feed([]byte(test.input), now); !reflect.DeepEqual(got, test.want) {
			t.Errorf("input %q: got %v want %v", test.input, got, test.want)
		}
	}
	var k watchKeys
	if got := k.feed([]byte("\x1b[15"), now); len(got) != 0 || len(k.pending) == 0 {
		t.Fatal("unknown split sequence discarded early")
	}
	if got := k.feed([]byte("~q"), now.Add(50*time.Millisecond)); !reflect.DeepEqual(got, []string{"text:q"}) {
		t.Fatalf("split unsupported sequence: %v", got)
	}
	k.feed([]byte("\x1b[15;"), now)
	if got := k.feed(nil, now.Add(150*time.Millisecond)); len(got) != 0 || len(k.pending) != 0 {
		t.Fatal("incomplete sequence not bounded")
	}
	if got := k.feed([]byte{3}, now.Add(time.Second)); !reflect.DeepEqual(got, []string{"quit"}) {
		t.Fatal("quit after incomplete sequence lost")
	}
	k.feed([]byte("\x1b]"+strings.Repeat("1", 128)), now)
	if len(k.pending) != 0 {
		t.Fatal("unsupported sequence buffer unbounded")
	}
}

func TestWatchClipUnicodeStylesAndEdges(t *testing.T) {
	line := cell{{"A", ansiGreen}, {"界e\u0301👩‍💻Z", ansiYellow}}
	for _, test := range []struct {
		left, width int
		want        string
	}{
		{0, 1, "A"}, {1, 1, " "}, {2, 3, " e\u0301 "}, {3, 3, "e\u0301👩‍💻"}, {4, 2, "👩‍💻"}, {5, 2, " Z"},
	} {
		got := watchClip(line, test.left, test.width)
		if got.paint(false) != test.want || uniseg.StringWidth(got.paint(false)) > test.width {
			t.Fatalf("clip %d,%d = %q want %q", test.left, test.width, got.paint(false), test.want)
		}
		if strings.Contains(got.paint(false), "e") && !strings.Contains(got.paint(true), "\x1b[33me\u0301\x1b[0m") {
			t.Fatal("left-edge style lost")
		}
	}
	if got := watchClip(line, 0, 100).paint(true); got != line.paint(true) {
		t.Fatal("full-width color bytes changed")
	}
	split := cell{{"e", ansiGreen}, {"\u0301Z", ansiYellow}}
	if got := watchClip(split, 0, 1).paint(false); got != "e\u0301" {
		t.Fatalf("split cluster: %q", got)
	}
}

func TestWatchViewportColumnsNotesAndFrames(t *testing.T) {
	now := time.Now()
	wall := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s := watchState{}
	s.finish(watchFixture(wall), nil, now, wall)
	rows := s.snapshot.report.Accounts
	rows[0].Reason = strings.Repeat("long note ", 40) + "END-NOTE"
	lines := s.lines(now, wall)
	v := watchViewport{x: 10000, y: 10000}
	v.clamp(lines, 19, 3)
	var end strings.Builder
	for _, line := range lines {
		end.WriteString(watchClip(line, v.x, 19).paint(false))
	}
	if !strings.Contains(end.String(), "END-NOTE") {
		t.Fatal("rightmost note unreachable")
	}
	for _, size := range [][2]int{{180, 40}, {80, 24}, {40, 10}, {20, 8}, {10, 3}} {
		v = watchViewport{}
		frame := watchFrame(&s, &v, size[0], size[1], now, wall, true)
		parts := regexp.MustCompile(`\x1b\[[0-9]+;1H\x1b\[2K`).Split(frame, -1)
		for _, line := range parts {
			if uniseg.StringWidth(ansiPattern.ReplaceAllString(line, "")) > size[0]-1 {
				t.Fatalf("frame overflow at %v", size)
			}
		}
		plain := watchFrame(&s, &v, size[0], size[1], now, wall, false)
		if ansiPattern.MatchString(plain) {
			t.Fatal("NO_COLOR emitted SGR")
		}
	}
	wide := watchFrame(&s, &watchViewport{}, 180, 40, now, wall, false)
	for _, word := range []string{"EMAIL", "SESSION", "WEEKLY", "FABLE", "CREDITS", "RST", "LOGIN", "observed 1m ago", "specific denial", "fixture usage unavailable"} {
		if !strings.Contains(wide, word) {
			t.Fatalf("rich viewport missing %s", word)
		}
	}
	if dir := os.Getenv("CODATOR_WATCH_CAPTURE_DIR"); dir != "" {
		for _, color := range []bool{false, true} {
			label := "plain"
			if color {
				label = "ansi"
			}
			for _, size := range [][2]int{{180, 40}, {40, 10}} {
				v := watchViewport{}
				if size[0] == 40 {
					v.x = 35
					v.y = 8
				}
				name := filepath.Join(dir, "frame-"+label+"-"+stringSize(size)+".txt")
				if err := os.WriteFile(name, []byte(watchFrame(&s, &v, size[0], size[1], now, wall, color)), 0600); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
}

func stringSize(size [2]int) string { return fmt.Sprintf("%dx%d", size[0], size[1]) }

func TestWatchClosedClippedBoxesAndDetailEllipsis(t *testing.T) {
	wall := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	r := watchFixture(wall)
	r.Accounts[0].Reason = strings.Repeat("long note ", 30) + "END-NOTE"
	lines := statusPanelLines(r.Providers[0], r.Accounts[:3], wall)
	for _, width := range []int{19, 39, 80, 118} {
		for _, left := range []int{0, 1, 30, 100} {
			for _, line := range lines {
				full := line.paint(false)
				got := watchBoxClip(line, left, width).paint(false)
				if uniseg.StringWidth(got) > width {
					t.Fatal("closed box overflow", left, width, got)
				}
				if !strings.HasPrefix(got, string([]rune(full)[0])) || !strings.HasSuffix(got, string([]rune(full)[len([]rune(full))-1])) {
					t.Fatal("box lost edge", left, width, got)
				}
			}
		}
	}
	for _, line := range lines {
		if watchBoxClip(line, 0, 1000).paint(true) != line.paint(true) {
			t.Fatal("unclipped SGR box bytes changed")
		}
	}
	line := cell{{"Busy: " + strings.Repeat("界e\u0301", 40) + "END", ansiYellow}}
	if got := watchDetailClip(line, 0, 39).paint(false); !strings.HasSuffix(got, "…") || uniseg.StringWidth(got) > 39 {
		t.Fatal("truncated detail unmarked", got)
	}
	v := watchViewport{x: math.MaxInt}
	v.clamp([]cell{line}, 37, 5)
	if got := watchDetailClip(line, v.x, 39).paint(false); !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "END") || uniseg.StringWidth(got) > 39 {
		t.Fatal("detail tail not reachable", got)
	}
	if got := watchDetailClip(line, 0, 1000).paint(true); got != line.paint(true) {
		t.Fatal("full detail changed")
	}
}

func TestWatchPTY(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY prerequisite: python3 unavailable")
	}
	binary := filepath.Join(t.TempDir(), "codator")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Env = append(os.Environ(), "GOMAXPROCS=2")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("PTY binary build: %v\n%s", err, output)
	}
	cmd := exec.Command(python, "tests/status_watch_pty.py", binary)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY: %v\n%s", err, output)
	} else {
		t.Logf("%s", output)
	}
}
