#!/usr/bin/env python3
"""Generate site/assets/demo.svg: lockmq's value in one looping image.

Claude Code (left, the agent's account) lists the queues of a vhost: a catalogue call, answered
at once. It then asks to read the last messages of a stream: the lockmq console (right, its own
account) shows the approval screen, the human types y, and the agent gets the messages back with
the customer's email replaced by a reference. Pure SVG + CSS keyframes, so it plays on GitHub, on
the site and in any browser. Every animation shares one clock of LOOP seconds.

The console lines follow the real output of lockmq and lockkit (approval frame, screen body,
PII line); keep them in step when those change.

    python3 scripts/brand/demo.py > site/assets/demo.svg
"""

LOOP = 14.0
C = dict(bg="#0b1120", pane="#020617", bar="#0f172a", border="#253047", text="#e2e8f0",
         dim="#64748b", sub="#94a3b8", orange="#fb923c", teal="#5eead4", violet="#a78bfa",
         red="#f87171", green="#4ade80", yellow="#facc15")
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
    return anim([(0, "opacity:0"), (t_in, "opacity:0"), (t_in + fade, "opacity:1"),
                 (t_out, "opacity:1"), (t_out + fade, "opacity:0"), (LOOP, "opacity:0")])


def move(t0, t1, x0, y0, x1, y1):
    return anim([(0, f"transform:translate({x0}px,{y0}px);opacity:0"),
                 (t0, f"transform:translate({x0}px,{y0}px);opacity:0"),
                 (t0 + 0.05, f"transform:translate({x0}px,{y0}px);opacity:1"),
                 (t1, f"transform:translate({x1}px,{y1}px);opacity:1"),
                 (t1 + 0.15, f"transform:translate({x1}px,{y1}px);opacity:0"),
                 (LOOP, f"transform:translate({x1}px,{y1}px);opacity:0")])


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


W, H = 1200, 640
body.append(f'<rect width="{W}" height="{H}" rx="24" fill="{C["bg"]}"/>')

# --- Agent pane (left) --------------------------------------------------------------------
AX, AY, AW, AH = 24, 32, 500, 516
pane(AX, AY, AW, AH, "✻ Claude Code", "account alice")
LH = 19
ax, ay = AX + 16, AY + 58
agent = [
    (0.3, [("> ", C["dim"]), ("which queues in orders have no consumer?", C["text"])]),
    (1.1, [("● ", C["green"]), ("lockmq_queues", C["text"]), ('(vhost: "orders")', C["sub"])]),
    (2.1, [("  └ ", C["dim"]), ("orders.failed   quorum   ready 1204  consumers 0", C["sub"])]),
    (2.1, [("    ", C["dim"]), ("orders.retry    classic  ready 0     consumers 1", C["sub"])]),
    (2.1, [("    ", C["dim"]), ("order-events    stream             consumers 2", C["sub"])]),
    (2.8, [("● ", C["text"]), ("orders.failed has no consumer: 1204 waiting.", C["text"])]),
    (2.8, [("  ", C["text"]), ("Reading the last order events to see why.", C["text"])]),
    (3.7, [("● ", C["green"]), ("lockmq_read", C["text"]),
           ('(name: "order-events", from: "last",', C["sub"])]),
    (3.7, [("             ", C["text"]), ("count: 3)", C["sub"])]),
]
for i, (t, parts) in enumerate(agent):
    body.append(spans(ax, ay + i * LH, parts, cls=show(t)))
row = len(agent)
body.append(spans(ax, ay + row * LH, [("  └ ", C["dim"]), ("waiting for approval in the lockmq console…",
                                                            C["yellow"])], cls=show(4.0, 7.0)))
reply = [
    (7.3, [("  └ ", C["dim"]), ("3 messages · untrusted data from the broker", C["sub"])]),
    (7.5, [("    ", C["dim"]), ('{"order_id": "A-1042", "status": "payment_failed",', C["text"])]),
    (7.5, [("    ", C["dim"]), (' "customer": {"email": ', C["text"]), ('"<redacted:r1.1>"', C["teal"]),
           ("},", C["text"])]),
    (7.5, [("    ", C["dim"]), (' "reason": "card_declined"}', C["text"])]),
    (7.6, [("    ", C["dim"]), ("… 2 more, same reason", C["dim"])]),
    (8.6, [("● ", C["text"]), ("The last 3 orders failed on card_declined, and", C["text"])]),
    (8.6, [("  ", C["text"]), ("nothing consumes orders.failed. I never saw the", C["text"])]),
    (8.6, [("  ", C["text"]), ("customers' emails, only references.", C["text"])]),
]
for i, (t, parts) in enumerate(reply):
    body.append(spans(ax, ay + (row + i) * LH, parts, cls=show(t)))

# --- Console pane (right) -----------------------------------------------------------------
CX, CY, CW, CH = 548, 32, 628, 516
pane(CX, CY, CW, CH, "lockmq console", "account lockmq · its own session")
cx, cy = CX + 16, CY + 58
con = [
    (0.0, [("$ ", C["dim"]), ("lockmq console --profile dev --project ~/shop", C["text"])]),
    (0.0, [("✓ ", C["green"]), ("profile ", C["text"]), ("dev", C["text"]),
           (" · mq.example.com:15671 ", C["text"]), ("(approved policy)", C["dim"])]),
    (0.0, [("", C["text"])]),
    (0.0, [("› ", C["orange"]), ("Listening on /run/lockmq/3f9a0c1d-dev.sock", C["text"])]),
]
CS = 12  # console font size; one character is 0.602 em wide
CHW = CS * 0.602
for i, (t, parts) in enumerate(con):
    body.append(spans(cx, cy + i * LH, parts, size=CS))
