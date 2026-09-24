#!/usr/bin/env python3
"""A synthetic composer TUI — NOT the real runtime — for tests against a
PRIVATE tmux server (#180). It paints the fence and prompt chrome the driver
classifies, turns on bracketed paste mode (ESC[?2004h), and collapses a
bracketed paste of 5+ lines or over 800 characters into
"[Pasted text #N +M lines]", the shape measured on the runtime.

FAKE_SWALLOW=N ignores the first N submits, modelling a swallowed Enter.
FAKE_LOG=<path> appends every submitted turn, so a test can see exactly what
was submitted and how often."""
import os, sys, tty, termios, codecs, shutil
SWALLOW = int(os.environ.get("FAKE_SWALLOW", "0"))
LOG = os.environ.get("FAKE_LOG", "")
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
    comp = ["❯ " + rows[0]] + ["  " + r for r in rows[1:]]
    rule = "─" * cols
    tail = [rule] + comp + [rule, "  ? for shortcuts"]
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
    global buf, n_paste
    fd = 0
    old = termios.tcgetattr(fd)
    tty.setraw(fd)
    os.write(1, b"\x1b[?2004h")
    dec = codecs.getincrementaldecoder("utf-8")()
    pending = ""
    in_paste = False
    pbuf = ""
    try:
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
