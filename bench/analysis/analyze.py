#!/usr/bin/env python3
"""DIMP benchmark analysis.

Reads results/raw/<solution>/<run-id>/{metadata.json,loadgen.csv,events.jsonl,
triggers.csv,rss.csv}, computes per-run metrics (definitions: see
docs/evaluation-methodology.md, section "Metrics"), aggregates repetitions with
95% Student-t confidence intervals and writes:

  results/summary_runs.csv   one row per run (all metrics)
  results/summary.csv        long format: solution,experiment,metric,n,mean,std,ci95_lo,ci95_hi,min,max
  results/summary.md         readable tables per experiment
  results/plots/...          timeline / RSS plots per run (rep 1 by default), CDF per experiment

Usage: python3 bench/analysis/analyze.py [--raw results/raw] [--out results] [--plots first|all|none]
"""
import argparse
import glob
import json
import math
import os
import sys

import numpy as np
import pandas as pd

try:
    from scipy import stats as _st

    def tq(df):
        return float(_st.t.ppf(0.975, df))
except Exception:  # pragma: no cover - fallback table
    _T = {1: 12.706, 2: 4.303, 3: 3.182, 4: 2.776, 5: 2.571, 6: 2.447, 7: 2.365, 8: 2.306, 9: 2.262,
          10: 2.228, 15: 2.131, 20: 2.086, 30: 2.042}

    def tq(df):
        k = max(x for x in _T if x <= df) if df >= 1 else 1
        return _T[k] if df <= 30 else 1.96

# Fixed categorical order (validated reference palette); entities keep their color.
PALETTE = ["#2a78d6", "#eb6834", "#1baf7a", "#eda100", "#e87ba4", "#008300", "#6250d6", "#e34948"]
ERR_COLORS = {"refused": PALETTE[1], "reset": PALETTE[7], "timeout": PALETTE[6], "eof": PALETTE[3],
              "other": "#52514e"}
INK, INK2, GRID = "#0b0b0b", "#52514e", "#e4e3df"

POST_WINDOW_S = 1.0       # upgrade window extends this long after upgrade_done/failed
NO_DONE_WINDOW_S = 5.0    # ... or after the last trigger if no completion event


# --------------------------------------------------------------------------- loading
def load_run(d):
    meta = json.load(open(os.path.join(d, "metadata.json")))
    lg = pd.read_csv(os.path.join(d, "loadgen.csv"), dtype={"version": str, "err_class": str},
                     keep_default_na=False, na_values=[])
    for c in ["intended_ns", "start_ns", "end_ns", "latency_ns", "pid", "count", "server_ts_ns"]:
        lg[c] = pd.to_numeric(lg[c], errors="coerce").fillna(0).astype("int64")
    ev = []
    p = os.path.join(d, "events.jsonl")
    if os.path.exists(p):
        for line in open(p):
            try:
                e = json.loads(line)
                if "ev" in e and "ts_ns" in e:
                    ev.append(e)
            except ValueError:
                pass
    ev.sort(key=lambda e: e["ts_ns"])
    trig = pd.read_csv(os.path.join(d, "triggers.csv")) if os.path.exists(os.path.join(d, "triggers.csv")) else pd.DataFrame()
    rss = pd.DataFrame()
    p = os.path.join(d, "rss.csv")
    if os.path.exists(p) and os.path.getsize(p) > 0:
        rss = pd.read_csv(p)
    return meta, lg, ev, trig, rss


def q(s, p):
    return float(np.quantile(s, p)) if len(s) else float("nan")


def longest_fail_streak(df, ok_mask):
    """Longest run of consecutive failed requests in intended-time order.
    Returns span (ns) from first failure's intended time to the intended time
    of the next successful request (or last failure + 1 interval)."""
    okv = ok_mask.to_numpy()
    t = df["intended_ns"].to_numpy()
    best, i, n = 0, 0, len(okv)
    while i < n:
        if okv[i]:
            i += 1
            continue
        j = i
        while j < n and not okv[j]:
            j += 1
        end = t[j] if j < n else t[j - 1]
        best = max(best, end - t[i])
        i = j
    return best


