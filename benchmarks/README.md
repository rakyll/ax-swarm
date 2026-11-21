# AX Python Execution Benchmark

Benchmarks comparing Python execution in a local AX cluster (`ax-local`, Kind arm64 + gVisor) against running directly on macOS.

## Environments

- **AX (`ax-local`, Kind arm64 + gVisor)**
  - CLI: `/Users/jbd/go/bin/ax` (`dev+0b44e971e802-dirty`, API `v1alpha1`), tunnel `http://127.0.0.1:54380`
  - Image: `localhost:5001/ax-runner:latest@sha256:f2651e18c50310cee1b2afb8896bc86acb76f0165e7fb396cca7f6ca60858d21`
  - Runtime: Python **3.12.16** (GCC 14.2.0), `Linux 4.19.0-gvisor` aarch64 (Debian 13), 2 CPUs
- **Local Native arm64**
  - Command: `arch -arm64 /usr/local/bin/python3 -`
  - Runtime: Python **3.13.7** (Clang 16.0.0), macOS 26.6.2 arm64, Apple M4 (10 CPUs)
- **Local Rosetta x86_64**
  - Command: `/usr/local/bin/python3 -` (default host shell runs under Rosetta 2)

## How It Was Measured

- Each workload is a stdlib-only Python snippet (see `workloads.py` and `bench.py`) piped into `python3 -`.
- Each run records total wall time (`wall_ms`) and in-Python compute time (`in_python_ms` via `time.perf_counter()`). Startup and transport overhead is `wall_ms - in_python_ms`.
- Warm runs executed 7 times per workload; the table below reports medians of the 6 warm runs (excluding the first cold run).
- Task lifecycle tests (cold start and suspend/resume) ran 5 times per workload.
- Raw data and scripts are in `results.json`, `run_ax_bench.py`, `bench.py`, and `workloads.py`.

## Warm Execution (Medians in ms, 6 Runs)

| Workload | Mode | Wall (Min–Max) | In-Python | Overhead | vs Local arm64 |
|---|---|---:|---:|---:|---|
| `noop` | Local arm64 | 29.89 (29.23–32.13) | 0.00 | 29.89 | baseline |
| `noop` | Local Rosetta x86_64 | 44.68 (43.49–48.18) | 0.00 | 44.68 | +14.80ms (1.50×) |
| `noop` | AX warm (`--server`) | **136.86** (110.96–154.98) | 0.00 | **136.85** | +106.97ms (4.58×) |
| `noop` | AX warm (default CLI) | **141.96** (111.90–177.25) | 0.00 | **141.96** | +112.08ms (4.75×) |
| `cpu_sieve` (n=5M) | Local arm64 | 109.04 (108.78–131.32) | 76.14 | 33.35 | baseline |
| `cpu_sieve` (n=5M) | Local Rosetta x86_64 | 189.09 (185.43–198.50) | 139.26 | 50.77 | 1.83× compute |
| `cpu_sieve` (n=5M) | AX warm (`--server`) | **228.88** (212.98–235.74) | **90.60** | **137.23** | 1.19× compute, +103.88ms overhead |
| `cpu_sieve` (n=5M) | AX warm (default CLI) | **235.46** (221.20–251.43) | **93.54** | **141.92** | 1.23× compute, +108.57ms overhead |
| `cpu_math` (1.5M) | Local arm64 | 193.84 (186.91–198.54) | 160.45 | 31.81 | baseline |
| `cpu_math` (1.5M) | Local Rosetta x86_64 | 345.52 (335.30–351.14) | 298.05 | 45.80 | 1.86× compute |
| `cpu_math` (1.5M) | AX warm (`--server`) | **327.52** (308.00–394.05) | **188.19** | **137.82** | 1.17× compute, +106.01ms overhead |
| `cpu_math` (1.5M) | AX warm (default CLI) | **333.48** (307.36–370.12) | **189.81** | **144.86** | 1.18× compute, +113.05ms overhead |
| `mem_json` (50k/6.6MB) | Local arm64 | 168.08 (165.07–173.99) | 107.44 | 60.74 | baseline |
| `mem_json` (50k/6.6MB) | Local Rosetta x86_64 | 263.64 (256.34–267.70) | 174.36 | 86.63 | 1.62× compute |
| `mem_json` (50k/6.6MB) | AX warm (`--server`) | **358.15** (336.88–423.77) | **176.07** | **187.73** | 1.64× compute, +126.99ms overhead |
| `mem_json` (50k/6.6MB) | AX warm (default CLI) | **371.49** (318.15–590.94) | **179.13** | **188.37** | 1.67× compute, +127.63ms overhead |

## AX Task Lifecycle (Medians in ms, 5 Runs, `--server http://127.0.0.1:54380`)

- **Suspend & resume (`noop`)**: suspend **96.93** | resume **105.65** | ssh **129.04** | resume + exec **233.40** | full cycle **331.34**
- **Cold task (`noop`)**: apply **43.83** + resume **91.19** + ssh **176.59** = **317.43** to output (+ delete **43.08** = **360.55** total)
- **Cold task (`cpu_sieve`)**: apply **46.26** + resume **111.86** + ssh **279.52** (in-Python **110.86**) = **449.86** to output (+ delete **66.80** = **512.89** total)

## Sanity Checks

- All 127 workload runs and 4 environment checks in `results.json` exited `0` with empty `stderr`, zero retries, and identical output checksums across every environment.
- Follow-up spot-checks (4 runs each) matched the reported medians closely (e.g., AX `noop` 127.29ms, `cpu_sieve` 226.62ms, `mem_json` 331.53ms; local arm64 `noop` 27.45ms, `cpu_sieve` 105.42ms).

## Takeaways

1. **Per-call overhead**: Warm `ax ssh` adds ~104–113ms over local Python startup (~137–142ms total overhead vs ~30–33ms locally). Passing `--server` saves ~5ms when the tunnel is already warm.
2. **Compute & memory**: Inside AX gVisor (arm64), CPU-heavy Python runs **1.17–1.23×** slower than native macOS arm64 (and ~1.5× faster than Rosetta x86_64). Allocation-heavy `mem_json` runs **1.64–1.67×** slower due to gVisor syscall/memory overhead and runtime differences (Python 3.12 GCC vs 3.13 Clang).
3. **Cold start & suspend/resume**: Creating a new task from scratch (`apply` → `resume` → `ssh`) takes **~317ms** for `noop`, while resuming a suspended task and running a command takes **~233ms**.
