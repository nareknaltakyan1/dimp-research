#!/usr/bin/env python3
"""Cross-solution report for the final campaign (run AFTER analyze.py).

Reads results/summary_runs.csv (per-run metrics from analyze.py) and the raw
runs, and writes:
  results/report_tables.md                 main comparison + per-RQ tables (mean ± 95% CI)
  results/plots/availability.png           errors per run, per experiment (small multiples)
  results/plots/downtime.png               downtime per experiment (small multiples)
  results/plots/timeline_<exp>.png         latency over time around the upgrade, rep 1 of each solution
  results/plots/cdf_upgrade_<exp>.png      latency CDF in the upgrade window, by solution family
  results/plots/e5_rss.png                 session RSS before each upgrade vs upgrade index (E5)
Usage: python3 bench/analysis/report.py [--raw results/raw] [--out results]
"""
import argparse
import math
import os

import numpy as np
import pandas as pd

from analyze import PALETTE, INK, INK2, GRID, ERR_COLORS, _style, load_run, tq

ORDER = ["b0-naive-restart", "s1-epoch-swap", "s2-plugin", "s3-wasm", "s4-reuseport", "s4-reuseport+migrate",
         "s5-fdpass", "s6-hotpatch", "s7-inplace", "k8s-naive", "k8s-prestop",
         "s4-reuseport+crash", "s5-fdpass+crash", "s7-inplace+tampered"]
SHORT = {"b0-naive-restart": "B0 restart", "s1-epoch-swap": "S1 epoch", "s2-plugin": "S2 plugin", "s3-wasm": "S3 wasm",
         "s4-reuseport": "S4 reuseport", "s4-reuseport+migrate": "S4 +migrate", "s5-fdpass": "S5 fdpass",
         "s6-hotpatch": "S6 hotpatch", "s7-inplace": "S7 inplace", "k8s-naive": "B1 k8s naive",
         "k8s-prestop": "B1 k8s preStop", "s4-reuseport+crash": "S4 (crash)", "s5-fdpass+crash": "S5 (crash)",
         "s7-inplace+tampered": "S7 (tampered)"}
FAMILIES = [("in-process code swap", ["s1-epoch-swap", "s2-plugin", "s3-wasm", "s6-hotpatch", "s7-inplace"]),
            ("process restart / handoff", ["b0-naive-restart", "s4-reuseport", "s4-reuseport+migrate", "s5-fdpass"]),
            ("Kubernetes rolling update (B1)", ["k8s-naive", "k8s-prestop"])]
EXPS = ["E1", "E2", "E3", "E4-ka", "E4-noka", "E5", "E6"]
BAR = PALETTE[0]


def order(sols):
    return [s for s in ORDER if s in sols] + sorted(s for s in sols if s not in ORDER)


def ci(x):
    x = pd.to_numeric(x, errors="coerce").dropna().astype(float)
    n = len(x)
    if n == 0:
        return float("nan"), float("nan"), 0
    if n == 1:
        return float(x.iloc[0]), float("nan"), 1
    return float(x.mean()), tq(n - 1) * float(x.std(ddof=1)) / math.sqrt(n), n


def fmt(x, fs="{:.1f}"):
    mu, h, n = ci(x)
    if n == 0 or math.isnan(mu):
        return "n/a"
    return fs.format(mu) + ("" if math.isnan(h) else " ± " + fs.format(h))


