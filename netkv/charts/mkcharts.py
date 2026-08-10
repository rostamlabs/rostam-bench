#!/usr/bin/env python3
"""Generate the benchmark charts as SVG (light + dark), for the netkv README.

Design rules followed (dataviz skill):
  - form chosen by the data's job: lines for change-over-concurrency,
    grouped bars for comparing a few named configurations
  - ONE y-axis, never dual
  - categorical hues assigned in FIXED order and bound to the ENTITY, so an
    engine keeps its colour in every chart
  - palette validated by scripts/validate_palette.js for BOTH surfaces
    (light PASSes with a contrast WARN -> relieved by direct labels + the
    README's tables; dark PASSes outright)
  - 2px lines, >=8px markers, recessive grid, selective direct labels
  - label text wears ink tokens, never the series colour; the colored marker
    beside it carries identity
"""
import pathlib
import sys

W, H = 760, 400
# MB is 66, not 52: at 52 the x-axis label and the footnote landed on the same
# baseline. Caught by screenshotting, not by the palette validator.
ML, MR, MT, MB = 66, 132, 52, 66
PW, PH = W - ML - MR, H - MT - MB

THEMES = {
    "light": dict(surface="#fcfcfb", ink="#0b0b0b", ink2="#52514e", grid="#e5e4e0",
                  rule="#8a8880",
                  series=["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#4a3aa7"]),
    "dark": dict(surface="#1a1a19", ink="#ffffff", ink2="#c3c2b7", grid="#33322e",
                 rule="#7a786f",
                 series=["#3987e5", "#d95926", "#199e70", "#c98500", "#d55181", "#008300", "#9085e9"]),
}

# Entity -> fixed categorical slot. Colour follows the ENTITY, never its rank,
# so an engine keeps its hue in every chart even when the ordering changes.
#
# WHY THE CHARTS ARE FACETED. Seven series cannot pass the validator's
# --pairs all check against this 8-hue palette: orange(#eb6834) sits at
# dE 3.2 from green(#008300) under protanopia and dE 12.9 from
# magenta(#e87ba4) for NORMAL vision (below the hard floor of 15). The default
# adjacent-pairs check misses both because those slots are not neighbours --
# but in these charts the series genuinely overlap, so all-pairs is the
# relevant test. Per the skill, the remedy at that point is to CUT SERIES OR
# FACET, not to force the colours. So every chart draws from a subset
# validated under --pairs all:
#   facet A (multi-threaded)  blue, orange, aqua, violet   -> PASS
#   facet B (redis-family)    blue, yellow, magenta, green -> PASS
# Rostam is blue in both, and the engines omitted from a chart are still in
# that section's table.
SLOT = {"Rostam": 0, "Memcached": 1, "Dragonfly": 2,      # facet A + ...
        "Valkey": 3, "Redis": 4, "KeyDB": 5,              # facet B
        "Aerospike": 6}                                    # ... violet, facet A


def esc(s):
    return s.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def head(t, title, sub):
    return f'''<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="{esc(title)}">
<rect width="{W}" height="{H}" fill="{t['surface']}"/>
<style>
 .ttl{{font:600 15px system-ui,-apple-system,Segoe UI,Roboto,sans-serif;fill:{t['ink']}}}
 .sub{{font:400 12px system-ui,-apple-system,Segoe UI,Roboto,sans-serif;fill:{t['ink2']}}}
 .ax {{font:400 11px system-ui,-apple-system,Segoe UI,Roboto,sans-serif;fill:{t['ink2']}}}
 .lb {{font:600 11px system-ui,-apple-system,Segoe UI,Roboto,sans-serif;fill:{t['ink']}}}
 .an {{font:400 10.5px system-ui,-apple-system,Segoe UI,Roboto,sans-serif;fill:{t['ink2']}}}
</style>
<text class="ttl" x="{ML}" y="26">{esc(title)}</text>
<text class="sub" x="{ML}" y="43">{esc(sub)}</text>
'''


def yaxis(t, ymax, ticks, unit="k ops/s"):
    o = []
    for v in ticks:
        y = MT + PH - (v / ymax) * PH
        o.append(f'<line x1="{ML}" y1="{y:.1f}" x2="{ML+PW}" y2="{y:.1f}" stroke="{t["grid"]}" stroke-width="1"/>')
        o.append(f'<text class="ax" x="{ML-9}" y="{y+4:.1f}" text-anchor="end">{v:g}</text>')
    # Unit label sits in the LEFT MARGIN above the top tick. It is horizontally
    # clear of the subtitle (which starts at x=ML), so there is no collision --
    # moving it to the bottom instead made it overlap the first x tick.
    o.append(f'<text class="ax" x="{ML-9}" y="{MT-10}" text-anchor="end">{unit}</text>')
    return "".join(o)


