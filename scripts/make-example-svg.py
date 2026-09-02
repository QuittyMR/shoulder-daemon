#!/usr/bin/env python3
"""Generate docs/assets/example.svg: a looping, self-contained terminal demo."""

FS = 15.0          # font size
CW = FS * 0.602    # monospace advance
LH = 30.0          # line height
PAD_X = 28.0
TOP = 62.0         # first baseline
T = 14.0           # loop length, seconds

BG, CHROME, BORDER = "#0d1117", "#161b22", "#30363d"
FG, DIM, FAINT = "#e6edf3", "#8b949e", "#6e7681"
BLUE, GREEN, AMBER = "#58a6ff", "#3fb950", "#d29922"

# (row, [(text, colour)...], typed_from_seconds or None, appear_at_seconds)
SCENE = [
    (0, [("› ", BLUE), ("actually we use master here, not main", FG)], 0.5, None),
    (1, [("  INFO ", GREEN), ("fact stored (local): the main branch is master, not main", DIM)], None, 2.6),
    (3, [("  — a new session, a week later —", FAINT)], None, 3.9),
    (5, [("› ", BLUE), ("rebase this onto the main branch", FG)], 4.8, None),
    (7, [("  shoulder-daemon → ", AMBER), ("the main branch is master, not main", FG)], None, 7.0),
    (8, [("  Rebasing onto master.", DIM)], None, 8.3),
]
TYPE_RATE = 0.042      # seconds per character
FADE_OUT, FADE_DONE = 12.4, 13.1

ROWS = max(r for r, *_ in SCENE) + 1
W = 760.0
H = TOP + (ROWS - 1) * LH + 34.0


def pct(t):
    return round(100.0 * t / T, 4)


def keyframes(name, stops):
    body = " ".join(f"{pct(t)}%{{{decl}}}" for t, decl in stops)
    return f"@keyframes {name}{{{body}}}"


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


css, defs, body = [], [], []

for i, (row, spans, typed_at, appear_at) in enumerate(SCENE):
    y = TOP + row * LH
    plain = "".join(t for t, _ in spans)
    n = len(plain)

    attrs = f'x="{PAD_X}" y="{y}"'
    if typed_at is not None:
        dur = n * TYPE_RATE
        end = typed_at + dur
        # The clip rect is what types: its width steps one character at a time.
        # Its static width is the full line, so a renderer that ignores CSS
        # animation shows the finished transcript rather than an empty box.
        defs.append(
            f'<clipPath id="c{i}"><rect id="r{i}" x="{PAD_X}" y="{y - FS + 1}" '
            f'height="{FS + 8}" width="{n * CW + 4}"/></clipPath>'
        )
        css.append(f"#r{i}{{animation:t{i} {T}s infinite steps({n});animation-fill-mode:both}}")
        css.append(keyframes(f"t{i}", [
            (0, "width:0"), (typed_at, "width:0"),
            (end, f"width:{round(n * CW + 4, 2)}px"),
            (T, f"width:{round(n * CW + 4, 2)}px"),
        ]))
        attrs += f' clip-path="url(#c{i})"'
        # Block cursor, walking the line in the same steps, gone once the line
        # is sent.
        css.append(f"#k{i}{{animation:x{i} {T}s infinite steps({n}),o{i} {T}s infinite;animation-fill-mode:both}}")
        css.append(keyframes(f"x{i}", [
            (0, f"x:{PAD_X}px"), (typed_at, f"x:{PAD_X}px"),
            (end, f"x:{round(PAD_X + n * CW, 2)}px"),
            (T, f"x:{round(PAD_X + n * CW, 2)}px"),
        ]))
        css.append(keyframes(f"o{i}", [
            (0, "opacity:0"), (max(typed_at - 0.01, 0), "opacity:0"),
            (typed_at, "opacity:1"), (end + 0.45, "opacity:1"),
            (end + 0.46, "opacity:0"), (T, "opacity:0"),
        ]))
        body.append(
            f'<rect id="k{i}" x="{PAD_X}" y="{y - FS + 2}" width="{round(CW, 2)}" '
            f'height="{FS + 4}" fill="{FG}" opacity="0"/>'
        )
    else:
        css.append(f"#l{i}{{animation:a{i} {T}s infinite;animation-fill-mode:both}}")
        css.append(keyframes(f"a{i}", [
            (0, "opacity:0"), (max(appear_at - 0.25, 0), "opacity:0"),
            (appear_at, "opacity:1"), (T, "opacity:1"),
        ]))

    # textLength pins each line to the same grid whatever monospace font the
    # reader has, so the typing clip lands on character boundaries.
    tspans = "".join(f'<tspan fill="{c}">{esc(t)}</tspan>' for t, c in spans)
    body.append(
        f'<text id="l{i}" {attrs} textLength="{round(n * CW, 2)}" '
        f'lengthAdjust="spacing">{tspans}</text>'
    )

css.append(f".scene{{animation:fade {T}s infinite}}")
css.append(keyframes("fade", [
    (0, "opacity:1"), (FADE_OUT, "opacity:1"),
    (FADE_DONE, "opacity:0"), (T - 0.01, "opacity:0"), (T, "opacity:1"),
]))
css.append("@media (prefers-reduced-motion:reduce){*{animation:none!important}}")

dots = "".join(
    f'<circle cx="{22 + i * 20}" cy="19" r="6" fill="{c}"/>'
    for i, c in enumerate(("#ff5f57", "#febc2e", "#28c840"))
)

svg = f'''<svg xmlns="http://www.w3.org/2000/svg" width="{W:.0f}" height="{H:.0f}" viewBox="0 0 {W:.0f} {H:.0f}" role="img" aria-label="Terminal demo: the user mentions in passing that the branch is called master, shoulder-daemon stores it, and in a later session it tells the agent before the agent gets it wrong.">
<title>shoulder-daemon learns a fact once and supplies it in a later session</title>
<defs>{"".join(defs)}</defs>
<style>
text{{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,"DejaVu Sans Mono",monospace;font-size:{FS}px;white-space:pre}}
{chr(10).join(css)}
</style>
<rect width="{W:.0f}" height="{H:.0f}" rx="10" fill="{BG}" stroke="{BORDER}"/>
<path d="M0 10a10 10 0 0 1 10-10h{W - 20:.0f}a10 10 0 0 1 10 10v28H0z" fill="{CHROME}"/>
<line x1="0" y1="38" x2="{W:.0f}" y2="38" stroke="{BORDER}"/>
{dots}
<g class="scene">{"".join(body)}</g>
</svg>
'''
open("docs/assets/example.svg", "w").write(svg)
print(f"docs/assets/example.svg  {W:.0f}x{H:.0f}")