# --------------------------------------------------------------------------- tables
def tables(runs, out):
    L = ["# Cross-solution tables (final campaign)", "",
         "Mean ± 95% CI half-width (Student t) over repetitions; the run is the unit of replication.",
         "Errors are also given as total failed / total measured requests over all runs.", ""]

    def errs(g):
        return f"{int(g.n_errors.sum())}/{int(g.n_requests.sum())}"

    def state(g):
        bad = g[(g.count_resets > 0) | (g.count_regressions > 0) | (g.count_duplicates > 0)]
        return "yes" if len(bad) == 0 else f"NO ({len(bad)}/{len(g)} runs)"

    # main comparison: E1 numbers + state over E1-E4 + rollback from E6
    L += ["## Main comparison (E1: 200 req/s, keep-alive, 1 upgrade v1->v2)", "",
          "| solution | errors (total) | downtime ms | p99 upgrade window ms | upgrade duration ms | state preserved (E1-E4) | rollback (E6 rollback_ok) |",
          "|---|---|---|---|---|---|---|"]
    for s in order(runs.solution.unique()):
        if s in ("s4-reuseport+crash", "s5-fdpass+crash", "s7-inplace+tampered"):
            continue
        g1 = runs[(runs.solution == s) & (runs.experiment == "E1")]
        if not len(g1):
            continue
        g14 = runs[(runs.solution == s) & runs.experiment.isin(["E1", "E2", "E3", "E4-ka", "E4-noka"])]
        g6 = runs[(runs.solution == s) & (runs.experiment == "E6")]
        rb = f"{int((g6.rollback_ok == True).sum())}/{len(g6)}" if len(g6) else "n/a (not run)"
        L.append(f"| {s} | {errs(g1)} | {fmt(g1.downtime_ms)} | {fmt(g1.lat_upg_p99_ms, '{:.2f}')} | "
                 f"{fmt(g1.upgrade_duration_ms)} | {state(g14)} | {rb} |")
    L.append("")

    # per experiment availability / latency / duration
    for exp in EXPS:
        g = runs[runs.experiment == exp]
        if not len(g):
            continue
        L += [f"## {exp}", "",
              "| solution | n | errors (total) | error classes | downtime ms | fail window ms | p99 steady ms | p99 upg ms | p99.9 upg ms | max upg ms | upgrade ms | time to new ver ms | mix ms | resets/regr/dups (sum) | rollback_ok |",
              "|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|"]
        for s in order(g.solution.unique()):
            x = g[g.solution == s]
            cls = {c: int(x[f"err_{c}"].sum()) for c in ["refused", "reset", "timeout", "eof", "other"]}
            cls["http"] = int(x.err_http.sum())
            cl = ", ".join(f"{k} {v}" for k, v in cls.items() if v) or "-"
            rb = f"{int((x.rollback_ok == True).sum())}/{len(x)}" if exp == "E6" else ""
            L.append(f"| {s} | {len(x)} | {errs(x)} | {cl} | {fmt(x.downtime_ms)} | {fmt(x.failure_window_ms)} | "
                     f"{fmt(x.lat_steady_p99_ms, '{:.2f}')} | {fmt(x.lat_upg_p99_ms, '{:.2f}')} | "
                     f"{fmt(x.lat_upg_p999_ms, '{:.2f}')} | {fmt(x.lat_upg_max_ms)} | {fmt(x.upgrade_duration_ms)} | "
                     f"{fmt(x.time_to_new_version_ms)} | {fmt(x.version_mix_ms)} | "
                     f"{int(x.count_resets.sum())}/{int(x.count_regressions.sum())}/{int(x.count_duplicates.sum())} | {rb} |")
        L.append("")

    # state continuity table
    L += ["## State continuity (all experiments; sums over runs)", "",
          "resets / regressions / duplicate counts / lost increments (lost only defined without reset). "
          "A state-preserving upgrade shows 0/0/0.", "",
          "| solution | " + " | ".join(EXPS) + " |", "|---|" + "---|" * len(EXPS)]
    for s in order(runs.solution.unique()):
        cells = []
        for exp in EXPS:
            x = runs[(runs.solution == s) & (runs.experiment == exp)]
            if not len(x):
                cells.append("")
                continue
            lost = pd.to_numeric(x.count_lost_increments, errors="coerce")
            ls = "n/a" if lost.isna().any() else str(int(lost.sum()))
            cells.append(f"{int(x.count_resets.sum())}/{int(x.count_regressions.sum())}/{int(x.count_duplicates.sum())}/{ls}")
        L.append(f"| {s} | " + " | ".join(cells) + " |")
    L.append("")

    # resources (E5)
    g = runs[runs.experiment == "E5"]
    if len(g):
        L += ["## Resources across repeated upgrades (E5)", "",
              "| solution | n | upgrades done/failed per run | RSS steady MiB | RSS end MiB | RSS growth MiB | RSS slope KiB/upgrade | max procs | errors (total) | upgrade ms |",
              "|---|---|---|---|---|---|---|---|---|---|"]
        for s in order(g.solution.unique()):
            x = g[g.solution == s]
            ud = "; ".join(f"{int(a)}/{int(b)}" for a, b in zip(x.upgrade_done_events, x.upgrade_failed_events))
            L.append(f"| {s} | {len(x)} | {ud} | {fmt(x.rss_steady_mb)} | {fmt(x.rss_end_mb)} | {fmt(x.rss_growth_mb)} | "
                     f"{fmt(x.rss_slope_kb_per_upgrade)} | {int(x.max_procs.max()) if x.max_procs.notna().any() else 'n/a'} | "
                     f"{errs(x)} | {fmt(x.upgrade_duration_ms)} |")
        L.append("")
    open(os.path.join(out, "report_tables.md"), "w").write("\n".join(L) + "\n")


