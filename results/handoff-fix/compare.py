#!/usr/bin/env python3
"""Before/after comparison for the handoff keep-alive fix.

Before = campaign (results/summary.csv, results/raw/<label>/*-final-*).
After  = results/handoff-fix/analysis/summary.csv (produced by
         bench/analysis/analyze.py --raw results/handoff-fix/raw) and the raw
         runs in results/handoff-fix/raw/fix-<label>/.
Writes results/handoff-fix/summary.csv and prints a markdown table body.

Run inside the bench container:  python3 results/handoff-fix/compare.py
"""
import csv, glob, json, os, statistics as st

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
OUT = os.path.join(ROOT, "results/handoff-fix")
CELLS = [(l, e) for e in ["E1", "E4-ka", "E2", "E6"] for l in ["s4-reuseport", "s4-reuseport+migrate", "s5-fdpass"]] + \
        [("s4-reuseport+crash", "E6"), ("s5-fdpass+crash", "E6")]
METRICS = ["n_requests", "n_errors", "err_reset", "err_eof", "err_refused", "err_http", "upgrade_duration_ms",
           "lat_upg_p99_ms", "lat_upg_max_ms", "max_success_gap_ms", "count_duplicates", "count_lost_increments",
           "upgrade_failed_events", "upgrade_done_events"]


def load_summary(path):
    d = {}
    if not os.path.exists(path):
        return d
    for r in csv.DictReader(open(path)):
        d[(r["solution"], r["experiment"], r["metric"])] = r
    return d


def raw_runs(base, label, exp, tag):
    """Per-run error details from loadgen.csv (post warm-up), relative to the trigger."""
    out = []
    for d in sorted(glob.glob(os.path.join(base, label, f"{exp}-{tag}*"))):
        m = json.load(open(os.path.join(d, "metadata.json")))
        trig = [int(l.split(",")[1]) for l in open(os.path.join(d, "triggers.csv")).read().split("\n")[1:] if l]
        warm = m["t0"] + int(m["warmup_s"] * 1e9)
        errs = reused = 0
        offs = []
        with open(os.path.join(d, "loadgen.csv")) as f:
            for r in csv.DictReader(f):
                if int(r["intended_ns"]) < warm or r["err_class"] == "none":
                    continue
                errs += 1
                reused += r["conn_reused"] == "1"
                if trig:
                    offs.append((int(r["start_ns"]) - trig[0]) / 1e6)
        ev = [json.loads(l) for l in open(os.path.join(d, "events.jsonl")) if l.strip()]
        dr = [e for e in ev if e.get("ev") == "old_version_drained"]
        out.append({"errors": errs, "reused": reused, "off_min": min(offs) if offs else None,
                    "off_max": max(offs) if offs else None,
                    "drain_ms": dr[0].get("drain_ms") if dr else None,
                    "idle_closed": dr[0].get("idle_closed") if dr else None})
    return out


def main():
    before = load_summary(os.path.join(ROOT, "results/summary.csv"))
    after = load_summary(os.path.join(OUT, "analysis/summary.csv"))
    rows = []
    for label, exp in CELLS:
        rb = raw_runs(os.path.join(ROOT, "results/raw"), label, exp, "final")
        ra = raw_runs(os.path.join(OUT, "raw"), "fix-" + label, exp, "fix")
        for phase, summ, key, rr in [("before", before, label, rb), ("after", after, "fix-" + label, ra)]:
            row = {"cell": f"{label}:{exp}", "phase": phase, "runs": len(rr)}
            for mt in METRICS:
                r = summ.get((key, exp, mt))
                row[mt + "_mean"] = r["mean"] if r else ""
                row[mt + "_min"] = r["min"] if r else ""
                row[mt + "_max"] = r["max"] if r else ""
            row["errors_total"] = sum(x["errors"] for x in rr)
            row["errors_per_run"] = "/".join(str(x["errors"]) for x in rr)
            row["errors_on_reused_conns"] = sum(x["reused"] for x in rr)
            offs = [x for x in rr if x["off_min"] is not None]
            row["err_offset_ms_range"] = f"{min(x['off_min'] for x in offs):.1f}-{max(x['off_max'] for x in offs):.1f}" if offs else ""
            dms = [x["drain_ms"] for x in rr if x["drain_ms"] is not None]
            row["drain_ms_median"] = f"{st.median(dms):.1f}" if dms else ""
            ic = [x["idle_closed"] for x in rr if x["idle_closed"] is not None]
            row["idle_closed_per_run"] = "/".join(str(x) for x in ic)
            rows.append(row)
    keys = list(rows[0].keys())
    with open(os.path.join(OUT, "summary.csv"), "w", newline="") as f:
        w = csv.DictWriter(f, fieldnames=keys)
        w.writeheader()
        w.writerows(rows)

    def g(r, k, fmt="{:.1f}"):
        v = r.get(k, "")
        try:
            return fmt.format(float(v))
        except (TypeError, ValueError):
            return "-"
    print("| cell | phase | runs | errors/run | total | on reused conn | err offset ms | reset/eof (mean) | upgrade ms | upg p99 ms | upg max ms | dup/lost | failed ev | drain ms (median) | idle closed/run |")
    print("|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|")
    for r in rows:
        print(f"| {r['cell']} | {r['phase']} | {r['runs']} | {r['errors_per_run']} | {r['errors_total']} | {r['errors_on_reused_conns']} | "
              f"{r['err_offset_ms_range'] or '-'} | {g(r,'err_reset_mean')}/{g(r,'err_eof_mean')} | {g(r,'upgrade_duration_ms_mean')} | "
              f"{g(r,'lat_upg_p99_ms_mean','{:.2f}')} | {g(r,'lat_upg_max_ms_mean')} | {g(r,'count_duplicates_mean','{:.0f}')}/{g(r,'count_lost_increments_mean','{:.0f}')} | "
              f"{g(r,'upgrade_failed_events_mean','{:.0f}')} | {r['drain_ms_median'] or '-'} | {r['idle_closed_per_run'] or '-'} |")


if __name__ == "__main__":
    main()
