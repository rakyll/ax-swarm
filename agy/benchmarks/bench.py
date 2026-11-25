#!/usr/bin/env python3
"""Self-contained Python benchmark suite for local host and AX comparison."""

import argparse
import json
import statistics
import subprocess
import sys
import time
from pathlib import Path

WORKLOAD_ORDER = ["noop", "cpu_sieve", "cpu_math", "mem_json"]

# Single-quote-free Python 3.8+ stdlib-only snippets so they work cleanly in:
# - local `python3 -c '<snippet>'`
# - `ax ssh <task> -- python3 -c '<snippet>'`
# - `ax apply -f -` YAML spec.commands
WORKLOAD_SNIPPETS = {
    "env": (
        "import json,os,platform,socket,sys\n"
        "info={"
        "\"python_version\":sys.version,"
        "\"executable\":sys.executable,"
        "\"platform\":platform.platform(),"
        "\"system\":platform.system(),"
        "\"machine\":platform.machine(),"
        "\"hostname\":socket.gethostname(),"
        "\"cpu_count\":os.cpu_count(),"
        "\"pid\":os.getpid(),"
        "\"cwd\":os.getcwd()"
        "}\n"
        "os_rel=\"\"\n"
        "try:\n"
        " os_rel=open(\"/etc/os-release\").read().strip().splitlines()[0]\n"
        "except Exception:\n"
        " pass\n"
        "info[\"os_release\"]=os_rel\n"
        "print(\"BENCH_RESULT:\"+json.dumps(info))"
    ),
    "noop": (
        "import json,time\n"
        "t0=time.perf_counter()\n"
        "val=1\n"
        "t1=time.perf_counter()\n"
        "print(\"BENCH_RESULT:\"+json.dumps({\"workload\":\"noop\",\"in_python_ms\":round((t1-t0)*1000.0,6),\"checksum\":val}))"
    ),
    "cpu_sieve": (
        "import json,time\n"
        "t0=time.perf_counter()\n"
        "n=5000000\n"
        "sieve=bytearray(b\"\\x01\")*(n+1)\n"
        "sieve[0:2]=b\"\\x00\\x00\"\n"
        "limit=int(n**0.5)+1\n"
        "for p in range(2,limit):\n"
        " if sieve[p]:\n"
        "  sieve[p*p:n+1:p]=b\"\\x00\"*(((n-p*p)//p)+1)\n"
        "primes=[i for i,v in enumerate(sieve) if v]\n"
        "count=len(primes)\n"
        "total=sum(primes)\n"
        "t1=time.perf_counter()\n"
        "print(\"BENCH_RESULT:\"+json.dumps({\"workload\":\"cpu_sieve\",\"n\":n,\"in_python_ms\":round((t1-t0)*1000.0,3),\"prime_count\":count,\"checksum\":total}))"
    ),
    "cpu_math": (
        "import json,math,time\n"
        "t0=time.perf_counter()\n"
        "acc=0.0\n"
        "for i in range(1,1500001):\n"
        " acc+=math.sin(i)*math.cos(i)+1.0/(i+1.0)\n"
        "t1=time.perf_counter()\n"
        "print(\"BENCH_RESULT:\"+json.dumps({\"workload\":\"cpu_math\",\"iters\":1500000,\"in_python_ms\":round((t1-t0)*1000.0,3),\"checksum\":round(acc,6)}))"
    ),
    "mem_json": (
        "import json,time\n"
        "t0=time.perf_counter()\n"
        "data=[{\"id\":i,\"group\":f\"g{i%64}\",\"name\":f\"item_{i:06d}\",\"values\":[i,i*2,(i*3)%997,round(i*0.125,3)],\"meta\":{\"active\":(i%2==0),\"tag\":f\"t{i%16}\"}} for i in range(50000)]\n"
        "raw=json.dumps(data)\n"
        "parsed=json.loads(raw)\n"
        "totals={}\n"
        "for row in parsed:\n"
        " g=row[\"group\"]\n"
        " totals[g]=totals.get(g,0)+row[\"values\"][2]\n"
        "checksum=sum(totals.values())\n"
        "t1=time.perf_counter()\n"
        "print(\"BENCH_RESULT:\"+json.dumps({\"workload\":\"mem_json\",\"records\":len(parsed),\"json_bytes\":len(raw),\"in_python_ms\":round((t1-t0)*1000.0,3),\"checksum\":checksum}))"
    ),
}


def parse_bench_result(stdout: str):
    for line in reversed(stdout.splitlines()):
        line = line.strip()
        if "BENCH_RESULT:" in line:
            return json.loads(line.split("BENCH_RESULT:", 1)[1].strip())
    raise ValueError(f"Missing BENCH_RESULT line in stdout: {stdout!r}")


def summarize(values):
    if not values:
        return None
    s = sorted(values)
    return {
        "count": len(s),
        "min_ms": round(s[0], 3),
        "median_ms": round(statistics.median(s), 3),
        "mean_ms": round(statistics.mean(s), 3),
        "max_ms": round(s[-1], 3),
        "stdev_ms": round(statistics.stdev(s), 3) if len(s) > 1 else 0.0,
    }


def run_cmd_timed(cmd, stdin_data=None, timeout=180):
    t0 = time.perf_counter()
    proc = subprocess.run(
        cmd,
        input=stdin_data,
        capture_output=True,
        text=True,
        timeout=timeout,
    )
    wall_ms = (time.perf_counter() - t0) * 1000.0
    parsed = None
    parse_err = None
    if proc.returncode == 0:
        try:
            parsed = parse_bench_result(proc.stdout)
        except Exception as e:
            parse_err = str(e)
    in_py_ms = parsed.get("in_python_ms") if isinstance(parsed, dict) else None
    overhead_ms = (wall_ms - in_py_ms) if in_py_ms is not None else None
    return {
        "returncode": proc.returncode,
        "wall_ms": round(wall_ms, 3),
        "in_python_ms": round(in_py_ms, 6) if in_py_ms is not None else None,
        "overhead_ms": round(overhead_ms, 3) if overhead_ms is not None else None,
        "parsed": parsed,
        "stderr": proc.stderr.strip(),
        "parse_error": parse_err,
    }


def run_suite(mode_name, command_builder, iterations=7):
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=["local"], default="local")
    parser.add_argument("--iterations", type=int, default=7)
    parser.add_argument("--out", default="/tmp/ax-bench/local_baseline.json")
    args = parser.parse_args()

    py_exe = sys.executable
    res = run_suite(
        "local_host",
        lambda wname, code: ([py_exe, "-c", code], None),
        iterations=args.iterations,
    )
    res["command_template"] = f"{py_exe} -c '<snippet>'"
    Path(args.out).write_text(json.dumps(res, indent=2))
    print(f"Saved {args.out}")
    print("Env:", json.dumps(res["env"]))
    for wname in WORKLOAD_ORDER:
        w = res["workloads"][wname]
        ws = w["warm_runs_summary"]
        print(
            f"{wname:10s} cold_wall={w['cold_run']['wall_ms']:7.2f}ms | "
            f"warm_wall med={ws['wall']['median_ms']:7.2f}ms (min={ws['wall']['min_ms']:.2f}, max={ws['wall']['max_ms']:.2f}) | "
            f"in_py med={ws['in_python']['median_ms']:7.3f}ms | "
            f"overhead med={ws['overhead']['median_ms']:6.2f}ms | checksum={w['checksums']}"
        )


if __name__ == "__main__":
    main()