# --------------------------------------------------------------------------- plots
def small_multiples_bars(runs, metric, title, xlabel, path, symlog=False, exps=("E1", "E2", "E3", "E4-ka", "E4-noka", "E6")):
    import matplotlib.pyplot as plt
    exps = [e for e in exps if (runs.experiment == e).any()]
    sols = order(runs[runs.experiment.isin(exps)].solution.unique())
    fig, axs = plt.subplots(1, len(exps), figsize=(2.6 * len(exps) + 1.6, 0.32 * len(sols) + 1.4), sharey=True)
    axs = np.atleast_1d(axs)
    y = np.arange(len(sols))
    for ax, exp in zip(axs, exps):
        _style(ax)
        ax.grid(False, axis="y")
        for i, s in enumerate(sols):
            x = pd.to_numeric(runs[(runs.solution == s) & (runs.experiment == exp)][metric], errors="coerce").dropna()
            if not len(x):
                ax.text(0, i, " not run", va="center", fontsize=7, color=INK2)
                continue
            mu, lo, hi = x.mean(), x.min(), x.max()
            ax.barh(i, mu, height=0.62, color=BAR, edgecolor="white", linewidth=1)
            if hi > lo:
                ax.plot([lo, hi], [i, i], color=INK, lw=1)
            lab = f"{mu:.0f}" if mu >= 10 else f"{mu:.1f}" if mu > 0 else "0"
            ax.text(max(mu, hi) if mu > 0 else 0, i, " " + lab, va="center", fontsize=7, color=INK2)
        if symlog:
            ax.set_xscale("symlog", linthresh=1)
            ax.set_xlim(left=0)
        ax.set_title(exp, loc="left", fontsize=9, color=INK)
        ax.set_xlabel(xlabel, fontsize=8, color=INK2)
        ax.margins(x=0.25)
    axs[0].set_yticks(y)
    axs[0].set_yticklabels([SHORT.get(s, s) for s in sols], fontsize=8)
    axs[0].invert_yaxis()
    fig.suptitle(title, x=0.01, ha="left", fontsize=10, color=INK)
    fig.tight_layout()
    fig.savefig(path, dpi=140)
    plt.close(fig)


