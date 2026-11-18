#!/usr/bin/env python3
"""Generate site/assets/demo.svg: lockmq's value in one looping image.

An agent (left) asks lockmq about a RabbitMQ broker; the human answers in `lox run` (right), in a
terminal of their own. Pure SVG + CSS keyframes, so it plays on GitHub, on the site and in any
browser. Every animation shares one clock of LOOP seconds.

Nothing on the two terminals is typed here: it is read from a recording of one real run, kept in
scripts/brand/demo/ (recorded 2026-11-18 with lockmq v0.10.0, lox v0.12.1 and RabbitMQ 4.4.0).

  demo/frames/*.ansi  the `lox run` terminal (106x22), captured with `tmux capture-pane -e` at six
                      moments of the run: the console's password question, the password typed,
                      the lock serving, the connect question, the connection approved, the
                      stream read approved by a rule.
  demo/agent.json     the agent's side of the same run (Claude Code, headless): its prompt, the
                      lockmq_queues and lockmq_read calls with their results as the MCP server
                      returned them, and its answer. "calls_not_shown" names the calls of the run
                      this image leaves out.

What this script adds: the layout, the two window frames, the captions under them, the dots that
mark a call as running or done, and the cuts ("…"). To record again, see the demo's pull request
(lockmq#76) for the fixture; then

    python3 scripts/brand/demo.py > site/assets/demo.svg
"""
import json
import pathlib
import re
import textwrap

HERE = pathlib.Path(__file__).parent / "demo"
LOOP = 20.0
C = dict(bg="#1a1814", pane="#0e0d0a", bar="#24211b", border="#3a352c", text="#e2e8f0",
         dim="#7c8799", sub="#94a3b8", orange="#fb923c", teal="#5eead4", violet="#a78bfa",
         red="#f87171", green="#4ade80", yellow="#facc15", magenta="#f0abfc")
FONT = "'JetBrains Mono','SF Mono',Menlo,Consolas,'DejaVu Sans Mono',monospace"
SANS = "Inter,-apple-system,'Segoe UI',system-ui,Helvetica,Arial,'DejaVu Sans',sans-serif"

css, body = [], []
n = 0


def p(t):
    return f"{t / LOOP * 100:.2f}%"


def anim(frames):
    """Register keyframes [(time_s, 'css decls'), ...] and return a class name."""
    global n
    n += 1
    name = f"k{n}"
    rules = " ".join(f"{p(t)}{{{d}}}" for t, d in frames)
    css.append(f"@keyframes {name}{{{rules}}}")
    css.append(f".{name}{{animation:{name} {LOOP}s linear infinite}}")
    return name


END = LOOP - 0.6


def show(t_in, t_out=END, fade=0.2):
    """Opacity window: hidden, visible from t_in to t_out."""
    frames = [(0, "opacity:0")]
    if t_in > 0:
        frames += [(t_in, "opacity:0")]
        frames += [(t_in + fade, "opacity:1")]
    else:
        frames = [(0, "opacity:1")]
    frames += [(t_out, "opacity:1"), (t_out + fade, "opacity:0"), (LOOP, "opacity:0")]
    return anim(frames)


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def text(x, y, s, cls="", size=12.5, col=C["text"], weight=400, family=FONT, anchor="start"):
    c = f' class="{cls}"' if cls else ""
    return (f'<text x="{x}" y="{y}"{c} font-family="{family}" font-size="{size}" '
            f'fill="{col}" font-weight="{weight}" text-anchor="{anchor}" xml:space="preserve">{esc(s)}</text>')


def spans(x, y, parts, cls="", size=12.5):
    """One line of coloured runs [(text, colour), ...]."""
    c = f' class="{cls}"' if cls else ""
    runs = "".join(f'<tspan fill="{col}">{esc(s)}</tspan>' for s, col in parts)
    return (f'<text x="{x}" y="{y}"{c} font-family="{FONT}" font-size="{size}" '
            f'xml:space="preserve">{runs}</text>')