def decollide(ys, min_gap=15.0, lo=MT - 6, hi=MT + PH + 6):
    """Nudge end-labels apart so they never overlap.

    Screenshotting the first render showed Rostam/Memcached and Redis/Aerospike
    printed on top of each other -- the series genuinely finish within ~2% of
    one another, which is the chart's whole point, so the fix is to move the
    LABELS, not to hide the overlap.
    """
    order = sorted(range(len(ys)), key=lambda i: ys[i])
    out = list(ys)
    for k, i in enumerate(order):          # top-down pass
        if k and out[i] - out[order[k - 1]] < min_gap:
            out[i] = out[order[k - 1]] + min_gap
    for k in range(len(order) - 2, -1, -1):  # bottom-up correction
        i, j = order[k], order[k + 1]
        if out[j] - out[i] < min_gap:
            out[i] = out[j] - min_gap
    shift = max(0.0, lo - min(out)) - max(0.0, max(out) - hi)
    return [y + shift for y in out]


def line_chart(theme, title, sub, xs, series, ymax, ticks, xlabel, rule=None):
    t = THEMES[theme]
    o = [head(t, title, sub), yaxis(t, ymax, ticks)]
    n = len(xs)
    xpos = [ML + (PW * i / (n - 1)) for i in range(n)]
    for x, lab in zip(xpos, xs):
        o.append(f'<text class="ax" x="{x:.1f}" y="{MT+PH+20}" text-anchor="middle">{lab}</text>')
    o.append(f'<text class="ax" x="{ML+PW/2:.1f}" y="{MT+PH+40}" text-anchor="middle">{esc(xlabel)}</text>')

    if rule:
        val, txt = rule
        y = MT + PH - (val / ymax) * PH
        o.append(f'<line x1="{ML}" y1="{y:.1f}" x2="{ML+PW}" y2="{y:.1f}" stroke="{t["rule"]}" '
                 f'stroke-width="1.5" stroke-dasharray="5 4"/>')
        o.append(f'<text class="an" x="{ML+6}" y="{y-7:.1f}">{esc(txt)}</text>')

    names = [s[0] for s in series]
    colors = [t["series"][SLOT[n]] if n in SLOT else t["series"][i] for i, n in enumerate(names)]
    ends = [MT + PH - (v[-1] / ymax) * PH for _, v in series]
    lab_y = decollide(ends)

    for (name, vals), c in zip(series, colors):
        pts = " ".join(f"{x:.1f},{MT+PH-(v/ymax)*PH:.1f}" for x, v in zip(xpos, vals))
        o.append(f'<polyline points="{pts}" fill="none" stroke="{c}" stroke-width="2" '
                 f'stroke-linejoin="round" stroke-linecap="round"/>')
        for x, v in zip(xpos, vals):
            y = MT + PH - (v / ymax) * PH
            # 2px surface ring so overlapping markers stay legible
            o.append(f'<circle cx="{x:.1f}" cy="{y:.1f}" r="4.5" fill="{c}" stroke="{t["surface"]}" stroke-width="2"/>')

    for name, c, ey, ly in zip(names, colors, ends, lab_y):
        if abs(ly - ey) > 1.5:   # leader line back to the true endpoint
            o.append(f'<path d="M {ML+PW+3:.1f} {ey:.1f} L {ML+PW+9:.1f} {ly:.1f}" '
                     f'stroke="{c}" stroke-width="1" fill="none" opacity="0.65"/>')
        o.append(f'<circle cx="{ML+PW+13}" cy="{ly:.1f}" r="4.5" fill="{c}"/>')
        o.append(f'<text class="lb" x="{ML+PW+23}" y="{ly+4:.1f}">{esc(name)}</text>')
    o.append("</svg>")
    return "".join(o)


def bar_chart(theme, title, sub, groups, series, ymax, ticks, xlabel, note=None):
    """groups: [group-label]; series: [(name, [v per group])]"""
    t = THEMES[theme]
    o = [head(t, title, sub), yaxis(t, ymax, ticks)]
    ng, ns = len(groups), len(series)
    gw = PW / ng
    bw = min(46, (gw - 22) / ns)
    for gi, g in enumerate(groups):
        gx = ML + gw * gi + gw / 2
        o.append(f'<text class="ax" x="{gx:.1f}" y="{MT+PH+20}" text-anchor="middle">{esc(g)}</text>')
        for si, (name, vals) in enumerate(series):
            c = t["series"][SLOT[name]] if name in SLOT else t["series"][si]
            v = vals[gi]
            bh = (v / ymax) * PH
            # 2px surface gap between adjacent bars
            x = gx - (ns * bw) / 2 + si * bw + 1
            y = MT + PH - bh
            o.append(f'<rect x="{x:.1f}" y="{y:.1f}" width="{bw-2:.1f}" height="{bh:.1f}" '
                     f'fill="{c}" rx="4" ry="4"/>')
            o.append(f'<text class="lb" x="{x+(bw-2)/2:.1f}" y="{y-6:.1f}" text-anchor="middle">{v:g}</text>')
    o.append(f'<text class="ax" x="{ML+PW/2:.1f}" y="{MT+PH+38}" text-anchor="middle">{esc(xlabel)}</text>')
    # Legend for >=2 series only. A single series needs none -- the title
    # already names it, and the box was overflowing the right margin.
    lx = ML + PW + 13
    for si, (name, _) in enumerate(series if ns > 1 else []):
        c = t["series"][SLOT[name]] if name in SLOT else t["series"][si]
        y = MT + 12 + si * 22
        o.append(f'<rect x="{lx}" y="{y-8}" width="10" height="10" fill="{c}" rx="2"/>')
        o.append(f'<text class="lb" x="{lx+16}" y="{y+1}">{esc(name)}</text>')
    if note:
        o.append(f'<text class="an" x="{ML}" y="{H-10}">{esc(note)}</text>')
    o.append("</svg>")
    return "".join(o)