def timelines(runs, exp, path, window=(-1.0, 6.0)):
    import matplotlib.pyplot as plt
    g = runs[(runs.experiment == exp) & (runs.rep == 1)]
    sols = order(g.solution.unique())
    if not sols:
        return
    nc = 4
    nr = math.ceil(len(sols) / nc)
    fig, axs = plt.subplots(nr, nc, figsize=(13, 2.5 * nr), sharex=True, sharey=True)
    axs = np.atleast_1d(axs).ravel()
    for ax, s in zip(axs, sols):
        _style(ax)
        meta, lg, ev, trig, _ = load_run(g[g.solution == s].run_dir.iloc[0])
        ref = trig.ts_ns.iloc[0]
        t = (lg.intended_ns - ref) / 1e9
        sel = (t >= window[0]) & (t <= window[1])
        d, tt = lg[sel], t[sel]
        ok = d.err_class == "none"
        ax.scatter(tt[ok & (d.work_ms == 0)], d[ok & (d.work_ms == 0)].latency_ns / 1e6, s=2, color=PALETTE[0], lw=0, alpha=.6)
        if (ok & (d.work_ms > 0)).any():
            ax.scatter(tt[ok & (d.work_ms > 0)], d[ok & (d.work_ms > 0)].latency_ns / 1e6, s=2, color=PALETTE[2], lw=0, alpha=.6)
        nbad = int((~ok).sum())
        if nbad:
            ax.scatter(tt[~ok], np.full(nbad, 0.05), s=14, marker="x", color=ERR_COLORS["reset"], lw=1)
        for e in ev:
            if e["ev"] in ("upgrade_done", "upgrade_failed"):
                ax.axvline((e["ts_ns"] - ref) / 1e9, color=PALETTE[2] if e["ev"] == "upgrade_done" else PALETTE[7],
                           lw=1, ls="--")
        ax.axvline(0, color=INK, lw=1, ls="--")
        ax.set_yscale("log")
        ax.set_ylim(0.03, 3000)
        ax.set_title(f"{SHORT.get(s, s)}: {nbad} errors", loc="left", fontsize=8, color=INK)
    for ax in axs[len(sols):]:
        ax.set_visible(False)
    for ax in axs[::nc]:
        ax.set_ylabel("latency ms (log)", fontsize=8, color=INK2)
    for ax in axs[-nc:]:
        ax.set_xlabel("s from upgrade trigger", fontsize=8, color=INK2)
    fig.suptitle(f"{exp}: latency of each request vs intended send time, rep 1 (blue: work_ms=0, green: long, red x at bottom: failed; "
                 "black dashed: trigger, green/red dashed: upgrade done/failed)", x=0.01, ha="left", fontsize=9, color=INK)
    fig.tight_layout()
    fig.savefig(path, dpi=130)
    plt.close(fig)


def cdfs(runs, exp, path):
    import matplotlib.pyplot as plt
    g = runs[runs.experiment == exp]
    fams = [(n, [s for s in m if (g.solution == s).any()]) for n, m in FAMILIES]
    fams = [f for f in fams if f[1]]
    if not fams:
        return
    fig, axs = plt.subplots(1, len(fams), figsize=(4.6 * len(fams), 3.8), sharey=True)
    axs = np.atleast_1d(axs)
    for ax, (name, sols) in zip(axs, fams):
        _style(ax)
        for i, s in enumerate(sols):
            up, st = [], []
            for _, r in g[g.solution == s].iterrows():
                meta, lg, ev, trig, _ = load_run(r.run_dir)
                ms = meta["t0"] + int(meta["warmup_s"] * 1e9)
                okd = lg[(lg.err_class == "none") & (lg.work_ms == 0) & (lg.intended_ns >= ms)]
                t1 = trig.ts_ns.iloc[0]
                we = t1 + int(r.upgrade_window_s * 1e9)
                up += list(okd[(okd.intended_ns >= t1) & (okd.intended_ns <= we)].latency_ns / 1e6)
                st += list(okd[okd.intended_ns < t1].latency_ns / 1e6)
            col = PALETTE[i]
            for arr, ls, lw in [(up, "-", 1.8), (st, ":", 1.0)]:
                if arr:
                    x = np.sort(arr)
                    ax.plot(x, np.arange(1, len(x) + 1) / len(x), ls=ls, lw=lw, color=col,
                            label=f"{SHORT.get(s, s)}" if ls == "-" else None)
        ax.set_xscale("log")
        ax.set_title(name, loc="left", fontsize=9, color=INK)
        ax.set_xlabel("latency ms (successful work_ms=0)", fontsize=8, color=INK2)
        ax.legend(fontsize=7, frameon=False, loc="lower right")
    axs[0].set_ylabel("CDF", fontsize=8, color=INK2)
    fig.suptitle(f"{exp}: latency CDF in the upgrade window (solid) vs steady state (dotted), pooled over reps",
                 x=0.01, ha="left", fontsize=9, color=INK)
    fig.tight_layout()
    fig.savefig(path, dpi=130)
    plt.close(fig)


