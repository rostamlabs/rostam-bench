#!/usr/bin/env python3
"""Render the benchmark charts as static SVG for the README.

GitHub does not execute JavaScript in markdown, so the charts have to be plain
SVG with no script and no external references. Each chart is emitted twice,
light and dark, because a single file cannot be legible on both GitHub themes;
the README pairs them with <picture> + prefers-color-scheme.

The measurements live in DATA below so a future run regenerates the charts by
editing numbers in one place rather than touching path geometry:

    python3 charts/make_charts.py [extra-output-dir ...]

The engine repo's README shows the same figures, so it keeps a mirrored copy
under docs/assets/bench. Pass that directory as an argument to regenerate both
at once and keep them from drifting apart:

    python3 charts/make_charts.py ../../rostam/docs/assets/bench

Source: VectorDBBench 1.0.22, Cohere 1M x 768d cosine, one session,
m=16 / ef_construction=200 / k=100 on every engine.
"""

import math
import pathlib

OUT = pathlib.Path(__file__).parent

# ---------------------------------------------------------------- measurements

# (recall, qps) at each measured ef, per engine
CURVE = {
    "rostam":   [(0.8889, 4691.5), (0.9694, 2675.4), (0.9877, 1660.9),
                 (0.9952, 1042.3), (0.9978, 713.9)],
    "pgvector": [(0.9075, 2833.2), (0.9747, 1087.1), (0.9897, 602.9)],
    "milvus":   [(0.9080, 2588.9), (0.9838, 1023.2), (0.9906, 805.4)],
    "weaviate": [(0.8741, 1135.5), (0.9618, 947.2),  (0.9829, 756.9)],
    "redis":    [(0.8731, 879.9),  (0.9616, 423.9),  (0.9828, 239.8)],
    "qdrant":   [(0.9675, 662.0),  (0.9926, 385.6),  (0.9968, 276.9)],
}

# QPS interpolated to a common recall; None = outside that engine's range
MATCHED = {
    "0.95": {"rostam": 3161, "milvus": 1721, "pgvector": 1729,
             "weaviate": 973, "qdrant": None, "redis": 484},
    "0.97": {"rostam": 2642, "milvus": 1308, "pgvector": 1209,
             "weaviate": 873, "qdrant": 634,  "redis": 351},
    "0.98": {"rostam": 2088, "milvus": 1102, "pgvector": 916,
             "weaviate": 783, "qdrant": 524,  "redis": 264},
    "0.99": {"rostam": 1471, "milvus": 826,  "pgvector": None,
             "weaviate": None, "qdrant": 414, "redis": None},
}

# ef=300 row: load wall-clock (s), load CPU (s), cores at peak, max qps
LOAD = {
    "rostam":   dict(load=282.2,  cpu=1933.8, cores=7.54,  qps=2675.4),
    "pgvector": dict(load=386.1,  cpu=2637.0, cores=11.04, qps=1087.1),
    "milvus":   dict(load=476.3,  cpu=2695.6, cores=9.81,  qps=1023.2),
    "qdrant":   dict(load=592.2,  cpu=2386.0, cores=9.92,  qps=385.6),
    "redis":    dict(load=1354.9, cpu=1355.5, cores=1.00,  qps=423.9),
    "weaviate": dict(load=1432.7, cpu=4937.0, cores=8.86,  qps=947.2),
}

LABEL = {"rostam": "Rostam", "milvus": "Milvus", "pgvector": "pgvector",
         "weaviate": "Weaviate", "qdrant": "Qdrant", "redis": "Redis"}
ORDER = ["rostam", "milvus", "pgvector", "weaviate", "qdrant", "redis"]

THEME = {
    "light": dict(
        ink="#0F151D", muted="#55616F", faint="#8994A2",
        rule="#DCE1E7", grid="#E7EBEF", bg="#FFFFFF", ok="#0E7C5A",
        e=dict(rostam="#B45309", milvus="#0E8C86", pgvector="#4457C9",
               weaviate="#5E8C1F", qdrant="#C2385C", redis="#7B57C4"),
    ),
    "dark": dict(
        ink="#E4E9EF", muted="#93A0AE", faint="#64717F",
        rule="#2A3542", grid="#1E2732", bg="#0D1117", ok="#3FBE8F",
        e=dict(rostam="#F0A22E", milvus="#35C4BB", pgvector="#7F90F2",
               weaviate="#9CCB4B", qdrant="#F0708F", redis="#AC8DF0"),
    ),
}

MONO = "ui-monospace, SFMono-Regular, Menlo, Consolas, monospace"


# ---------------------------------------------------------------- svg helpers