V = C["violet"]
screen = [
    [("╭─ ", V), ("DEV", V), (" · mq.example.com:15671 / orders · user lockmq-ro · tier read ", C["text"]),
     ("─────", V)],
    [("│ ", V), ('requested by uid 1000 (alice) · pid 48211 · process "lockmq" (self-reported name)', C["dim"])],
    [("│ ", V), ("agent: see why orders end in orders.failed", C["dim"])],
    [("│", V)],
    [("│ ", V), ("READ stream order-events  from last  max 3", C["text"])],
    [("│", V)],
    [("│ ", V), ("stream: first offset 0 · last 18342 · via stream protocol", C["text"])],
    [("│ ", V), ("no offset stored · no message removed", C["text"])],
    [("│ ", V), ("PII: masked (1 mask rules, 0 allow rules; body detect; detectors: card, email)",
              C["text"])],
]
srow = len(con) + 1
g = [f'<g class="{show(4.4)}">']
for i, parts in enumerate(screen):
    g.append(spans(cx, cy + (srow + i) * LH, parts, size=CS))
g.append("</g>")
body.append("".join(g))
prow = srow + len(screen)
body.append(spans(cx, cy + prow * LH, [("╰─ ", V), ("Approve? [y/N] ", C["text"])], cls=show(4.6), size=CS))
# The human types y.
body.append(text(cx + CHW * 18, cy + prow * LH, "y", cls=show(6.2, fade=0.05), size=CS,
                 col=C["green"], weight=700))
# Cursor before the answer.
blink = anim([(0, "opacity:0"), (4.7, "opacity:0"), (4.71, "opacity:1"), (5.2, "opacity:1"),
              (5.21, "opacity:0"), (5.6, "opacity:0"), (5.61, "opacity:1"), (6.19, "opacity:1"),
              (6.2, "opacity:0"), (LOOP, "opacity:0")])
body.append(f'<rect x="{cx + CHW * 18:.1f}" y="{cy + prow * LH - 11}" width="{CHW:.1f}" height="14" '
            f'fill="{C["text"]}" class="{blink}"/>')
# Key hint, lit while the console waits.
body.append(f'<g class="{show(4.8, 6.2, fade=0.1)}"><rect x="{CX + CW - 196}" y="{cy + prow * LH - 15}" '
            f'width="180" height="22" rx="6" fill="{C["violet"]}"/>'
            + text(CX + CW - 106, cy + prow * LH, "only you can answer", size=12, col=C["pane"],
                   weight=700, family=SANS, anchor="middle") + "</g>")

# The broker, behind the console only.
BX, BY = CX + 16, CY + CH - 74
body.append(f'<rect x="{BX}" y="{BY}" width="{CW - 32}" height="58" rx="10" fill="{C["bar"]}" '
            f'stroke="{C["orange"]}" stroke-width="1.5" stroke-dasharray="6 5"/>')
body.append(text(BX + 16, BY + 25, "RabbitMQ", size=14, col=C["orange"], weight=700, family=SANS))
body.append(text(BX + 16, BY + 44, "management API · stream protocol · AMQP 0-9-1", size=12,
                 col=C["sub"], family=SANS))
body.append(text(BX + CW - 48, BY + 35, "credentials stay here, in the console", size=12,
                 col=C["sub"], family=SANS, anchor="end"))

# The socket between the two accounts.
SY = AY + AH / 2
body.append(f'<rect x="{AX + AW + 2}" y="{SY - 40}" width="{CX - AX - AW - 4}" height="80" rx="4" '
            f'fill="{C["teal"]}" fill-opacity=".85"/>')
body.append(text(AX + AW + 12, SY - 50, "socket", size=11, col=C["teal"], weight=700, family=SANS,
                 anchor="middle"))
pkt = 'r="6" cx="0" cy="0"'
body.append(f'<circle {pkt} fill="{C["teal"]}" class="{move(1.2, 1.7, 440, 100, 760, 100)}"/>')
body.append(f'<circle {pkt} fill="{C["teal"]}" class="{move(1.8, 2.1, 760, 100, 420, 150)}"/>')
body.append(f'<circle {pkt} fill="{C["violet"]}" class="{move(3.8, 4.3, 440, 210, 760, 230)}"/>')
body.append(f'<circle {pkt} fill="{C["orange"]}" class="{move(6.4, 6.8, 760, 470, 760, 300)}"/>')
body.append(f'<circle {pkt} fill="{C["teal"]}" class="{move(6.9, 7.3, 760, 300, 420, 260)}"/>')

# Captions.
caps = [(0.0, 3.6, "Your agent asks lockmq. It never holds a password."),
        (3.6, 6.4, "Every message read waits for you, in a console it can't touch."),
        (6.4, END, "It gets the messages back, masked.")]
for t0, t1, s in caps:
    body.append(text(W / 2, 600, s, cls=show(t0 + 0.05, t1 - 0.2), size=26, weight=650,
                     family=SANS, anchor="middle"))

label = ("lockmq: Claude Code lists RabbitMQ queues through lockmq, then asks to read a stream; "
         "the lockmq console, in its own account, asks the human to approve, and the agent gets "
         "the messages back with the customer's email masked.")
print(f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}" '
      f'role="img" aria-label="{label}">')
print("<title>lockmq: your AI reads the queue, you hold the key</title>")
print("<style>" + "\n".join(css) +
      "\n@media (prefers-reduced-motion: reduce){*{animation-delay:-8.8s!important;"
      "animation-play-state:paused!important}}</style>")
print("\n".join(body))
print("</svg>")