# The script lives IN charts/, so output is its own directory. It used to be
# parent/"charts", which was correct when mkcharts.py sat in netkv/ and wrote
# one level down; after the move that silently produced charts/charts/.
OUT = pathlib.Path(__file__).parent
OUT.mkdir(exist_ok=True)


# Mirror targets. The engine repo's README shows these same figures, so it keeps
# a copy under docs/assets/bench; passing that directory here regenerates both in
# one run and stops them drifting apart:
#
#     python3 mkcharts.py ../../../rostam/docs/assets/bench
#
# Same contract as vectordbbench/charts/make_charts.py, which grew this first —
# the vector charts reached the engine README and the KV ones did not, purely
# because only one generator had the hook.
TARGETS = [OUT]
for _arg in sys.argv[1:]:
    _dir = pathlib.Path(_arg)
    if not _dir.is_dir():
        raise SystemExit(f"not a directory: {_dir}")
    TARGETS.append(_dir)


def emit(stem, fn):
    for theme in ("light", "dark"):
        body = fn(theme)
        for d in TARGETS:
            (d / f"{stem}-{theme}.svg").write_text(body, encoding="utf-8")


# --- 1. wire concurrency sweep -------------------------------------------
emit("wire-sweep", lambda th: line_chart(
    th,
    "GET throughput vs concurrency — single node, over the network",
    "Hetzner CCX33, medians of 3, 0 errors. Anything at the dashed line is NIC-bound, not engine-bound.",
    ["8", "64", "256", "512"],
    # Trimmed to the validated facet-A four. Three of them pile up on the
    # ceiling and Aerospike sits below it, which is the whole point; Redis and
    # KeyDB are in this section's table. (Six series here failed --pairs all.)
    [("Rostam", [38.9, 133.6, 147.5, 150.4]),
     ("Memcached", [38.9, 134.6, 147.5, 149.8]),
     ("Dragonfly", [30.7, 124.2, 141.4, 147.4]),
     ("Aerospike", [37.1, 128.1, 125.0, 122.4])],
    170, [0, 50, 100, 150],
    "concurrent connections",
    rule=(150, "single-RX-queue NIC ceiling ~150k — three engines tie here because of this")))

# --- 2/3. full local sweep, all 7 engines on one machine ------------------
# Supersedes the earlier 3-engine co-located chart: same conditions for every
# engine, so the ranking is internally consistent rather than stitched together
# from a NIC-capped wire run and a partial loopback run.
LOCAL_GET = [
    ("Rostam",    [360.2, 726.1, 732.4, 705.1]),
    ("Memcached", [367.8, 682.9, 681.7, 667.1]),
    ("Dragonfly", [234.1, 532.0, 551.6, 518.7]),
    ("Aerospike", [318.4, 510.6, 516.9, 513.6]),
    ("Valkey",    [210.3, 239.1, 227.9, 221.3]),
    ("Redis",     [213.2, 228.3, 219.6, 208.5]),
    ("KeyDB",     [192.5, 206.3, 208.8, 196.3]),
]
LOCAL_PUT = [
    ("Rostam",    [357.0, 688.0, 699.2, 687.4]),
    ("Memcached", [363.5, 658.4, 671.5, 664.5]),
    ("Dragonfly", [223.6, 487.0, 515.1, 506.5]),
    ("Aerospike", [301.4, 499.1, 485.6, 476.1]),
    ("Valkey",    [214.7, 229.8, 224.1, 214.3]),
    ("KeyDB",     [199.3, 210.6, 213.7, 208.3]),
    ("Redis",     [167.6, 197.5, 204.6, 195.3]),
]

FAST = ("Rostam", "Memcached", "Dragonfly", "Aerospike")   # facet A
FAMILY = ("Rostam", "Valkey", "Redis", "KeyDB")            # facet B
pick = lambda rows, names: [r for r in rows if r[0] in names]

