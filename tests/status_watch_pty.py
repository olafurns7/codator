"""Actual Codator binary, private temp stores and fake native executables only."""
import fcntl
import json
import os
import pathlib
import pty
import re
import select
import signal
import struct
import subprocess
import sys
import tempfile
import termios
import time
import unicodedata

os.umask(0o077)
BINARY = sys.argv[1]
SGR = re.compile(rb"\x1b\[[0-9;]*m")


def drain(master, seconds):
    data = bytearray()
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], min(0.05, max(0, deadline-time.monotonic())))
        if ready:
            try:
                chunk = os.read(master, 65536)
            except OSError:
                break
            if not chunk:
                break
            data.extend(chunk)
    return bytes(data)


def screen(data, cols, rows):
    grid = [[" "] * cols for _ in range(rows)]
    x = y = 0
    text = data.decode("utf-8", "replace")
    token = re.compile(r"\x1b\[([0-9;?]*)([A-Za-z])")
    i = 0
    while i < len(text):
        match = token.match(text, i)
        if match:
            args, op = match.groups()
            if op == "H":
                nums = [int(n or 1) for n in args.split(";")]
                y, x = (nums + [1])[:2]
                y -= 1
                x -= 1
            elif op == "K" and args == "2" and 0 <= y < rows:
                grid[y] = [" "] * cols
            i = match.end()
            continue
        c = text[i]
        i += 1
        if c == "\r":
            x = 0
        elif c == "\n":
            y += 1
        elif not unicodedata.combining(c):
            width = 2 if unicodedata.east_asian_width(c) in "WF" else 1
            assert x + width <= cols, ("screen wrapping overflow", x, cols, repr(c))
            if 0 <= y < rows and 0 <= x < cols:
                grid[y][x] = c
            x += width
    return "\n".join("".join(row) for row in grid)


def fixture(root, busy=False, injection=False):
    native = root / "data/codator/codex/work/native"
    native.mkdir(parents=True, mode=0o700)
    bin_dir = root / "bin"
    bin_dir.mkdir(mode=0o700)
    log = root / "calls"
    email = "fixture\x1b[2J\r\n\u202e@example.invalid" if injection else "fixture@example.invalid"
    fake = f"""#!{sys.executable}
import json,os,sys,time
with open({str(log)!r},'a') as f: f.write('probe\\n')
if {busy!r}: time.sleep(30)
for line in sys.stdin:
 req=json.loads(line)
 if 'id' not in req: continue
 if req['method']=='initialize': result={{}}
 elif req['method']=='account/read': result={{'account':{{'type':'chatgpt','planType':'pro','email':{email!r}}}}}
 else:
  result={{'ordinaryUsageAllowed':True,'rateLimitsByLimitId':{{'codex':{{'primary':{{'usedPercent':25,'windowDurationMins':300,'resetsAt':time.time()+3600}},'secondary':{{'usedPercent':50,'windowDurationMins':10080,'resetsAt':time.time()+86400}},'spendControlReached':False,'credits':{{'hasCredits':True,'unlimited':False,'balance':'42.50'}}}}}},'rateLimitResetCredits':{{'availableCount':2}}}}
 print(json.dumps({{'id':req['id'],'result':result}}),flush=True)
"""
    (bin_dir / "codex").write_text(fake)
    (bin_dir / "codex").chmod(0o700)
    env = dict(os.environ, XDG_DATA_HOME=str(root / "data"), TERM="xterm-256color",
               PATH=str(bin_dir)+os.pathsep+os.environ.get("PATH", ""))
    env.pop("NO_COLOR", None)
    return env, log


def resize(fd, cols, rows):
    fcntl.ioctl(fd, termios.TIOCSWINSZ, struct.pack("HHHH", rows, cols, 0, 0))


# XNU bsd/sys/termios.h:259 "retype pending input (state)". ttioctl TIOCSETA
# (bsd/kern/tty.c:1358-1361,1391) ORs it into lflag whenever ICANON turns back
# on, and only a later slave read (tty.c:1730) clears it: kernel state, not a
# setting the watcher restores.
DARWIN_PENDIN = 0x20000000


