#!/usr/bin/env python3
"""A synthetic composer TUI — NOT the real runtime — for tests against a
PRIVATE tmux server (#180). It paints the fence and prompt chrome the driver
classifies, turns on bracketed paste mode (ESC[?2004h), and collapses a
bracketed paste of 5+ lines or over 800 characters into
"[Pasted text #N +M lines]", the shape measured on the runtime.

FAKE_SWALLOW=N ignores the first N submits, modelling a swallowed Enter.
FAKE_LOG=<path> appends every submitted turn, so a test can see exactly what
was submitted and how often.

The next three build a deliberately BROKEN candidate for `colab-fleetd compat`
(#183); every one defaults to today's behaviour, so no other test changes:
FAKE_GLYPH=<char> paints a different prompt glyph than the driver looks for.
FAKE_NO_BRACKET=1 never asks the terminal for bracketed paste (no ESC[?2004h).
FAKE_PLACEHOLDER=1 paints a DIM (SGR 2) placeholder in an empty composer, the way
the real runtime does, which the driver must read as an empty composer.
FAKE_PLACEHOLDER=plain paints the same placeholder WITHOUT dim, which the driver
must (and can only) read as typed text.

The footer always paints the permission-mode row the real runtime paints (#194): the
default mode says "manual mode on", and a launch with --dangerously-skip-permissions
starts in bypass, as a real session does.

FAKE_MODES=1 (#188) makes Shift+Tab (ESC[Z) cycle the mode and repaint that row, the
way the real runtime rewrites its indicator: manual -> accept edits -> plan -> auto
-> manual for a default launch; bypass -> auto -> manual -> accept edits -> plan ->
bypass for a bypass launch (the ring measured on a real build). Off by default, so a
test that does not press the key never sees the mode move."""
import os, sys, tty, termios, codecs, shutil, json
SWALLOW = int(os.environ.get("FAKE_SWALLOW", "0"))
LOG = os.environ.get("FAKE_LOG", "")
GLYPH = os.environ.get("FAKE_GLYPH", "❯")
NO_BRACKET = os.environ.get("FAKE_NO_BRACKET", "") == "1"
PLACEHOLDER = os.environ.get("FAKE_PLACEHOLDER", "")
MODES = os.environ.get("FAKE_MODES", "") == "1"
# The footer rows are what a real runtime build (2.1.282) painted after each press
# of Shift+Tab, measured in #194. The default mode is NOT a bare "? for shortcuts":
# it says "manual mode on", and the "(shift+tab to cycle)" hint appears only in
# the other modes.
MODE_ROWS = {
    "bypass": "\u23f5\u23f5 bypass permissions on (shift+tab to cycle) \u00b7 \u2190 for agents",
    "auto": "\u23f5\u23f5 auto mode on (shift+tab to cycle) \u00b7 \u2190 for agents",
    "manual": "\u23f8 manual mode on \u00b7 ? for shortcuts \u00b7 \u2190 for agents",
    "accept": "\u23f5\u23f5 accept edits on (shift+tab to cycle) \u00b7 \u2190 for agents",
    "plan": "\u23f8 plan mode on (shift+tab to cycle) \u00b7 \u2190 for agents",
}
# FAKE_MODE_WORDING=reworded models a candidate build that rewords every indicator
# row (#194): the wording the mode reader matches is gone, so a session reads as
# an indicator area naming no known mode.
if os.environ.get("FAKE_MODE_WORDING", "") == "reworded":
    MODE_ROWS = {k: v.replace(" on", " active") for k, v in MODE_ROWS.items()}
IMPORTS = os.environ.get("FAKE_IMPORTS", "")
IMPORTS_KEY = os.environ.get("FAKE_IMPORTS_KEY", "hasClaudeMdExternalIncludesApproved")

def external_imports():
    """The files this directory's CLAUDE.md imports from outside it."""
    cwd = os.getcwd()
    try:
        with open(os.path.join(cwd, "CLAUDE.md")) as f:
            lines = f.read().splitlines()
    except OSError:
        return []
    paths = [l[1:].strip() for l in lines if l.startswith("@")]
    return [p for p in paths if os.path.isabs(p) and not p.startswith(cwd + os.sep)]

def imports_question_open():
    """True when the runtime would ask whether it may follow those imports."""
    if not IMPORTS or not external_imports():
        return False
    try:
        with open(os.environ.get("FAKE_STATE", "")) as f:
            projects = json.load(f).get("projects", {})
    except (OSError, ValueError):
        projects = {}
    cwd = os.getcwd()
    for key in {cwd, os.path.realpath(cwd)}:
        if (projects.get(key) or {}).get(IMPORTS_KEY) is True:
            return False
    return True

