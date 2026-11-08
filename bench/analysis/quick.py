#!/usr/bin/env python3
"""Quick per-run check (integration smoke): prints the key metrics of the given
run directories (or every run under results/raw matching a substring).
  python3 bench/analysis/quick.py results/raw/s1-epoch-swap/E1-smoke-*"""
import glob, sys, os
sys.path.insert(0, os.path.dirname(__file__))
from analyze import analyze_run  # noqa: E402
keys = ["n_requests", "n_errors", "err_refused", "err_reset", "err_eof", "err_timeout", "err_http", "downtime_ms",
        "lat_upg_p99_ms", "lat_upg_max_ms", "upgrade_duration_ms", "upgrade_done_events", "upgrade_failed_events",
        "time_to_new_version_ms", "version_mix_ms", "versions_seen", "pids_seen", "count_resets", "count_regressions",
        "count_duplicates", "count_lost_increments", "rss_steady_mb", "rss_end_mb", "rss_slope_kb_per_upgrade",
        "max_procs", "rollback_ok", "client_lag_p99_ms", "swaps_observed"]
dirs = [d for a in sys.argv[1:] for d in sorted(glob.glob(a))]
for d in dirs:
    try:
        m, _ = analyze_run(d)
    except Exception as e:
        print(d, "ERROR", e); continue
    print(f"== {m['solution']} {m['run_id']}")
    print("   " + "  ".join(f"{k}={m[k]:.3g}" if isinstance(m.get(k), float) else f"{k}={m.get(k)}" for k in keys if k in m))