def pane(x, y, w, h, title, account):
    body.append(f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="12" fill="{C["pane"]}" '
                f'stroke="{C["border"]}" stroke-width="1.5"/>')
    body.append(f'<path d="M{x} {y + 12} a12 12 0 0 1 12 -12 h{w - 24} a12 12 0 0 1 12 12 v18 h-{w} z" '
                f'fill="{C["bar"]}"/>')
    for i, col in enumerate(("#f87171", "#facc15", "#4ade80")):
        body.append(f'<circle cx="{x + 18 + i * 16}" cy="{y + 15}" r="5" fill="{col}" fill-opacity=".7"/>')
    body.append(text(x + 70, y + 20, title, size=12, col=C["sub"], family=SANS, weight=600))
    body.append(text(x + w - 14, y + 20, account, size=12, col=C["dim"], family=SANS, anchor="end"))


# --- The recorded lox run terminal ----------------------------------------------------------
SGR_FG = {30: "#0e0d0a", 31: C["red"], 32: C["green"], 33: C["yellow"], 36: C["teal"], 95: C["magenta"]}
SGR_BG = {42: C["green"]}
WIDE = {"\U0001F514"}  # the bell takes two cells
BOX = "─│┌┐└┘"


def cells(line):
    """One captured line as cells [(char, fg, bg, bold, dim, reverse)], one per terminal column."""
    out = []
    fg = bg = None
    bold = dim = rev = False
    pos = 0
    for m in re.finditer(r"\x1b\[([0-9;]*)m", line):
        for ch in line[pos:m.start()]:
            out.append((ch, fg, bg, bold, dim, rev))
            if ch in WIDE:
                out.append(("", fg, bg, bold, dim, rev))
        pos = m.end()
        for code in (int(c) for c in (m.group(1) or "0").split(";")):
            if code == 0:
                fg = bg = None
                bold = dim = rev = False
            elif code == 1:
                bold = True
            elif code == 2:
                dim = True
            elif code == 7:
                rev = True
            elif code == 39:
                fg = None
            elif code == 49:
                bg = None
            elif code in SGR_FG:
                fg = SGR_FG[code]
            elif code in SGR_BG:
                bg = SGR_BG[code]
            else:
                raise SystemExit(f"demo.py: SGR code {code} of the recording has no colour here")
    for ch in line[pos:]:
        out.append((ch, fg, bg, bold, dim, rev))
        if ch in WIDE:
            out.append(("", fg, bg, bold, dim, rev))
    return out


def terminal(path, x0, y0, cw, lh, size):
    """A captured frame as SVG. Box-drawing characters become strokes and every run of text is
    placed at its own column, so the frame keeps its grid whatever monospace font renders it."""
    out = []
    for row, line in enumerate(path.read_text().split("\n")):
        cs = cells(line)
        base = y0 + row * lh
        mid = base - size * 0.32
        col = 0
        while col < len(cs):
            ch, fg, bg, bold, dim, rev = cs[col]
            end = col + 1
            # a run: same style, and either all box-drawing of one kind, or plain text
            while end < len(cs) and cs[end][1:] == cs[col][1:] and (
                    (ch == "─" and cs[end][0] == "─") or
                    (ch not in BOX and cs[end][0] not in BOX and ch != " " and cs[end][0] != " "
                     and ch.isascii() and cs[end][0].isascii()) or
                    (ch == " " and cs[end][0] == " ")):
                end += 1
            x = x0 + col * cw
            w = (end - col) * cw
            colour = fg or (C["sub"] if dim else C["text"])
            if dim and fg:
                op = ' fill-opacity=".75"'
            else:
                op = ""
            if rev:
                out.append(f'<rect x="{x:.1f}" y="{base - size:.1f}" width="{w:.1f}" height="{lh - 2}" fill="{C["text"]}"/>')
                colour = C["pane"]
            elif bg:
                out.append(f'<rect x="{x:.1f}" y="{base - size:.1f}" width="{w:.1f}" height="{lh - 2}" fill="{bg}"/>')
            stroke = f'stroke="{colour}" stroke-width="1.2" fill="none"' + (' stroke-opacity=".55"' if dim else "")
            cx = x + cw / 2
            if ch == "─":
                out.append(f'<path d="M{x:.1f} {mid:.1f}h{w:.1f}" {stroke}/>')
            elif ch == "│":
                out.append(f'<path d="M{cx:.1f} {mid - lh / 2:.1f}v{lh}" {stroke}/>')
            elif ch in "┌┐└┘":
                dx = cw / 2 if ch in "┌└" else -cw / 2
                dy = lh / 2 if ch in "┌┐" else -lh / 2
                out.append(f'<path d="M{cx + dx:.1f} {mid:.1f}h{-dx:.1f}v{dy:.1f}" {stroke}/>')
            elif ch.strip():
                s = "".join(c[0] for c in cs[col:end])
                wt = ' font-weight="700"' if bold else ""
                out.append(f'<text x="{x:.1f}" y="{base:.1f}" fill="{colour}"{op}{wt}>{esc(s)}</text>')
            col = end
    return "".join(out)