def termios_delta(before, after, platform):
    """Changed fields; exact except Darwin's kernel-set PENDIN after raw mode."""
    after = list(after)
    if platform == "darwin" and not before[3] & DARWIN_PENDIN:
        after[3] &= ~DARWIN_PENDIN
    fields = ("iflag", "oflag", "cflag", "lflag", "ispeed", "ospeed")
    delta = {f: (hex(b), hex(a), "xor " + hex(a ^ b)) for f, b, a in zip(fields, before, after) if a != b}
    delta.update({"cc[%d]" % i: (b, a) for i, (b, a) in enumerate(zip(before[6], after[6])) if a != b})
    assert bool(delta) == (after != list(before)), "delta missed a field"
    return delta


def termios_selfcheck():
    # Hosted darwin arm64 tuple (run 38651859651): sole delta lflag 0x5cb -> 0x200005cb.
    cc = [bytes([c]) for c in (4, 255, 255, 127, 23, 21, 18, 255, 3, 28, 26, 25, 17, 19, 22, 15, 1, 0, 20, 255)]
    before = [11010, 3, 19200, 0x5cb, 9600, 9600, cc]
    pendin = before[:3] + [0x200005cb] + before[4:]
    assert termios_delta(before, before, "darwin") == {} and termios_delta(before, before, "linux") == {}
    assert termios_delta(before, pendin, "darwin") == {}, "darwin PENDIN not normalized"
    assert termios_delta(before, pendin, "linux"), "PENDIN accepted off darwin"
    cleared = [before[:3] + [0x200005cb] + before[4:], before]
    assert termios_delta(*cleared, "darwin"), "PENDIN loss accepted"
    for i, value in ((0, 11011), (1, 2), (2, 19201), (3, 0x200005ca), (4, 38400), (5, 38400)):
        assert termios_delta(before, before[:i] + [value] + before[i+1:], "darwin"), ("field change accepted", i)
    assert termios_delta(before, before[:6] + [cc[:5] + [b"\x00"] + cc[6:]], "darwin"), "cc change accepted"
    print("PASS termios restoration self-check")


def assert_termios_restored(fd, before, label):
    after = termios.tcgetattr(fd)
    delta = termios_delta(before, after, sys.platform)
    assert not delta, (label, "termios not restored", sys.platform, delta, before, after)