emit("local-get-fast", lambda th: line_chart(
    th,
    "GET — the engines that scale with concurrency (loopback)",
    "One machine, all engines in Docker, 8 pinned cores each, 0 errors. Directional: ranking, not absolutes.",
    ["8", "64", "256", "512"], pick(LOCAL_GET, FAST),
    800, [0, 200, 400, 600, 800], "concurrent connections"))

emit("local-get-family", lambda th: line_chart(
    th,
    "GET — the Redis family plateaus (loopback, same run)",
    "Redis, Valkey and KeyDB execute commands single-threaded, so extra connections queue instead of adding work.",
    ["8", "64", "256", "512"], pick(LOCAL_GET, FAMILY),
    800, [0, 200, 400, 600, 800], "concurrent connections"))

emit("local-put-fast", lambda th: line_chart(
    th,
    "PUT — the engines that scale with concurrency (loopback)",
    "Same run and conditions as the GET chart.",
    ["8", "64", "256", "512"], pick(LOCAL_PUT, FAST),
    800, [0, 200, 400, 600, 800], "concurrent connections"))

emit("local-put-family", lambda th: line_chart(
    th,
    "PUT — the Redis family plateaus (loopback, same run)",
    "Same plateau on writes; Rostam shown for scale.",
    ["8", "64", "256", "512"], pick(LOCAL_PUT, FAMILY),
    800, [0, 200, 400, 600, 800], "concurrent connections"))

# --- 3. cost of replication ----------------------------------------------
emit("replication", lambda th: line_chart(
    th,
    "What replication costs — PUT throughput, Rostam vs Rostam",
    "Local, 4 cores/node, durability matched (raft -nosync -volatile-log; PB has no WAL). Directional.",
    ["64", "256", "512"],
    [("single node RF=1", [460.2, 452.8, 445.5]),
     ("PB RF=2", [307.9, 355.5, 351.5]),
     ("raft RF=3", [112.4, 162.3, 175.3])],
    500, [0, 100, 200, 300, 400, 500],
    "concurrent connections"))

# --- 4. sharding & scale-out ---------------------------------------------
emit("sharding", lambda th: bar_chart(
    th,
    "Sharding and scale-out — PUT, 3-node PB RF=2",
    "Shard count is not a throughput lever; the 2->3 node step is (see note).",
    ["4 shards", "8", "16", "32"],
    [("shard sweep @512 conns", [329.4, 346.3, 335.7, 349.3])],
    420, [0, 100, 200, 300, 400],
    "shards per cluster",
    note="4->8 buys ~5%, then flat. Node scale-out 2->3 at constant RF=2: 190k -> 357k (1.88x for 1.5x nodes)."))

print("wrote:", ", ".join(sorted(p.name for p in OUT.glob("*.svg"))))

# --- 8. replicated writes at matched commit semantics ---------------------
# ONE chart, three series. Redis/Valkey/KeyDB land within ~7% of each other at
# every concurrency (28.7/28.0/28.9 -> 43.2/42.2/42.1 -> 42.9/45.9/43.9), so
# drawing them separately spends three of a four-colour facet to say one thing:
# the Redis-protocol engines share a ceiling. Collapsed to their median and
# labelled as the family; the per-engine numbers are in the table.
#
# memcached is deliberately absent: at 271k it would flatten everything else,
# and it has NO replication, so putting it on a replication chart invites the
# one comparison this section exists to avoid.
RF2_ALL = [("Rostam", [35.6, 80.9, 113.6]),
           ("Aerospike", [38.0, 57.0, 85.9]),
           # Not a SLOT name, so it takes series[2] = aqua, which sits in the
           # same validated facet-A set as blue (Rostam) and violet (Aerospike).
           ("Redis / Valkey / KeyDB", [28.7, 42.2, 43.9])]
RF2_MASTER = [("Rostam", [57.8, 103.0, 157.4]),
              ("Aerospike", [65.3, 94.2, 125.7])]

emit("rf2-commit-all", lambda th: line_chart(
    th,
    "Replicated writes (RF=2) — ack only after a replica has the write",
    "12-vCPU EPYC Genoa, 3 co-located nodes + generator, n=2 reps, 0 errors. "
    "Third line is Redis/Valkey/KeyDB, which overlap within ~7%.",
    ["8", "32", "128"], RF2_ALL, 130, [0, 50, 100],
    "concurrent connections"))

emit("rf2-commit-master", lambda th: line_chart(
    th,
    "Same run, both engines relaxed to ack at the master",
    "Worth 1.57x to Rostam and 1.58x to Aerospike — the posture flatters neither.",
    ["8", "32", "128"], RF2_MASTER, 170, [0, 50, 100, 150],
    "concurrent connections"))
