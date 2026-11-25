#!/usr/bin/env python3
"""Full local (arm64 native + Rosetta x86_64) vs AX Python benchmark suite.

Writes complete raw measurements, environment fingerprints, per-phase breakdowns,
and summary comparisons to /tmp/ax-bench/results.json.
"""

import json
import platform
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, "/tmp/ax-bench")
from bench import (
    WORKLOAD_ORDER,
    WORKLOAD_SNIPPETS,
    run_cmd_timed,
    summarize,
)

AX_BIN = "/Users/jbd/go/bin/ax"
AX_SERVER = "http://127.0.0.1:54380"
AX_IMAGE = (
    "localhost:5001/ax-runner:latest"
    "@sha256:f2651e18c50310cee1b2afb8896bc86acb76f0165e7fb396cca7f6ca60858d21"
)
HOST_PY = "/usr/local/bin/python3"
OUT_PATH = Path("/tmp/ax-bench/results.json")


def task_yaml(name: str) -> str:
    return (
        "apiVersion: ax.io/v1alpha1\n"
        "kind: Task\n"
        "metadata:\n"
        f"  name: {name}\n"
        "spec:\n"
        "  profile:\n"
        f"    image: {AX_IMAGE}\n"
        "  debug: true\n"
    )


def ax_cmd(args, use_direct_server=True):
    cmd = [AX_BIN]
    if use_direct_server:
        cmd.extend(["--server", AX_SERVER])
    cmd.extend(args)
    return cmd