W, H = 1200, 590
body.append(f'<rect width="{W}" height="{H}" rx="24" fill="{C["bg"]}"/>')
PY, PH = 28, 444

# Timeline (seconds).
T_TYPED, T_SERVING = 1.3, 2.6          # lox run: the password typed, then the lock serving
T_PROMPT, T_QUEUES = 3.2, 4.2          # agent: the prompt, the lockmq_queues call
T_ASK, T_YES, T_CONNECTED = 4.7, 7.4, 7.8  # lox run: the connect question, y, connected
T_QUEUES_OUT = 8.2
T_READ, T_AUTO, T_READ_OUT = 9.6, 10.2, 10.8
T_ANSWER = 13.2

# --- Agent pane (left) ----------------------------------------------------------------------
rec = json.loads((HERE / "agent.json").read_text())
queues, read = rec["calls"]
assert queues["tool"] == "lockmq_queues" and read["tool"] == "lockmq_read"
AX, AW = 16, 448
pane(AX, PY, AW, PH, "✻ Claude Code", "the agent's session")
AS, LH = 11.5, 17
ax, ay = AX + 14, PY + 54
COLS = 60


def args(d, keep):
    return ", ".join(f"{k}: {json.dumps(d[k])}" for k in keep)


row = 0


def put(parts, cls):
    global row
    body.append(spans(ax, ay + row * LH, parts, cls=cls, size=AS))
    row += 1


def running(t0, t1):
    """The dot of a tool call: dim while it runs, green once it answered."""
    y = ay + row * LH
    body.append(text(ax, y, "●", cls=show(t0, t1, fade=0.1), size=AS, col=C["dim"]))
    body.append(text(ax, y, "●", cls=show(t1 + 0.1), size=AS, col=C["green"]))


for i, s in enumerate(textwrap.wrap(rec["prompt"], COLS - 2)):
    put([("> " if i == 0 else "  ", C["dim"]), (s, C["text"])], show(T_PROMPT))
row += 1
running(T_QUEUES, T_QUEUES_OUT)
put([("  ", C["text"]), (queues["tool"], C["text"]), ("(" + args(queues["input"], ["vhost", "sort"]) + ")", C["sub"])],
    show(T_QUEUES))
items = queues["result"]["data"]["items"]
wn = max(len(q["name"]) for q in items)
for i, q in enumerate(items):
    line = (f'{q["name"]:<{wn}}  {q["type"]:<7}  messages_ready {q["messages_ready"]:<4}  '
            f'consumers {q["consumers"]}')
    put([("  └ " if i == 0 else "    ", C["dim"]), (line, C["sub"])], show(T_QUEUES_OUT))
row += 1
running(T_READ, T_READ_OUT)
put([("  ", C["text"]), (read["tool"], C["text"]),
     ("(" + args(read["input"], ["vhost", "name"]) + ",", C["sub"])], show(T_READ))