def run_case(name, action=b"q", sig=None, busy=False, no_color=False, rich=False, tiny=False, injection=False):
    with tempfile.TemporaryDirectory(prefix="codator-watch-pty-") as tmp:
        # The store refuses symlinked ancestors such as macOS /var -> /private/var.
        root = pathlib.Path(tmp).resolve()
        env, log = fixture(root, busy, injection)
        if no_color:
            env["NO_COLOR"] = "1"
        master, slave = pty.openpty()
        cols, rows = (10, 3) if tiny else (180, 40)
        resize(slave, cols, rows)
        before = termios.tcgetattr(slave)
        proc = subprocess.Popen([BINARY, "status", "--watch", "codex"], stdin=slave,
                                stdout=slave, stderr=slave, env=env, close_fds=True)
        all_data = bytearray()
        try:
            all_data.extend(drain(master, 0.5))
            if tiny:
                assert not log.exists(), "tiny view started a probe"
                assert b"Widen" in all_data
                resize(slave, 180, 40)
                proc.send_signal(signal.SIGWINCH)
                all_data.extend(drain(master, 0.5))
            assert log.read_text().splitlines() == ["probe"], (name, "first collection count")
            if rich:
                view = screen(all_data, 180, 40)
                for field in ("╭─ codex", "ACCOUNT", "EMAIL", "5H", "WEEKLY", "CREDITS", "RST", "fixture@example.invalid", "75%", "50%"):
                    assert field in view, (name, "missing rich field", field, view)
                assert SGR.search(all_data), "color missing"
                # Atomic arrow smoke; timed split/150ms policy: TestWatchKeysSplitAndBounded.
                arrow_start = time.monotonic()
                os.write(master, b"\x1b[C")
                all_data.extend(drain(master, 0.3))
                assert proc.poll() is None, ("arrow quit", proc.poll(), round(time.monotonic() - arrow_start, 4), bytes(all_data[-500:]))
                resize(slave, 40, 10)
                proc.send_signal(signal.SIGWINCH)
                narrow = drain(master, 0.3)
                screen(narrow, 40, 10)
                all_data.extend(narrow)
                os.write(master, b"r" * 100 + b"l" * 100 + b"\x1b[6~\x1b[F")
                all_data.extend(drain(master, 0.4))
                resize(slave, 180, 40)
                proc.send_signal(signal.SIGWINCH)
                all_data.extend(drain(master, 0.3))
                assert log.read_text().splitlines() == ["probe"], "redraw/resize/keys reprobed"
                # Approximately one idle frame/second, never a busy redraw loop.
                idle = drain(master, 1.2)
                assert idle.count(b"\x1b[1;1H") <= 2, "idle redraw loop"
                all_data.extend(idle)
            if injection:
                assert b"\x1b[2J" not in all_data and "\u202e".encode() not in all_data
                assert b"fixture?[2J???@example.invalid" in all_data, "injection not sanitized"
            if no_color:
                assert not SGR.search(all_data), "NO_COLOR emitted SGR"
            start = time.monotonic()
            if sig is not None:
                proc.send_signal(sig)
            else:
                os.write(master, action)
            deadline = start + 4
            while proc.poll() is None and time.monotonic() < deadline:
                all_data.extend(drain(master, 0.05))
            assert proc.poll() == 0, (name, "quit/cleanup failed", proc.poll(), bytes(all_data[-500:]))
            all_data.extend(drain(master, 0.1))
            assert b"\x1b[?25h\x1b[?1049l" in all_data, "screen/cursor not restored"
            assert_termios_restored(slave, before, name)
            assert log.read_text().splitlines() == ["probe"], "quit started another probe"
            if rich and (dest := os.environ.get("CODATOR_WATCH_CAPTURE_DIR")):
                path = pathlib.Path(dest) / "pty-rich-ansi.txt"
                path.write_bytes(all_data)
                path.chmod(0o600)
            print("PASS", name)
        finally:
            if proc.poll() is None:
                proc.kill()
            proc.wait(timeout=4)
            os.close(master)
            os.close(slave)