def upgrade_spans(ev):
    """Pair each upgrade_start with the next upgrade_done/upgrade_failed."""
    spans, cur = [], None
    for e in ev:
        if e["ev"] == "upgrade_start":
            cur = e
        elif e["ev"] in ("upgrade_done", "upgrade_failed") and cur is not None:
            spans.append((cur, e))
            cur = None
    return spans


# --------------------------------------------------------------------------- per-run metrics
def analyze_run(d):
    meta, lg, ev, trig, rss = load_run(d)
    m = {"solution": meta["sol"], "experiment": meta["exp"], "rep": meta["rep"], "run_id": meta["run_id"],
         "run_dir": d}
    t0 = meta["t0"]
    lg = lg.sort_values("intended_ns").reset_index(drop=True)
    ms = t0 + int(meta["warmup_s"] * 1e9)
    post = lg[lg.intended_ns >= ms].reset_index(drop=True)
    ok = post.err_class == "none"
    trig_ts = list(trig["ts_ns"]) if len(trig) else []
    t_trig = trig_ts[0] if trig_ts else None

    m["n_requests"] = len(post)
    m["n_ok"] = int(ok.sum())
    m["n_errors"] = int((~ok).sum())
    m["error_rate"] = m["n_errors"] / max(1, len(post))
    for c in ["refused", "reset", "timeout", "eof", "other"]:
        m[f"err_{c}"] = int((post.err_class == c).sum())
    m["err_http"] = int(post.err_class.str.startswith("http_").sum())
    dur = (post.intended_ns.max() - post.intended_ns.min()) / 1e9 if len(post) > 1 else float("nan")
    m["achieved_ok_rps"] = m["n_ok"] / dur if dur and dur > 0 else float("nan")
    lag = (post.start_ns - post.intended_ns) / 1e6
    m["client_lag_p99_ms"] = q(lag, 0.99)  # harness validity: dispatcher lateness
    m["retried_requests"] = int((post.attempts > 1).sum())

    # ---- windows
    spans = upgrade_spans(ev)
    ends = [e["ts_ns"] for e in ev if e["ev"] in ("upgrade_done", "upgrade_failed")]
    if t_trig is not None:
        if ends and ends[-1] >= t_trig:
            w_end = ends[-1] + int(POST_WINDOW_S * 1e9)
            m["upgrade_completed_event"] = True
        else:
            w_end = trig_ts[-1] + int(NO_DONE_WINDOW_S * 1e9)
            m["upgrade_completed_event"] = False
        steady = post[post.intended_ns < t_trig]
        upw = post[(post.intended_ns >= t_trig) & (post.intended_ns <= w_end)]
    else:
        w_end = None
        steady, upw = post, post.iloc[0:0]
    m["upgrade_window_s"] = (w_end - t_trig) / 1e9 if t_trig else float("nan")

    # ---- availability
    okt = np.sort(post.loc[ok, "end_ns"].to_numpy())
    m["max_success_gap_ms"] = float(np.diff(okt).max() / 1e6) if len(okt) > 1 else float("nan")
    # reference: same statistic over completions that land in [warm-up end, T) (selected by
    # completion time, not intended time, so long requests issued just before T don't create an artificial gap)
    st_ok = okt[(okt >= ms) & (okt < t_trig)] if t_trig is not None else okt
    m["steady_max_success_gap_ms"] = float(np.diff(st_ok).max() / 1e6) if len(st_ok) > 1 else float("nan")
    if t_trig is not None:
        sel = okt[(okt >= t_trig - int(1e9)) & (okt <= w_end)]
        m["downtime_ms"] = float(np.diff(sel).max() / 1e6) if len(sel) > 1 else float("nan")
    else:
        m["downtime_ms"] = float("nan")
    m["failure_window_ms"] = longest_fail_streak(post, ok) / 1e6
    ref = post[post.err_class == "refused"].intended_ns
    m["refused_window_ms"] = float((ref.max() - ref.min()) / 1e6 + 1000.0 / meta["rate"]) if len(ref) else 0.0

    # ---- latency (successful, work_ms==0 "short" requests; ms, measured from intended send time)
    def lat(df, short=True):
        x = df[(df.err_class == "none")]
        if short:
            x = x[x.work_ms == 0]
        return x.latency_ns / 1e6
    for name, df in [("steady", steady), ("upg", upw)]:
        s = lat(df)
        m[f"lat_{name}_n"] = len(s)
        m[f"lat_{name}_p50_ms"] = q(s, 0.50)
        m[f"lat_{name}_p99_ms"] = q(s, 0.99)
        m[f"lat_{name}_p999_ms"] = q(s, 0.999)
        m[f"lat_{name}_max_ms"] = float(s.max()) if len(s) else float("nan")
    longs = upw[(upw.err_class == "none") & (upw.work_ms > 0)]
    m["lat_upg_long_max_ms"] = float(longs.latency_ns.max() / 1e6) if len(longs) else float("nan")
    m["long_requests_upg"] = int((upw.work_ms > 0).sum())
    m["long_requests_upg_failed"] = int(((upw.work_ms > 0) & (upw.err_class != "none")).sum())

    # ---- upgrade timing from server events
    durs = [(b["ts_ns"] - a["ts_ns"]) / 1e6 for a, b in spans if b["ev"] == "upgrade_done"]
    m["upgrades_triggered"] = len(trig_ts)
    m["upgrade_start_events"] = sum(1 for e in ev if e["ev"] == "upgrade_start")
    m["upgrade_done_events"] = sum(1 for e in ev if e["ev"] == "upgrade_done")
    m["upgrade_failed_events"] = sum(1 for e in ev if e["ev"] == "upgrade_failed")
    m["upgrade_duration_ms"] = float(np.mean(durs)) if durs else float("nan")
    m["upgrade_duration_max_ms"] = float(np.max(durs)) if durs else float("nan")
    if spans:
        m["trigger_to_start_ms"] = (spans[0][0]["ts_ns"] - t_trig) / 1e6 if t_trig else float("nan")

    # ---- version mix: per swap, last response of old version minus first response of new
    # version (server ts), counting only responses between this trigger and the next one.
    okp = post[ok & (post.server_ts_ns > 0)]
    mixes, first_new = [], []
    targets = list(trig["target"]) if len(trig) else []
    for i, ts in enumerate(trig_ts):
        nxt = trig_ts[i + 1] if i + 1 < len(trig_ts) else okp.server_ts_ns.max() + 1
        seg = okp[(okp.server_ts_ns >= ts) & (okp.server_ts_ns < nxt)]
        before = okp[okp.server_ts_ns < ts]
        if not len(seg) or not len(before):
            continue
        old_v = before.sort_values("server_ts_ns").version.iloc[-1]
        new_v = str(targets[i]) if i < len(targets) else None
        new = seg[seg.version == new_v] if new_v else seg[seg.version != old_v]
        if not len(new):
            continue
        fn = new.server_ts_ns.min()
        first_new.append((fn - ts) / 1e6)
        old_after = seg[(seg.version == old_v) & (seg.server_ts_ns >= fn)]
        mixes.append((old_after.server_ts_ns.max() - fn) / 1e6 if len(old_after) else 0.0)
    m["swaps_observed"] = len(first_new)
    m["time_to_new_version_ms"] = float(np.mean(first_new)) if first_new else float("nan")
    m["version_mix_ms"] = float(np.mean(mixes)) if mixes else float("nan")
    m["version_mix_max_ms"] = float(np.max(mixes)) if mixes else float("nan")
    m["versions_seen"] = " ".join(sorted(set(post.loc[ok, "version"]) - {""}))
    m["pids_seen"] = int(post.loc[ok, "pid"].nunique())
    if meta.get("expect_fail"):
        bad = set(str(t) for t in targets)
        m["rollback_ok"] = bool(m["upgrade_failed_events"] >= 1 and not (set(post.loc[ok, "version"]) & bad)
                                and m["n_errors"] == 0)

    # ---- state continuity (counter), ordered by server timestamp
    # Reset/regression detection uses only work_ms=0 responses: S6 assigns the count when a request is
    # admitted, and S4/S5 reserve count values for requests in flight at the handoff seal, so a long
    # (work_ms>0) request's count can lag its response timestamp by ~rate*work_ms increments. That is
    # reordering, not state loss. Duplicates and lost increments below use all successful responses.
    s_all = okp.sort_values(["server_ts_ns", "count"])["count"].to_numpy()
    s = okp[okp.work_ms == 0].sort_values(["server_ts_ns", "count"])["count"].to_numpy()
    # Tolerance for increment-vs-timestamp reordering under concurrency: the number of requests
    # that can be in flight within 50 ms at the offered rate (min 16), capped by the concurrency cap.
    # (Using the cap alone (512-2048) hid resets at low rates, e.g. B0 in E5 where the counter
    # only reaches ~200-400 between restarts.)
    J = int(min(meta["concurrency"], max(16, math.ceil(meta["rate"] * 0.05))))
    m["count_tolerance"] = J
    resets = regress = 0
    run_max = -1
    for c in s:
        if run_max >= 0 and c < run_max - J:
            if c < 0.1 * run_max:
                resets += 1
            else:
                regress += 1
            run_max = c
        else:
            run_max = max(run_max, c)
    m["count_resets"] = resets
    m["count_regressions"] = regress
    m["count_duplicates"] = int(len(s_all) - len(np.unique(s_all)))
    m["count_lost_increments"] = int(s_all.max() - s_all.min() + 1 - len(np.unique(s_all))) if len(s_all) and resets == 0 else float("nan")

    # ---- memory (sum over all processes of the server session)
    if len(rss):
        tot = rss.groupby("ts_ns").agg(rss_kb=("rss_kb", "sum"), nproc=("pid", "nunique")).reset_index()
        m["max_procs"] = int(tot.nproc.max())
        pre = tot[(tot.ts_ns >= ms) & ((tot.ts_ns < t_trig) if t_trig else True)]
        m["rss_steady_mb"] = float(pre.rss_kb.median() / 1024) if len(pre) else float("nan")
        m["rss_peak_mb"] = float(tot.rss_kb.max() / 1024)
        last = tot[tot.ts_ns >= tot.ts_ns.max() - int(2e9)]
        m["rss_end_mb"] = float(last.rss_kb.median() / 1024)
        m["rss_growth_mb"] = m["rss_end_mb"] - m["rss_steady_mb"]
        if len(trig_ts) >= 3:
            # RSS just before each trigger (after the previous upgrade settled)
            pts = []
            for i, ts in enumerate(trig_ts[1:], start=1):
                w = tot[(tot.ts_ns < ts) & (tot.ts_ns >= ts - int(5e8))]
                if len(w):
                    pts.append((i, w.rss_kb.iloc[-1]))
            if len(pts) >= 3:
                x, y = np.array(pts).T
                m["rss_slope_kb_per_upgrade"] = float(np.polyfit(x, y, 1)[0])
    return m, (meta, lg, ev, trig, rss)