put([("    " + args(read["input"], ["from", "count"]) + ", …)", C["sub"])], show(T_READ))
msgs = read["result"]["data"]["messages"]
first = json.dumps(msgs[0]["body"], separators=(",", ":"))
lines = [""]
for piece in re.findall(r'[^,]*,?', first):  # wrapped after a comma, nothing else changed
    if lines[-1] and len(lines[-1]) + len(piece) > COLS - 4:
        lines.append("")
    lines[-1] += piece
assert "".join(lines) == first
for i, s in enumerate(lines):
    parts = [("  └ " if i == 0 else "    ", C["dim"])]
    for k, piece in enumerate(re.split(r'("<redacted:[^"]*>")', s)):
        if piece:
            parts.append((piece, C["teal"] if k % 2 else C["text"]))
    put(parts, show(T_READ_OUT))
put([("    … +" + str(len(msgs) - 1) + " messages", C["dim"])], show(T_READ_OUT))
row += 1
for i, s in enumerate(textwrap.wrap(rec["answer"], COLS - 2)):
    put([("● " if i == 0 else "  ", C["text"]), (s, C["text"])], show(T_ANSWER))

# --- lox run pane (right) -------------------------------------------------------------------
CX = AX + AW + 16
CW = W - 16 - CX
pane(CX, PY, CW, PH, "lox run", "your terminal, outside the agent")
TS, TLH = 10.6, 17.6
TCW = TS * 0.602
tx, ty = CX + (CW - 106 * TCW) / 2, PY + 54
shots = [("1-password", 0, T_TYPED), ("2-password-typed", T_TYPED, T_SERVING),
         ("3-serving", T_SERVING, T_ASK), ("4-connect-question", T_ASK, T_CONNECTED),
         ("5-connected", T_CONNECTED, T_AUTO), ("6-read-auto", T_AUTO, END)]
for name, t0, t1 in shots:
    body.append(f'<g class="{show(t0, t1 - 0.12, fade=0.12)}" font-family="{FONT}" font-size="{TS}" '
                f'xml:space="preserve">{terminal(HERE / "frames" / (name + ".ansi"), tx, ty, TCW, TLH, TS)}</g>')
# The key the human presses on the connect question.
body.append(f'<g class="{show(T_YES - 0.9, T_CONNECTED - 0.12, fade=0.1)}">'
            f'<rect x="{CX + CW - 150}" y="{ty + 15 * TLH - 16:.1f}" width="118" height="24" rx="6" fill="{C["green"]}"/>'
            + text(CX + CW - 91, ty + 15 * TLH, "you press y", size=12.5, col=C["pane"], weight=700,
                   family=SANS, anchor="middle") + "</g>")

# Captions.
caps = [(0.0, T_PROMPT, "You type the broker password in lox run. The agent never holds it."),
        (T_PROMPT, T_QUEUES_OUT + 0.9, "The first connection waits for your answer, in lox run."),
        (T_QUEUES_OUT + 0.9, 12.4, "A stream read passes on the default rules, and shows in the feed."),
        (12.4, END, "The messages come back with the customer's email masked.")]
for t0, t1, s in caps:
    body.append(text(W / 2, PY + PH + 70, s, cls=show(t0 + 0.05, t1 - 0.2), size=25, weight=650,
                     family=SANS, anchor="middle"))

label = ("lockmq demo, four steps. 1: in lox run, the human types the broker password the lock's console asks for. "
         "2: an agent (here Claude Code) calls lockmq_queues; lox run shows the question \"connect on dev/dev\" "
         "and the human presses y. 3: the agent gets the three queues of the vhost orders, then calls lockmq_read on "
         "the stream order-events; lox run approves it by a rule and lists it in its feed as auto:action. "
         "4: the agent gets the messages with the customer's email replaced by <redacted:r1.1>.")
print(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
      f'role="img" aria-label="{esc(label).replace(chr(34), "&quot;")}">')
print("<title>lockmq: your agent reads the queue, you hold the key</title>")
print("<style>" + "\n".join(css) +
      "\n@media (prefers-reduced-motion: reduce){*{animation-delay:-16s!important;"
      "animation-play-state:paused!important}}</style>")
print("\n".join(body))
print("</svg>")