def render_imports_dialog():
    cols, lines = shutil.get_terminal_size((80, 24))
    if IMPORTS == "reworded":
        options = ["Refuse to follow them", "Follow them"]
    else:
        options = ["No, disable external imports", "Yes, allow external imports"]
    sel = 1 if IMPORTS == "allow-highlighted" else 0
    body = ["", "─" * cols, "  Allow external CLAUDE.md file imports?", "",
            "  This project's CLAUDE.md imports files outside the current working directory.", "",
            "  External imports:"] + ["    " + p for p in external_imports()] + [""]
    for i, o in enumerate(options):
        body.append(("  \u276f " if i == sel else "    ") + o)
    body += ["", "  Enter to confirm \u00b7 Esc to cancel"]
    room = lines - len(body)
    frame = [""] * max(0, room) + body
    os.write(1, ("\x1b[H\x1b[2J" + "\r\n".join(frame[-lines:])).encode())

BYPASS = "--dangerously-skip-permissions" in sys.argv[1:]
# The measured ring. A launch without bypass never lands on it.
RING = ["bypass", "auto", "manual", "accept", "plan"]
if not BYPASS:
    RING = RING[1:]
mode = 0 if BYPASS else RING.index("manual")
transcript = ["fake tui ready (synthetic, not the real runtime)"]
buf = ""
pastes = {}
n_paste = 0

def render():
    cols, lines = shutil.get_terminal_size((80, 24))
    width = cols - 4
    rows = []
    for logical in (buf.split("\n") if buf else [""]):
        while len(logical) > width:
            rows.append(logical[:width]); logical = logical[width:]
        rows.append(logical)
    comp = [GLYPH + " " + rows[0]] + ["  " + r for r in rows[1:]]
    if PLACEHOLDER and not buf:
        dim_on, dim_off = ("\x1b[2m", "\x1b[0m") if PLACEHOLDER == "1" else ("", "")
        comp = [GLYPH + "\u00a0" + dim_on + 'Try "synthetic"' + dim_off]
    rule = "─" * cols
    footer = "  " + MODE_ROWS[RING[mode]]
    tail = [rule] + comp + [rule, footer]
    room = lines - len(tail)
    head = transcript[-room:] if room > 0 else []
    frame = head + [""] * max(0, room - len(head)) + tail
    os.write(1, ("\x1b[H\x1b[2J" + "\r\n".join(frame[-lines:])).encode())

def submit():
    global buf, SWALLOW
    if SWALLOW > 0:
        SWALLOW -= 1
        return
    text = buf
    for k, v in pastes.items():
        text = text.replace(k, v)
    if LOG:
        with open(LOG, "a") as f:
            f.write(repr(text.strip()) + "\n")
    transcript.append("⏺ ACK (fake) for a %d-byte turn" % len(text.strip()))
    buf = ""

def main():
    global buf, n_paste, mode
    fd = 0
    old = termios.tcgetattr(fd)
    tty.setraw(fd)
    if not NO_BRACKET:
        os.write(1, b"\x1b[?2004h")
    dec = codecs.getincrementaldecoder("utf-8")()
    pending = ""
    in_paste = False
    pbuf = ""
    try:
        if imports_question_open():
            # The dialog is never answered: it stays until the process is killed.
            render_imports_dialog()
            while True:
                chunk = os.read(fd, 65536)
                if not chunk or b"\x03" in chunk:
                    return
                render_imports_dialog()
        render()
        while True:
            chunk = os.read(fd, 65536)
            if not chunk:
                break
            s = pending + dec.decode(chunk)
            pending = ""
            i = 0
            while i < len(s):
                if in_paste:
                    end = s.find("\x1b[201~", i)
                    if end < 0:
                        pbuf += s[i:]; i = len(s); break
                    pbuf += s[i:end]; i = end + 6; in_paste = False
                    if pbuf.count("\n") >= 4 or len(pbuf) > 800:
                        n_paste += 1
                        k = "[Pasted text #%d +%d lines]" % (n_paste, pbuf.count("\n"))
                        pastes[k] = pbuf
                        buf += k
                    else:
                        buf += pbuf
                    pbuf = ""
                    continue
                if s.startswith("\x1b[200~", i):
                    in_paste = True; i += 6; continue
                c = s[i]
                if MODES and s.startswith("\x1b[Z", i):
                    mode = (mode + 1) % len(RING)
                    i += 3; continue
                if c == "\x1b":
                    if len(s) - i < 6 and "\x1b[200~".startswith(s[i:]):
                        pending = s[i:]; break
                    j = i + 1
                    if j < len(s) and s[j] in "[O":
                        j += 1
                        while j < len(s) and not (s[j].isalpha() or s[j] == "~"):
                            j += 1
                    i = j + 1; continue
                if c == "\r":
                    submit()
                elif c == "\x15":
                    buf = ""
                elif c == "\x7f":
                    buf = buf[:-1]
                elif c == "\x03":
                    return
                elif c >= " ":
                    buf += c
                i += 1
            render()
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)

main()