# --------------------------------------------------------------------------- plots
def _style(ax):
    ax.grid(True, color=GRID, linewidth=0.6)
    ax.set_axisbelow(True)
    for s in ["top", "right"]:
        ax.spines[s].set_visible(False)
    for s in ["left", "bottom"]:
        ax.spines[s].set_color(INK2)
    ax.tick_params(colors=INK2, labelsize=8)


def plot_run(m, data, outdir):
    import matplotlib.pyplot as plt
    meta, lg, ev, trig, rss = data
    os.makedirs(outdir, exist_ok=True)
    ref = trig["ts_ns"].iloc[0] if len(trig) else meta["t0"]
    t = lambda ns: (np.asarray(ns) - ref) / 1e9  # noqa: E731
    fig, axs = plt.subplots(3, 1, figsize=(10, 7.5), sharex=True, gridspec_kw={"height_ratios": [3, 1.4, 1.2]})
    ax = axs[0]
    okd = lg[lg.err_class == "none"]
    sh, lo = okd[okd.work_ms == 0], okd[okd.work_ms > 0]
    ax.scatter(t(sh.intended_ns), sh.latency_ns / 1e6, s=3, color=PALETTE[0], alpha=.5, lw=0, label="ok, work_ms=0")
    if len(lo):
        ax.scatter(t(lo.intended_ns), lo.latency_ns / 1e6, s=3, color=PALETTE[2], alpha=.5, lw=0, label="ok, work_ms>0")
    bad = lg[lg.err_class != "none"]
    for c, g in bad.groupby("err_class"):
        ax.scatter(t(g.intended_ns), np.maximum(g.latency_ns / 1e6, 0.01), s=10, marker="x",
                   color=ERR_COLORS.get(c, ERR_COLORS["other"]), label=f"error: {c}")
    ax.set_yscale("log")
    ax.set_ylabel("latency from intended send (ms)", color=INK2, fontsize=9)
    title = f"{m['solution']} / {m['experiment']} / rep {m['rep']}: {meta['rate']:.0f} req/s, keep-alive={'on' if meta['keepalive'] else 'off'}"
    ax.set_title(title, loc="left", fontsize=10, color=INK)
    marks = {"upgrade_start": ("start", INK), "new_version_serving": ("new serving", PALETTE[6]),
             "upgrade_done": ("done", PALETTE[2]), "upgrade_failed": ("FAILED", PALETTE[7])}
    for a in axs:
        _style(a)
        for e in ev:
            if e["ev"] in marks and len(trig) <= 5:
                a.axvline(t(e["ts_ns"]), color=marks[e["ev"]][1], lw=1, ls="--", alpha=.8)
        if len(trig) > 5:
            for ts in trig.ts_ns:
                a.axvline(t(ts), color=INK2, lw=.4, alpha=.4)
    if len(trig) <= 5:
        ymax = ax.get_ylim()[1]
        for e in ev:
            if e["ev"] in ("upgrade_start", "upgrade_done", "upgrade_failed"):
                ax.text(t(e["ts_ns"]), ymax, " " + marks[e["ev"]][0], fontsize=7, color=INK2, va="top", rotation=90)
    ax.legend(fontsize=7, loc="upper left", frameon=False, markerscale=3)
    # errors / throughput per 100 ms bin
    b = 0.1
    lo_t, hi_t = t(lg.intended_ns.min()), t(lg.intended_ns.max())
    bins = np.arange(lo_t, hi_t + b, b)
    ax = axs[1]
    okc, _ = np.histogram(t(okd.end_ns), bins=bins)
    ax.plot(bins[:-1] + b / 2, okc / b, color=PALETTE[0], lw=1.2, label="successful responses/s")
    bottom = np.zeros(len(bins) - 1)
    for c, g in bad.groupby("err_class"):
        h, _ = np.histogram(t(g.intended_ns), bins=bins)
        ax.bar(bins[:-1], h / b, width=b, bottom=bottom, align="edge", color=ERR_COLORS.get(c, ERR_COLORS["other"]),
               label=f"{c}/s")
        bottom += h / b
    ax.set_ylim(bottom=0)
    ax.set_ylabel("per second", color=INK2, fontsize=9)
    ax.legend(fontsize=7, loc="lower left", frameon=False, ncol=4)
    # version share
    ax = axs[2]
    vers = sorted(set(okd.version) - {""})
    for i, v in enumerate(vers):
        h, _ = np.histogram(t(okd[okd.version == v].end_ns), bins=bins)
        ax.plot(bins[:-1] + b / 2, h / np.maximum(okc, 1), color=PALETTE[(4 + i * 2) % 8], lw=1.4, label=v)
    ax.set_ylim(-0.05, 1.05)
    ax.set_ylabel("version share", color=INK2, fontsize=9)
    ax.set_xlabel("time relative to first upgrade trigger (s)", color=INK2, fontsize=9)
    ax.legend(fontsize=7, loc="center left", frameon=False)
    fig.tight_layout()
    fig.savefig(os.path.join(outdir, "timeline.png"), dpi=130)
    plt.close(fig)

    if len(rss):
        tot = rss.groupby("ts_ns").rss_kb.sum().reset_index()
        fig, ax = plt.subplots(figsize=(10, 3))
        _style(ax)
        ax.plot(t(tot.ts_ns), tot.rss_kb / 1024, color=PALETTE[0], lw=1.5)
        for ts in trig.ts_ns if len(trig) else []:
            ax.axvline(t(ts), color=INK2, lw=.5, alpha=.5)
        ax.set_ylabel("RSS of server session (MiB)", color=INK2, fontsize=9)
        ax.set_xlabel("time relative to first upgrade trigger (s); grey lines = triggers", color=INK2, fontsize=9)
        ax.set_title(f"{m['solution']} / {m['experiment']} / rep {m['rep']}: memory", loc="left", fontsize=10, color=INK)
        fig.tight_layout()
        fig.savefig(os.path.join(outdir, "rss.png"), dpi=130)
        plt.close(fig)