def run_simple_timed(cmd, stdin_data=None, timeout=120):
    t0 = time.perf_counter()
    proc = subprocess.run(
        cmd,
        input=stdin_data,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    ms = (time.perf_counter() - t0) * 1000.0
    return {
        "returncode": proc.returncode,
        "wall_ms": round(ms, 3),
        "stdout": proc.stdout.strip(),
        "stderr": proc.stderr.strip(),
    }


def delete_task_quiet(name: str):
    subprocess.run(
        ax_cmd(["delete", "task", name], use_direct_server=True),
        capture_output=True,
        text=True,
        timeout=60,
    )


def run_suite_with_pause(mode_name, command_builder, iterations=7, pause_s=0.1):
    env_cmd, env_stdin = command_builder("env", WORKLOAD_SNIPPETS["env"])
    env_res = run_cmd_timed(env_cmd, stdin_data=env_stdin)
    if env_res["returncode"] != 0 or env_res["parsed"] is None:
        raise RuntimeError(f"[{mode_name}] env fingerprint failed: {env_res}")

    workloads_out = {}
    for wname in WORKLOAD_ORDER:
        code = WORKLOAD_SNIPPETS[wname]
        runs = []
        for idx in range(iterations):
            cmd, stdin_data = command_builder(wname, code)
            res = run_cmd_timed(cmd, stdin_data=stdin_data)
            res["iteration"] = idx + 1
            res["is_cold"] = (idx == 0)
            if res["returncode"] != 0 or res["parsed"] is None:
                raise RuntimeError(f"[{mode_name}] {wname} iter {idx+1} failed: {res}")
            runs.append(res)
            if pause_s > 0:
                time.sleep(pause_s)

        warm_runs = runs[1:] if len(runs) > 1 else runs
        checksums = sorted({str(r["parsed"].get("checksum")) for r in runs})
        workloads_out[wname] = {
            "iterations": iterations,
            "checksums": checksums,
            "cold_run": {
                "wall_ms": runs[0]["wall_ms"],
                "in_python_ms": runs[0]["in_python_ms"],
                "overhead_ms": runs[0]["overhead_ms"],
            },
            "all_runs_summary": {
                "wall": summarize([r["wall_ms"] for r in runs]),
                "in_python": summarize([r["in_python_ms"] for r in runs]),
                "overhead": summarize([r["overhead_ms"] for r in runs]),
            },
            "warm_runs_summary": {
                "wall": summarize([r["wall_ms"] for r in warm_runs]),
                "in_python": summarize([r["in_python_ms"] for r in warm_runs]),
                "overhead": summarize([r["overhead_ms"] for r in warm_runs]),
            },
            "runs": runs,
        }

    return {
        "mode": mode_name,
        "env_wall_ms": env_res["wall_ms"],
        "env": env_res["parsed"],
        "workloads": workloads_out,
    }


def run_ssh_with_retry(name: str, code: str, use_direct_server=True, max_retries=5):
    cmd = ax_cmd(["ssh", name, "--", "python3", "-"], use_direct_server=use_direct_server)
    t_first = time.perf_counter()
    retries = 0
    while True:
        res = run_cmd_timed(cmd, stdin_data=code)
        if res["returncode"] == 0 and res["parsed"] is not None:
            res["retries"] = retries
            res["wall_including_retries_ms"] = round((time.perf_counter() - t_first) * 1000.0, 3)
            return res
        if retries >= max_retries:
            res["retries"] = retries
            return res
        retries += 1
        time.sleep(0.15)


def benchmark_cold_lifecycle(workloads=("noop", "cpu_sieve"), iterations=5, use_direct_server=True):
    mode_name = "ax_cold_lifecycle_direct" if use_direct_server else "ax_cold_lifecycle_default"
    results = {}
    for wname in workloads:
        code = WORKLOAD_SNIPPETS[wname]
        runs = []
        for idx in range(iterations):
            tname = f"bench-cold-{wname.replace('_', '-')}-{idx+1}"
            delete_task_quiet(tname)
            try:
                t_start = time.perf_counter()
                r_apply = run_simple_timed(
                    ax_cmd(["apply", "-f", "-"], use_direct_server=use_direct_server),
                    stdin_data=task_yaml(tname),
                )
                if r_apply["returncode"] != 0:
                    raise RuntimeError(f"apply failed for {tname}: {r_apply}")

                r_resume = run_simple_timed(
                    ax_cmd(["resume", "task", tname], use_direct_server=use_direct_server)
                )
                if r_resume["returncode"] != 0:
                    raise RuntimeError(f"resume failed for {tname}: {r_resume}")

                r_ssh = run_ssh_with_retry(tname, code, use_direct_server=use_direct_server)
                if r_ssh["returncode"] != 0 or r_ssh["parsed"] is None:
                    raise RuntimeError(f"ssh failed for {tname}: {r_ssh}")
                cold_exec_total_ms = (time.perf_counter() - t_start) * 1000.0

                r_delete = run_simple_timed(
                    ax_cmd(["delete", "task", tname], use_direct_server=use_direct_server)
                )
                if r_delete["returncode"] != 0:
                    raise RuntimeError(f"delete failed for {tname}: {r_delete}")
                full_lifecycle_ms = (time.perf_counter() - t_start) * 1000.0

                in_py_ms = r_ssh["in_python_ms"]
                runs.append({
                    "iteration": idx + 1,
                    "task_name": tname,
                    "returncode": 0,
                    "apply_ms": r_apply["wall_ms"],
                    "resume_ms": r_resume["wall_ms"],
                    "ssh_wall_ms": r_ssh["wall_ms"],
                    "ssh_retries": r_ssh["retries"],
                    "in_python_ms": in_py_ms,
                    "cold_exec_total_ms": round(cold_exec_total_ms, 3),
                    "cold_overhead_ms": round(cold_exec_total_ms - in_py_ms, 3),
                    "delete_ms": r_delete["wall_ms"],
                    "full_lifecycle_ms": round(full_lifecycle_ms, 3),
                    "parsed": r_ssh["parsed"],
                })
            finally:
                delete_task_quiet(tname)
            time.sleep(0.15)

        checksums = sorted({str(r["parsed"].get("checksum")) for r in runs})
        results[wname] = {
            "iterations": iterations,
            "checksums": checksums,
            "summary": {
                "apply": summarize([r["apply_ms"] for r in runs]),
                "resume": summarize([r["resume_ms"] for r in runs]),
                "ssh_wall": summarize([r["ssh_wall_ms"] for r in runs]),
                "in_python": summarize([r["in_python_ms"] for r in runs]),
                "cold_exec_total": summarize([r["cold_exec_total_ms"] for r in runs]),
                "cold_overhead": summarize([r["cold_overhead_ms"] for r in runs]),
                "delete": summarize([r["delete_ms"] for r in runs]),
                "full_lifecycle": summarize([r["full_lifecycle_ms"] for r in runs]),
            },
            "runs": runs,
        }
    return {
        "mode": mode_name,
        "use_direct_server": use_direct_server,
        "command_flow": f"{AX_BIN} --server {AX_SERVER} apply -f - -> resume -> ssh -- python3 - -> delete",
        "workloads": results,
    }


def benchmark_suspend_resume(task_name: str, iterations=5, use_direct_server=True):
    code = WORKLOAD_SNIPPETS["noop"]
    runs = []
    for idx in range(iterations):
        r_suspend = run_simple_timed(
            ax_cmd(["suspend", "task", task_name], use_direct_server=use_direct_server)
        )
        if r_suspend["returncode"] != 0:
            raise RuntimeError(f"suspend failed on iter {idx+1}: {r_suspend}")

        t_res = time.perf_counter()
        r_resume = run_simple_timed(
            ax_cmd(["resume", "task", task_name], use_direct_server=use_direct_server)
        )
        if r_resume["returncode"] != 0:
            raise RuntimeError(f"resume failed on iter {idx+1}: {r_resume}")

        r_ssh = run_ssh_with_retry(task_name, code, use_direct_server=use_direct_server)
        if r_ssh["returncode"] != 0 or r_ssh["parsed"] is None:
            raise RuntimeError(f"ssh after resume failed on iter {idx+1}: {r_ssh}")
        resume_and_exec_ms = (time.perf_counter() - t_res) * 1000.0

        runs.append({
            "iteration": idx + 1,
            "returncode": 0,
            "suspend_ms": r_suspend["wall_ms"],
            "resume_ms": r_resume["wall_ms"],
            "ssh_wall_ms": r_ssh["wall_ms"],
            "ssh_retries": r_ssh["retries"],
            "in_python_ms": r_ssh["in_python_ms"],
            "resume_and_exec_ms": round(resume_and_exec_ms, 3),
            "full_cycle_ms": round(r_suspend["wall_ms"] + resume_and_exec_ms, 3),
            "parsed": r_ssh["parsed"],
        })
        time.sleep(0.15)

    return {
        "mode": "ax_suspend_resume_direct" if use_direct_server else "ax_suspend_resume_default",
        "workload": "noop",
        "iterations": iterations,
        "checksums": sorted({str(r["parsed"].get("checksum")) for r in runs}),
        "command_flow": f"{AX_BIN} --server {AX_SERVER} suspend task {task_name} -> resume -> ssh -- python3 -",
        "summary": {
            "suspend": summarize([r["suspend_ms"] for r in runs]),
            "resume": summarize([r["resume_ms"] for r in runs]),
            "ssh_wall": summarize([r["ssh_wall_ms"] for r in runs]),
            "in_python": summarize([r["in_python_ms"] for r in runs]),
            "resume_and_exec": summarize([r["resume_and_exec_ms"] for r in runs]),
            "full_cycle": summarize([r["full_cycle_ms"] for r in runs]),
        },
        "runs": runs,
    }


def compute_comparisons(base_res, ax_res):
    comp = {}
    for wname in WORKLOAD_ORDER:
        lw = base_res["workloads"][wname]["warm_runs_summary"]
        aw = ax_res["workloads"][wname]["warm_runs_summary"]
        l_wall = lw["wall"]["median_ms"]
        a_wall = aw["wall"]["median_ms"]
        l_py = lw["in_python"]["median_ms"]
        a_py = aw["in_python"]["median_ms"]
        l_ov = lw["overhead"]["median_ms"]
        a_ov = aw["overhead"]["median_ms"]
        comp[wname] = {
            "baseline_warm_wall_med_ms": l_wall,
            "ax_warm_wall_med_ms": a_wall,
            "wall_diff_ms": round(a_wall - l_wall, 3),
            "wall_ratio": round(a_wall / l_wall, 3) if l_wall > 0 else None,
            "baseline_in_python_med_ms": l_py,
            "ax_in_python_med_ms": a_py,
            "in_python_diff_ms": round(a_py - l_py, 3),
            "in_python_ratio": round(a_py / l_py, 3) if l_py > 0.01 else None,
            "baseline_overhead_med_ms": l_ov,
            "ax_overhead_med_ms": a_ov,
            "extra_overhead_ms": round(a_ov - l_ov, 3),
            "checksum_match": (
                base_res["workloads"][wname]["checksums"]
                == ax_res["workloads"][wname]["checksums"]
            ),
        }
    return comp


def run_warm_ax_mode(mode_name: str, task_name: str, use_direct_server: bool, iterations=7):
    delete_task_quiet(task_name)
    try:
        setup_apply = run_simple_timed(
            ax_cmd(["apply", "-f", "-"], use_direct_server=use_direct_server),
            stdin_data=task_yaml(task_name),
        )
        if setup_apply["returncode"] != 0:
            raise RuntimeError(f"Failed to create {task_name}: {setup_apply}")
        setup_resume = run_simple_timed(
            ax_cmd(["resume", "task", task_name], use_direct_server=use_direct_server)
        )
        if setup_resume["returncode"] != 0:
            raise RuntimeError(f"Failed to resume {task_name}: {setup_resume}")

        res = run_suite_with_pause(
            mode_name,
            lambda wname, code: (
                ax_cmd(["ssh", task_name, "--", "python3", "-"], use_direct_server=use_direct_server),
                code,
            ),
            iterations=iterations,
            pause_s=0.15,
        )
        res["command_template"] = (
            f"{AX_BIN} {'--server ' + AX_SERVER + ' ' if use_direct_server else ''}ssh {task_name} -- python3 - < <snippet>"
        )
        res["task_setup"] = {
            "apply_ms": setup_apply["wall_ms"],
            "resume_ms": setup_resume["wall_ms"],
        }
        return res
    finally:
        delete_task_quiet(task_name)


def main():
    started_at = datetime.now(timezone.utc).isoformat()

    print("1/6 Running local host native arm64 Python baseline (7 iters)...", flush=True)
    local_arm64 = run_suite_with_pause(
        "local_host_arm64",
        lambda wname, code: (["arch", "-arm64", HOST_PY, "-"], code),
        iterations=7,
        pause_s=0.05,
    )
    local_arm64["command_template"] = f"arch -arm64 {HOST_PY} - < <snippet>"

    print("2/6 Running local host default-shell Rosetta x86_64 baseline (7 iters)...", flush=True)
    local_rosetta = run_suite_with_pause(
        "local_host_rosetta_x86_64",
        lambda wname, code: ([HOST_PY, "-"], code),
        iterations=7,
        pause_s=0.05,
    )
    local_rosetta["command_template"] = f"{HOST_PY} - < <snippet>"

    print("3/6 Running AX warm persistent suite (--server direct, fresh task, 7 iters)...", flush=True)
    ax_warm_direct = run_warm_ax_mode(
        "ax_ssh_warm_direct", "bench-py-direct", use_direct_server=True, iterations=7
    )

    print("4/6 Running AX warm persistent suite (default CLI, fresh task, 7 iters)...", flush=True)
    ax_warm_default = run_warm_ax_mode(
        "ax_ssh_warm_default", "bench-py-default", use_direct_server=False, iterations=7
    )

    print("5/6 Running AX suspend/resume checkpoint benchmark (5 iters)...", flush=True)
    sr_task = "bench-py-sr"
    delete_task_quiet(sr_task)
    try:
        r_a = run_simple_timed(ax_cmd(["apply", "-f", "-"], use_direct_server=True), stdin_data=task_yaml(sr_task))
        r_r = run_simple_timed(ax_cmd(["resume", "task", sr_task], use_direct_server=True))
        if r_a["returncode"] != 0 or r_r["returncode"] != 0:
            raise RuntimeError(f"sr task setup failed: {r_a} / {r_r}")
        ax_suspend_resume = benchmark_suspend_resume(sr_task, iterations=5, use_direct_server=True)
    finally:
        delete_task_quiet(sr_task)

    print("6/6 Running AX cold task lifecycle benchmark (5 iters each: noop, cpu_sieve)...", flush=True)
    ax_cold_direct = benchmark_cold_lifecycle(
        workloads=("noop", "cpu_sieve"), iterations=5, use_direct_server=True
    )

    remaining = run_simple_timed(ax_cmd(["get", "tasks"], use_direct_server=True))
    ax_version = run_simple_timed([AX_BIN, "version"])
    host_cpu = run_simple_timed(["sysctl", "-n", "machdep.cpu.brand_string"])

    full_results = {
        "metadata": {
            "timestamp_utc": started_at,
            "completed_utc": datetime.now(timezone.utc).isoformat(),
            "ax_binary": AX_BIN,
            "ax_version": ax_version["stdout"],
            "ax_server_tunnel": AX_SERVER,
            "ax_image": AX_IMAGE,
            "host_cpu_brand": host_cpu["stdout"],
            "host_uname": " ".join(platform.uname()),
            "workload_order": WORKLOAD_ORDER,
            "workload_snippets": WORKLOAD_SNIPPETS,
            "remaining_tasks_after_cleanup": remaining["stdout"],
        },
        "modes": {
            "local_host_arm64": local_arm64,
            "local_host_rosetta_x86_64": local_rosetta,
            "ax_ssh_warm_direct": ax_warm_direct,
            "ax_ssh_warm_default": ax_warm_default,
            "ax_suspend_resume_direct": ax_suspend_resume,
            "ax_cold_lifecycle_direct": ax_cold_direct,
        },
        "comparisons_vs_local_arm64": {
            "ax_ssh_warm_direct": compute_comparisons(local_arm64, ax_warm_direct),
            "ax_ssh_warm_default": compute_comparisons(local_arm64, ax_warm_default),
        },
        "comparisons_vs_local_rosetta_x86_64": {
            "ax_ssh_warm_direct": compute_comparisons(local_rosetta, ax_warm_direct),
            "ax_ssh_warm_default": compute_comparisons(local_rosetta, ax_warm_default),
        },
    }

    OUT_PATH.write_text(json.dumps(full_results, indent=2))
    print(f"\nSaved full results to {OUT_PATH}\n")

    print("=== Environment Fingerprints ===")
    print("Local arm64:  ", json.dumps(local_arm64["env"]))
    print("Local x86_64: ", json.dumps(local_rosetta["env"]))
    print("AX gVisor:    ", json.dumps(ax_warm_direct["env"]))

    print("\n=== Warm Benchmarks (median ms, 6 warm runs after 1 cold) ===")
    for mode_key in ("local_host_arm64", "local_host_rosetta_x86_64", "ax_ssh_warm_direct", "ax_ssh_warm_default"):
        print(f"\n[{mode_key}]")
        m = full_results["modes"][mode_key]
        for wname in WORKLOAD_ORDER:
            w = m["workloads"][wname]
            ws = w["warm_runs_summary"]
            print(
                f"  {wname:10s} | cold={w['cold_run']['wall_ms']:7.2f}ms | "
                f"wall med={ws['wall']['median_ms']:7.2f}ms (min={ws['wall']['min_ms']:6.2f}, mean={ws['wall']['mean_ms']:6.2f}, max={ws['wall']['max_ms']:6.2f}) | "
                f"in_py med={ws['in_python']['median_ms']:7.3f}ms | "
                f"overhead med={ws['overhead']['median_ms']:7.2f}ms | chk={w['checksums']}"
            )

    print("\n=== AX Suspend/Resume (noop, 5 iters, --server direct) ===")
    sr = ax_suspend_resume["summary"]
    print(
        f"  suspend med={sr['suspend']['median_ms']:.2f}ms | "
        f"resume med={sr['resume']['median_ms']:.2f}ms | "
        f"ssh med={sr['ssh_wall']['median_ms']:.2f}ms | "
        f"resume+exec med={sr['resume_and_exec']['median_ms']:.2f}ms | "
        f"full_cycle med={sr['full_cycle']['median_ms']:.2f}ms"
    )

    print("\n=== AX Cold Task Lifecycle (5 iters each, --server direct) ===")
    for wname, cw in ax_cold_direct["workloads"].items():
        cs = cw["summary"]
        print(
            f"  {wname:10s} | apply={cs['apply']['median_ms']:.2f}ms | "
            f"resume={cs['resume']['median_ms']:.2f}ms | "
            f"ssh={cs['ssh_wall']['median_ms']:.2f}ms (in_py={cs['in_python']['median_ms']:.2f}ms) | "
            f"cold_exec_total={cs['cold_exec_total']['median_ms']:.2f}ms | "
            f"delete={cs['delete']['median_ms']:.2f}ms | "
            f"full={cs['full_lifecycle']['median_ms']:.2f}ms | chk={cw['checksums']}"
        )

    print("\n=== Comparisons vs local_host_arm64 ===")
    for mk, comps in full_results["comparisons_vs_local_arm64"].items():
        print(f"  [{mk}]")
        for wname, c in comps.items():
            print(
                f"    {wname:10s}: wall +{c['wall_diff_ms']:6.2f}ms ({c['wall_ratio']}x) | "
                f"in_py diff={c['in_python_diff_ms']:+7.2f}ms ({c['in_python_ratio']}x) | "
                f"extra_overhead=+{c['extra_overhead_ms']:6.2f}ms | chk_ok={c['checksum_match']}"
            )


if __name__ == "__main__":
    main()