def view_case(no_color=False):
    with tempfile.TemporaryDirectory(prefix="codator-watch-view-pty-") as tmp:
        root=pathlib.Path(tmp).resolve()
        env,log=fixture(root)
        (root / "data/codator/codex/other/native").mkdir(parents=True,mode=0o700)
        if no_color: env["NO_COLOR"]="1"
        master,slave=pty.openpty();resize(slave,180,40);before=termios.tcgetattr(slave)
        proc=subprocess.Popen([BINARY,"status","--watch","codex"],stdin=slave,stdout=slave,stderr=slave,env=env,close_fds=True)
        all_data=bytearray()
        def read(cols=180,rows=40):
            data=drain(master,0.35);all_data.extend(data);return screen(data,cols,rows)
        try:
            view=read()
            assert log.read_text().splitlines()==["probe","probe"],"initial collection"
            assert "Selected codex/other" in view,("initial label selection",view)
            os.write(master,b"]\r")
            wide=read()
            assert "Account detail" in wide and "Account: codex/work" in wide and "ACCOUNT" in wide,("split detail",wide)
            os.write(master,b"\t"+b"l"*20+b"[")
            reveal=read()
            assert "Selected codex/other" in reveal and "ACCOUNT" in reveal,("row/account reveal",reveal)
            os.write(master,b"\t\x1b[6~\x1b[F\x1b[H")
            read()
            resize(slave,40,10);proc.send_signal(signal.SIGWINCH)
            narrow=read(40,10)
            assert "Account detail" in narrow and "codex/other" in narrow,("narrow detail",narrow)
            os.write(master,b"\r/qrhl\x07/WORK\r")
            filtered=read(40,10)
            # Name q/r/h/l are text; cancel restores state and applying is view-only.
            assert proc.poll() is None and "codex/work" in filtered,("entry commands/filter",filtered)
            os.write(master,b"sff")
            read(40,10)
            resize(slave,180,40);proc.send_signal(signal.SIGWINCH)
            empty=read()
            assert "No accounts match filters" in empty,("empty intersecting filters",empty)
            os.write(master,b"/\x15\rffffppr"+b"r"*20)
            read()
            assert log.read_text().splitlines()==["probe","probe"],"view keys/manual gate reprobed"
            os.write(master,b"t")
            timeline=read()
            assert "Reset timeline" in timeline and "window reset" in timeline and "available-at" in timeline and "unknown" in timeline,("timeline",timeline)
            os.write(master,b"a")
            overview=read()
            assert "Availability overview" in overview and "available: 2 (recorded)" in overview and "Fresh launchable: 2" in overview,("overview",overview)
            os.write(master,b"]\r")
            linked=read()
            assert "No accounts match filters" in linked,("empty overview bucket",linked)
            os.write(master,b"a[\r/work\rt")
            filtered_timeline=read()
            assert "codex/work | window reset" in filtered_timeline and "codex/other | window reset" not in filtered_timeline,("timeline filter link",filtered_timeline)
            resize(slave,40,10);proc.send_signal(signal.SIGWINCH)
            read(40,10)
            os.write(master,b"a]\x1b[6~\x1b[F\x1b[H")
            read(40,10)
            os.write(master,b"[\r\r")
            resize(slave,132,18);proc.send_signal(signal.SIGWINCH)
            split=read(132,18)
            assert any("╭" in line[:87] and "╮" in line[:87] for line in split.splitlines()),("short split top not closed",split)
            assert any("╰" in line[:87] and "╯" in line[:87] for line in split.splitlines()),("short split bottom not closed",split)
            assert "…" in split,("clipped detail lacks ellipsis",split)
            # History controls are display-only; progression is injected in Go tests.
            os.write(master,b"g")
            graphs=read(132,18)
            assert "Session sparklines" in graphs and "used percent" in graphs,("graphs",graphs)
            os.write(master,b"c")
            feed=read(132,18)
            assert "Snapshot change feed" in feed and "first observations are baseline-only" in feed,("feed baseline",feed)
            os.write(master,b"c")
            tables=read(132,18)
            assert "ACCOUNT" in tables,("active feed returns tables",tables)
            os.write(master,b"g")
            resize(slave,40,18);proc.send_signal(signal.SIGWINCH)
            graph_narrow=read(40,18)
            assert "Session sparklines" in graph_narrow and "…" in graph_narrow,("narrow graph ellipsis",graph_narrow)
            os.write(master,b"c")
            feed_narrow=read(40,18)
            assert "Snapshot change feed" in feed_narrow and "…" in feed_narrow,("narrow feed ellipsis",feed_narrow)
            os.write(master,b"gc")
            read(40,18)
            resize(slave,132,18);proc.send_signal(signal.SIGWINCH)
            os.write(master,b"r"*50)
            read(132,18)
            assert log.read_text().splitlines()==["probe","probe"],"pair2/resize keys reprobed"
            if no_color: assert not SGR.search(all_data),"view NO_COLOR"
            else: assert SGR.search(all_data),"view color"
            os.write(master,b"\x1b[15~q")
            proc.wait(timeout=4);all_data.extend(drain(master,0.1))
            assert proc.returncode==0,"view exit"
            assert_termios_restored(slave,before,"view restoration")
            assert b"\x1b[?25h\x1b[?1049l" in all_data,"view screen/cursor"
            if dest:=os.environ.get("CODATOR_RICHER_S1_CAPTURE_DIR"):
                label="plain" if no_color else "ansi"
                path=pathlib.Path(dest)/("richer-s1-pty-"+label+".txt");path.write_bytes(all_data);path.chmod(0o600)
            if dest:=os.environ.get("CODATOR_RICHER_S2_CAPTURE_DIR"):
                label="plain" if no_color else "ansi"
                path=pathlib.Path(dest)/("richer-s2-pty-"+label+".txt");path.write_bytes(all_data);path.chmod(0o600)
            if dest:=os.environ.get("CODATOR_RICHER_S3_CAPTURE_DIR"):
                label="plain" if no_color else "ansi"
                path=pathlib.Path(dest)/("richer-s3-pty-"+label+".txt");path.write_bytes(all_data);path.chmod(0o600)
                for mode,text in (("graphs",graphs),("feed",feed),("graphs-narrow",graph_narrow),("feed-narrow",feed_narrow)):
                    path=pathlib.Path(dest)/("richer-s3-pty-screen-"+mode+"-"+label+".txt");path.write_text(text+"\n");path.chmod(0o600)
            print("PASS pair1+pair2+pair3 controls/timeline/overview/link/graphs/feed/closed split/ellipsis/no probes/restoration", "NO_COLOR" if no_color else "ANSI")
        finally:
            if proc.poll() is None: proc.kill()
            proc.wait(timeout=4);os.close(master);os.close(slave)