def plot_cdf(exp, groups, outdir):
    """groups: {solution: (steady_latencies_ms, upgrade_window_latencies_ms)} pooled over reps."""
    import matplotlib.pyplot as plt
    fig, ax = plt.subplots(figsize=(8, 4.5))
    _style(ax)
    for i, (sol, (st, up)) in enumerate(sorted(groups.items())):
        col = PALETTE[i % 8]
        for arr, ls, lab in [(up, "-", "upgrade window"), (st, ":", "steady")]:
            if len(arr):
                x = np.sort(arr)
                y = np.arange(1, len(x) + 1) / len(x)
                ax.plot(x, y, ls=ls, color=col, lw=1.6 if ls == "-" else 1.2, label=f"{sol}: {lab}")
    ax.set_xscale("log")
    ax.set_xlabel("latency of successful work_ms=0 requests (ms, from intended send)", color=INK2, fontsize=9)
    ax.set_ylabel("CDF", color=INK2, fontsize=9)
    ax.set_title(f"{exp}: latency CDF, steady state vs upgrade window (pooled reps)", loc="left", fontsize=10, color=INK)
    ax.legend(fontsize=7, frameon=False, loc="lower right")
    fig.tight_layout()
    os.makedirs(outdir, exist_ok=True)
    fig.savefig(os.path.join(outdir, f"cdf_{exp}.png"), dpi=130)
    plt.close(fig)