def esc(s):
    return (str(s).replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;"))


def text(x, y, s, fill, size=10.5, anchor="start", weight="normal", spacing=None):
    ls = f' letter-spacing="{spacing}"' if spacing else ""
    return (f'<text x="{x:.1f}" y="{y:.1f}" fill="{fill}" font-family="{MONO}" '
            f'font-size="{size}" text-anchor="{anchor}" font-weight="{weight}"{ls}>'
            f'{esc(s)}</text>')


def line(x1, y1, x2, y2, stroke, w=1, dash=None):
    d = f' stroke-dasharray="{dash}"' if dash else ""
    return (f'<line x1="{x1:.1f}" y1="{y1:.1f}" x2="{x2:.1f}" y2="{y2:.1f}" '
            f'stroke="{stroke}" stroke-width="{w}"{d}/>')


def rect(x, y, w, h, fill, opacity=1, rx=1):
    return (f'<rect x="{x:.1f}" y="{y:.1f}" width="{max(w, 0):.1f}" '
            f'height="{max(h, 0):.1f}" fill="{fill}" opacity="{opacity}" rx="{rx}"/>')


def circle(cx, cy, r, fill, stroke, sw=1.6, opacity=1):
    return (f'<circle cx="{cx:.1f}" cy="{cy:.1f}" r="{r:.1f}" fill="{fill}" '
            f'opacity="{opacity}" stroke="{stroke}" stroke-width="{sw}"/>')


def svg(width, height, body, t, title):
    return (f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {width} {height}" '
            f'width="{width}" height="{height}" role="img" aria-label="{esc(title)}">'
            f'<title>{esc(title)}</title>'
            f'<rect width="{width}" height="{height}" fill="{t["bg"]}"/>'
            + body + "</svg>\n")


def fmt(n):
    return f"{n:,.0f}"


# ---------------------------------------------------------------- chart 1

def chart_pareto(t):
    W, H = 880, 470
    L, R, T, B = 62, 112, 26, 56
    x0, x1, y0, y1 = L, W - R, H - B, T
    rmin, rmax, qmin, qmax = 0.865, 1.0, 200.0, 5200.0

    def X(r):
        return x0 + (r - rmin) / (rmax - rmin) * (x1 - x0)

    def Y(q):
        return y0 - (math.log10(q) - math.log10(qmin)) / \
            (math.log10(qmax) - math.log10(qmin)) * (y0 - y1)

    o = []
    for q in (200, 500, 1000, 2000, 5000):
        o.append(line(x0, Y(q), x1, Y(q), t["grid"]))
        o.append(text(x0 - 9, Y(q) + 3.5, fmt(q), t["faint"], anchor="end"))
    for r in (0.88, 0.90, 0.92, 0.94, 0.96, 0.98, 1.00):
        o.append(line(X(r), y0, X(r), y1, t["grid"]))
        o.append(text(X(r), y0 + 18, f"{r:.2f}", t["faint"], anchor="middle"))
    o.append(line(x0, y0, x1, y0, t["rule"]))
    o.append(text((x0 + x1) / 2, y0 + 40, "RECALL @ k=100", t["faint"],
                  size=10, anchor="middle", spacing="0.12em"))
    o.append(f'<g transform="rotate(-90 16 {(y0 + y1) / 2:.1f})">'
             + text(16, (y0 + y1) / 2, "QUERIES / SEC", t["faint"], size=10,
                    anchor="middle", spacing="0.12em") + "</g>")

    for k in ORDER:
        pts = CURVE[k]
        lead = k == "rostam"
        col = t["e"][k]
        d = " ".join(("M" if i == 0 else "L") + f"{X(r):.1f},{Y(q):.1f}"
                     for i, (r, q) in enumerate(pts))
        o.append(f'<path d="{d}" fill="none" stroke="{col}" '
                 f'stroke-width="{2.8 if lead else 1.6}" stroke-linecap="round" '
                 f'stroke-linejoin="round" opacity="{1 if lead else 0.85}"/>')
        for r, q in pts:
            o.append(circle(X(r), Y(q), 4.4 if lead else 3.1, t["bg"], col,
                            2.5 if lead else 1.7))
        lr, lq = pts[-1]
        o.append(text(X(lr) + 11, Y(lq) + 4, LABEL[k], col, size=11.5, weight="600"))

    lr, lq = CURVE["rostam"][-1]
    o.append(text(X(lr) + 11, Y(lq) + 19, "0.9978", t["e"]["rostam"], size=10.5, weight="600"))
    return svg(W, H, "".join(o), t,
               "Recall versus queries per second. Rostam's curve is above every "
               "other engine across the full recall range.")


# ---------------------------------------------------------------- chart 2

def chart_matched(t):
    W, H = 880, 400
    L, R, T, B = 60, 24, 20, 70
    x0, x1, y0, y1 = L, W - R, H - B, T
    qmax = 3400.0
    levels = ["0.95", "0.97", "0.98", "0.99"]

    def Y(q):
        return y0 - q / qmax * (y0 - y1)

    o = []
    for q in (0, 1000, 2000, 3000):
        o.append(line(x0, Y(q), x1, Y(q), t["grid"]))
        o.append(text(x0 - 9, Y(q) + 3.5, fmt(q), t["faint"], anchor="end"))

    gw = (x1 - x0) / len(levels)
    bw = min(21.0, (gw - 34) / len(ORDER))
    for gi, lv in enumerate(levels):
        gx = x0 + gi * gw
        row = MATCHED[lv]
        for bi, k in enumerate(ORDER):
            q = row[k]
            x = gx + (gw - bw * len(ORDER)) / 2 + bi * bw
            if q is None:
                o.append(text(x + bw / 2, y0 - 6, "–", t["faint"], anchor="middle"))
                continue
            lead = k == "rostam"
            o.append(rect(x, Y(q), bw - 3, y0 - Y(q), t["e"][k], 1 if lead else 0.8))
            if lead:
                o.append(text(x + (bw - 3) / 2, Y(q) - 6, fmt(q), t["e"][k],
                              anchor="middle", weight="600"))
        o.append(text(gx + gw / 2, y0 + 20, f"recall {lv}", t["muted"], anchor="middle"))
    o.append(line(x0, y0, x1, y0, t["rule"]))

    lx = x0
    for k in ORDER:
        o.append(rect(lx, y0 + 40, 9, 9, t["e"][k], rx=2))
        o.append(text(lx + 14, y0 + 48, LABEL[k], t["muted"]))
        lx += len(LABEL[k]) * 6.5 + 36
    return svg(W, H, "".join(o), t,
               "Throughput at matched recall for six engines at recall 0.95 "
               "through 0.99.")


# ---------------------------------------------------------------- chart 3

def chart_loadcpu(t):
    W, H = 880, 420
    L, R, T, B = 68, 132, 28, 56
    x0, x1, y0, y1 = L, W - R, H - B, T
    xmax, ymax = 1550.0, 5200.0

    def X(s):
        return x0 + s / xmax * (x1 - x0)

    def Y(c):
        return y0 - c / ymax * (y0 - y1)

    o = []
    for c in (0, 1000, 2000, 3000, 4000, 5000):
        o.append(line(x0, Y(c), x1, Y(c), t["grid"]))
        o.append(text(x0 - 9, Y(c) + 3.5, fmt(c), t["faint"], anchor="end"))
    for s in (0, 300, 600, 900, 1200, 1500):
        o.append(line(X(s), y0, X(s), y1, t["grid"]))
        o.append(text(X(s), y0 + 18, f"{s}s", t["faint"], anchor="middle"))
    o.append(line(x0, y0, x1, y0, t["rule"]))
    o.append(text((x0 + x1) / 2, y0 + 40, "LOAD WALL-CLOCK", t["faint"], size=10,
                  anchor="middle", spacing="0.12em"))
    o.append(f'<g transform="rotate(-90 18 {(y0 + y1) / 2:.1f})">'
             + text(18, (y0 + y1) / 2, "CPU-SECONDS SPENT", t["faint"], size=10,
                    anchor="middle", spacing="0.12em") + "</g>")
    o.append(text(x0 + 6, y1 + 10, "← faster · cheaper ↓", t["ok"], size=10.5))

    for k in ORDER:
        d = LOAD[k]
        lead = k == "rostam"
        col = t["e"][k]
        r = 5 + math.sqrt(d["cores"]) * 2.6
        o.append(circle(X(d["load"]), Y(d["cpu"]), r, col, col,
                        2.5 if lead else 1.6, 0.95 if lead else 0.3))
        right = d["load"] <= 1100
        dx = (r + 9) if right else -(r + 9)
        anchor = "start" if right else "end"
        o.append(text(X(d["load"]) + dx, Y(d["cpu"]) - 2, LABEL[k], col,
                      size=11.5, weight="600", anchor=anchor))
        o.append(text(X(d["load"]) + dx, Y(d["cpu"]) + 12,
                      f'{d["load"]:.0f}s · {fmt(d["cpu"])} cpu-s',
                      col if lead else t["muted"], anchor=anchor,
                      weight="600" if lead else "normal"))
    return svg(W, H, "".join(o), t,
               "Load wall-clock against CPU seconds spent. Rostam is fastest "
               "and cheapest simultaneously.")


# ---------------------------------------------------------------- main

CHARTS = {
    "pareto": chart_pareto,
    "matched-recall": chart_matched,
    "load-cpu": chart_loadcpu,
}

if __name__ == "__main__":
    import sys

    targets = [OUT] + [pathlib.Path(a) for a in sys.argv[1:]]
    for d in targets[1:]:
        if not d.is_dir():
            raise SystemExit(f"not a directory: {d}")
    for name, fn in CHARTS.items():
        for theme in ("light", "dark"):
            body = fn(THEME[theme])
            for d in targets:
                (d / f"{name}-{theme}.svg").write_text(body, encoding="utf-8")
            print(f"wrote {name}-{theme}.svg  ->  {len(targets)} location(s)")