def rejection_cases():
    with tempfile.TemporaryDirectory(prefix="codator-watch-reject-") as tmp:
        root = pathlib.Path(tmp)
        env = dict(os.environ, XDG_DATA_HOME=str(root / "must-not-create"), TERM="xterm")
        p = subprocess.run([BINARY, "status", "--watch"], input=b"", capture_output=True, env=env)
        assert p.returncode == 1 and b"requires input/output terminals" in p.stderr
        assert not (root / "must-not-create").exists(), "non-TTY touched store"
        master, slave = pty.openpty()
        resize(slave, 80, 24)
        before = termios.tcgetattr(slave)
        for terminal in ("dumb", ""):
            env["TERM"] = terminal
            p = subprocess.Popen([BINARY, "status", "--watch"], stdin=slave, stdout=slave, stderr=slave, env=env)
            p.wait(timeout=2)
            data = drain(master, 0.1)
            assert p.returncode == 1 and b"requires input/output terminals" in data
            assert not (root / "must-not-create").exists() and termios.tcgetattr(slave) == before
        env["TERM"] = "xterm"
        p = subprocess.run([BINARY, "status", "--watch", "--json"], capture_output=True, env=env)
        assert p.returncode == 2 and not (root / "must-not-create").exists()
        os.close(master)
        os.close(slave)
        print("PASS non-TTY/dumb/empty TERM/JSON rejection before store")


if len(sys.argv) > 2 and sys.argv[2] == "unknown-key":
    run_case("unsupported F5 then raw Ctrl-C while probing", action=b"\x1b[15~\x03", busy=True)
elif len(sys.argv)>2 and sys.argv[2]=="view":
    view_case();view_case(no_color=True)
else:
    termios_selfcheck()
    rejection_cases()
    run_case("rich/atomic arrow/scroll/resize/manual/no overlap/idle", rich=True)
    run_case("NO_COLOR", no_color=True)
    run_case("ANSI/control/bidi injection", injection=True)
    run_case("tiny defers collection then resize", tiny=True)
    for name, action in (("q while probing", b"q"), ("Esc while probing", b"\x1b"), ("Ctrl-C while probing", b"\x03")):
        run_case(name, action=action, busy=True)
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP, signal.SIGQUIT):
        run_case(signal.Signals(sig).name + " while probing", sig=sig, busy=True)
    run_case("unsupported F5 then raw Ctrl-C while probing", action=b"\x1b[15~\x03", busy=True)

    view_case()
    view_case(no_color=True)