# --------------------------------------------------------------------------- aggregation
KEY_METRICS = [
    ("error_rate", "error rate", "{:.2e}"), ("n_errors", "errors", "{:.1f}"),
    ("downtime_ms", "downtime ms", "{:.1f}"), ("steady_max_success_gap_ms", "steady max gap ms", "{:.1f}"), ("failure_window_ms", "fail window ms", "{:.1f}"),
    ("refused_window_ms", "refused window ms", "{:.1f}"),
    ("lat_steady_p99_ms", "p99 steady ms", "{:.2f}"), ("lat_upg_p99_ms", "p99 upg ms", "{:.2f}"),
    ("lat_upg_p999_ms", "p99.9 upg ms", "{:.2f}"), ("lat_upg_max_ms", "max upg ms", "{:.1f}"),
    ("upgrade_duration_ms", "upgrade ms", "{:.1f}"), ("version_mix_ms", "mix ms", "{:.1f}"),
    ("count_resets", "resets", "{:.1f}"), ("count_duplicates", "dups", "{:.1f}"),
    ("rss_growth_mb", "RSS growth MiB", "{:.2f}"), ("rss_slope_kb_per_upgrade", "RSS KiB/upgrade", "{:.1f}"),
    ("client_lag_p99_ms", "client lag p99 ms", "{:.2f}"),
]