def e5_rss(runs, path):
    import matplotlib.pyplot as plt
    g = runs[runs.experiment == "E5"]
    sols = order(g.solution.unique())
    if not sols:
        return
    nc = 4
    nr = math.ceil(len(sols) / nc)
    fig, axs = plt.subplots(nr, nc, figsize=(13, 2.6 * nr), sharex=True)
    axs = np.atleast_1d(axs).ravel()
    for ax, s in zip(axs, sols):
        _style(ax)
        for k, (_, r) in enumerate(g[g.solution == s].sort_values("rep").iterrows()):
            _, _, _, trig, rss = load_run(r.run_dir)
            if not len(rss):
                continue
            tot = rss.groupby("ts_ns").rss_kb.sum().reset_index()
            xs, ys = [], []
            for i, ts in enumerate(list(trig.ts_ns) + [tot.ts_ns.max() + 1]):
                w = tot[(tot.ts_ns < ts) & (tot.ts_ns >= ts - int(5e8))]
                if len(w):
                    xs.append(i)
                    ys.append(w.rss_kb.iloc[-1] / 1024)
            ax.plot(xs, ys, color=PALETTE[0], lw=1.8 if k == 0 else 1.2, ls="-" if k == 0 else "--",
                    label=f"rep {int(r.rep)}")
        ax.set_ylim(bottom=0)
        slope = pd.to_numeric(g[g.solution == s].rss_slope_kb_per_upgrade, errors="coerce").mean()
        ax.set_title(f"{SHORT.get(s, s)}: {slope:,.0f} KiB/upgrade", loc="left", fontsize=8, color=INK)
        ax.legend(fontsize=6, frameon=False, loc="lower right")
    for ax in axs[len(sols):]:
        ax.set_visible(False)
    for ax in axs[::nc]:
        ax.set_ylabel("session RSS MiB", fontsize=8, color=INK2)
    for ax in axs[-nc:]:
        ax.set_xlabel("upgrades completed", fontsize=8, color=INK2)
    fig.suptitle("E5: session RSS sampled just before each trigger, 50 repeated upgrades (own y-scale per panel; slope = least-squares fit, mean of 2 reps)",
                 x=0.01, ha="left", fontsize=9, color=INK)
    fig.tight_layout()
    fig.savefig(path, dpi=130)
    plt.close(fig)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", default="results")
    a = ap.parse_args()
    runs = pd.read_csv(os.path.join(a.out, "summary_runs.csv"))
    plots = os.path.join(a.out, "plots")
    os.makedirs(plots, exist_ok=True)
    tables(runs, a.out)
    small_multiples_bars(runs, "n_errors", "Failed requests per run (bar = mean, line = min-max over reps; symlog axis)",
                         "errors per run", os.path.join(plots, "availability.png"), symlog=True)
    small_multiples_bars(runs, "downtime_ms", "Downtime: longest gap between successful completions around the upgrade "
                         "(bar = mean, line = min-max; floor = inter-arrival time)", "downtime ms",
                         os.path.join(plots, "downtime.png"), symlog=True,
                         exps=("E1", "E2", "E3", "E4-ka", "E4-noka"))  # E6: downtime undefined after a permanent outage (B0)
    for exp in ["E1", "E3", "E4-noka", "E6"]:
        timelines(runs, exp, os.path.join(plots, f"timeline_{exp}.png"))
    for exp in ["E1", "E2", "E3", "E4-ka", "E4-noka"]:
        cdfs(runs, exp, os.path.join(plots, f"cdf_upgrade_{exp}.png"))
    e5_rss(runs, os.path.join(plots, "e5_rss.png"))
    print("report written:", os.path.join(a.out, "report_tables.md"), plots)


if __name__ == "__main__":
    main()
