#!/usr/bin/env python3
"""NOT the real runtime (#180 H2): paints a composer-shaped frame (rule,
prompt glyph, rule, status row) and exits without clearing it — the screen a
pane shows once a TUI has exited back to the shell that started it."""
import os, shutil
cols, lines = shutil.get_terminal_size((80, 24))
rule = "\u2500" * cols
tail = [rule, "\u276f ", rule, "  ? for shortcuts"]
head = ["fake tui (synthetic, exited)", "\u23fa earlier synthetic output"]
frame = head + [""] * (lines - 1 - len(head) - len(tail)) + tail
os.write(1, ("\x1b[H\x1b[2J" + "\r\n".join(frame) + "\r\n").encode())