def aggregate(runs):
    rows = []
    num = runs.select_dtypes(include=[np.number, bool]).columns.drop(["rep"], errors="ignore")
    for (sol, exp), g in runs.groupby(["solution", "experiment"]):
        for c in num:
            x = pd.to_numeric(g[c], errors="coerce").dropna().astype(float)
            if not len(x):
                continue
            n, mu = len(x), x.mean()
            sd = x.std(ddof=1) if n > 1 else float("nan")
            h = tq(n - 1) * sd / math.sqrt(n) if n > 1 else float("nan")
            rows.append(dict(solution=sol, experiment=exp, metric=c, n=n, mean=mu, std=sd,
                             ci95_lo=mu - h if n > 1 else float("nan"), ci95_hi=mu + h if n > 1 else float("nan"),
                             min=x.min(), max=x.max()))
    return pd.DataFrame(rows)


def fmt(fs, mu, lo, hi):
    if mu is None or (isinstance(mu, float) and math.isnan(mu)):
        return "n/a"
    s = fs.format(mu)
    if lo is not None and not math.isnan(lo):
        s += " ± " + fs.format((hi - lo) / 2)
    return s


def write_md(runs, agg, path, plots_rel):
    L = ["# DIMP benchmark summary", "",
         "Generated by `bench/analysis/analyze.py`. Values: mean ± 95% CI half-width (Student t) over repetitions;",
         "`n/a` = not applicable / not observable. Metric definitions: `docs/evaluation-methodology.md`.", ""]
    for exp in sorted(agg.experiment.unique()):
        a = agg[agg.experiment == exp]
        sols = sorted(a.solution.unique())
        L += [f"## {exp}", ""]
        reps = ", ".join(f"{s}: n={int(runs[(runs.solution == s) & (runs.experiment == exp)].shape[0])}" for s in sols)
        L += [f"Repetitions: {reps}", ""]
        L.append("| metric | " + " | ".join(sols) + " |")
        L.append("|---|" + "---|" * len(sols))
        for key, label, fs in KEY_METRICS:
            cells = []
            for s in sols:
                r = a[(a.solution == s) & (a.metric == key)]
                cells.append(fmt(fs, *r[["mean", "ci95_lo", "ci95_hi"]].iloc[0].tolist()) if len(r) else "n/a")
            if all(c == "n/a" for c in cells):
                continue
            L.append(f"| {label} | " + " | ".join(cells) + " |")
        if "rollback_ok" in runs.columns:
            rb = runs[runs.experiment == exp]
            if rb.rollback_ok.notna().any():
                L.append("| rollback ok (runs) | " + " | ".join(
                    f"{int((rb[rb.solution == s].rollback_ok == True).sum())}/{int((rb.solution == s).sum())}"
                    for s in sols) + " |")
        L.append("| versions seen | " + " | ".join(
            ";".join(sorted(set(runs[(runs.solution == s) & (runs.experiment == exp)].versions_seen))) for s in sols) + " |")
        L.append("")
        L.append(f"![CDF {exp}]({plots_rel}/cdf_{exp}.png)")
        L.append("")
    open(path, "w").write("\n".join(L) + "\n")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--raw", default="results/raw")
    ap.add_argument("--out", default="results")
    ap.add_argument("--plots", default="first", choices=["first", "all", "none"])
    a = ap.parse_args()
    dirs = sorted(os.path.dirname(p) for p in glob.glob(os.path.join(a.raw, "*", "*", "metadata.json")))
    if not dirs:
        print("no runs found under", a.raw, file=sys.stderr)
        return 1
    rows, cdf = [], {}
    plots = os.path.join(a.out, "plots")
    for d in dirs:
        try:
            m, data = analyze_run(d)
        except Exception as ex:  # keep going; report broken runs
            print(f"[skip] {d}: {ex}", file=sys.stderr)
            continue
        rows.append(m)
        _, lg, _, trig, _ = data
        if a.plots == "all" or (a.plots == "first" and m["rep"] == 1):
            plot_run(m, data, os.path.join(plots, m["solution"], m["run_id"]))
        meta = data[0]
        ms = meta["t0"] + int(meta["warmup_s"] * 1e9)
        okd = lg[(lg.err_class == "none") & (lg.work_ms == 0) & (lg.intended_ns >= ms)]
        if len(trig):
            t1 = trig.ts_ns.iloc[0]
            w_end = t1 + int(m["upgrade_window_s"] * 1e9) if not math.isnan(m["upgrade_window_s"]) else t1
            st = okd[okd.intended_ns < t1].latency_ns / 1e6
            up = okd[(okd.intended_ns >= t1) & (okd.intended_ns <= w_end)].latency_ns / 1e6
        else:
            st, up = okd.latency_ns / 1e6, pd.Series(dtype=float)
        g = cdf.setdefault(m["experiment"], {}).setdefault(m["solution"], ([], []))
        g[0].extend(st.tolist())
        g[1].extend(up.tolist())
    runs = pd.DataFrame(rows)
    if runs.empty:
        print("no analysable runs", file=sys.stderr)
        return 1
    os.makedirs(a.out, exist_ok=True)
    runs.to_csv(os.path.join(a.out, "summary_runs.csv"), index=False)
    agg = aggregate(runs)
    agg.to_csv(os.path.join(a.out, "summary.csv"), index=False)
    if a.plots != "none":
        for exp, groups in cdf.items():
            plot_cdf(exp, {s: (np.array(v[0]), np.array(v[1])) for s, v in groups.items()}, plots)
    write_md(runs, agg, os.path.join(a.out, "summary.md"), "plots")
    print(f"analysed {len(runs)} runs -> {a.out}/summary.csv, summary.md, plots/")
    return 0


if __name__ == "__main__":
    sys.exit(main())
