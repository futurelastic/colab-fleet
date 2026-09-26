#!/usr/bin/env python3
"""A synthetic multi-select question dialog — NOT the real runtime — for tests
against a PRIVATE tmux server (#219). It paints the dialog the way the runtime
was measured painting one whose question does not fit a row: a tab bar, then the
question as a block with a `│ ` rail down its left edge, then four boxed options
with a description under each, the free-text row, the unnumbered Submit row and
the chat row.

FAKE_QUESTION_ROWS=N is how many rows the wrapped question takes (default 10, which
with the rest of the dialog puts the tab bar more than 24 rows above the bottom row).
FAKE_LOG=<path> receives one JSON line per dialog outcome, so a test can see exactly
what was answered: {"answer": [ticked labels]} once the review screen is confirmed,
{"answer": null} if it is cancelled.

Keys, as measured for #176: a digit flips that box (the highlight does not move);
Up/Down walk the highlight over the boxes, the free-text row, Submit and the chat
row; Right on a box moves to the review screen, ticks kept; on the review screen
1 hands the answers over and 2 cancels."""
import os, sys, tty, termios, json

ROWS = int(os.environ.get("FAKE_QUESTION_ROWS", "10"))
LOG = os.environ.get("FAKE_LOG", "")
LABELS = ["Alpha", "Bravo", "Charlie", "Delta"]
RULE = "─" * 100

ticks = [False] * len(LABELS)
sel = 1  # 1..4 boxes, 5 free text, 6 Submit row, 7 chat row
stage = 0  # 0 question, 1 review, 2 answered


def rows():
    if stage == 2:
        return ["  answered"]
    out = ["  transcript line", RULE]
    out.append("←  %s Q1 pick  ✔ Submit  →" % ("☒" if any(ticks) else "☐"))
    out.append("")
    if stage == 1:
        picked = ", ".join(l for l, t in zip(LABELS, ticks) if t) or "(none)"
        out += ["Review your answers", "", " ● Which of these do you want?", "   → " + picked, "",
                "Ready to submit your answers?", "", "❯ 1. Submit answers", "  2. Cancel"]
        return out
    for i in range(1, ROWS + 1):
        out.append("│ row %d of a question long enough that the runtime wraps it" % i)
    out.append("")
    mark = lambda n: "❯ " if n == sel else "  "
    for i, l in enumerate(LABELS, 1):
        out.append("%s%d. [%s] %s" % (mark(i), i, "✔" if ticks[i - 1] else " ", l))
        out.append("         a description of " + l)
    n = len(LABELS)
    out.append("%s%d. [ ] Type something" % (mark(n + 1), n + 1))
    out.append("%s   Submit" % mark(n + 2))
    out.append(RULE)
    out.append("%s%d. Chat about this" % (mark(n + 3), n + 2))
    out.append("")
    out.append("Enter to select · ↑/↓ to navigate · Esc to cancel")
    return out


def paint():
    sys.stdout.write("\x1b[2J\x1b[H" + "\r\n".join(rows()))
    sys.stdout.flush()


def log(answer):
    if LOG:
        with open(LOG, "a") as f:
            f.write(json.dumps({"answer": answer}) + "\n")


def key():
    b = os.read(0, 16)
    return b.decode("utf-8", "replace")


fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
try:
    tty.setraw(fd)
    paint()
    while True:
        k = key()
        if not k:
            break
        n = len(LABELS)
        if stage == 0:
            if len(k) == 1 and k in "1234" and sel <= n:
                ticks[int(k) - 1] = not ticks[int(k) - 1]
            elif k == "\x1b[A" and sel > 1:
                sel -= 1
            elif k == "\x1b[B" and sel < n + 3:
                sel += 1
            elif k == "\x1b[C" and sel <= n:
                stage = 1
        elif stage == 1:
            if k == "1":
                log([l for l, t in zip(LABELS, ticks) if t])
                stage = 2
            elif k == "2":
                log(None)
                stage = 2
        paint()
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
