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
| `noop` | AX warm | **136.86** (110.96–154.98) | 0.00 | **136.85** | +106.97ms (4.58×) |
| `cpu_sieve` (n=5M) | Local arm64 | 109.04 (108.78–131.32) | 76.14 | 33.35 | baseline |
| `cpu_sieve` (n=5M) | AX warm | **228.88** (212.98–235.74) | **90.60** | **137.23** | 1.19× compute, +103.88ms overhead |
| `cpu_math` (1.5M) | Local arm64 | 193.84 (186.91–198.54) | 160.45 | 31.81 | baseline |
| `cpu_math` (1.5M) | AX warm | **327.52** (308.00–394.05) | **188.19** | **137.82** | 1.17× compute, +106.01ms overhead |
| `mem_json` (50k/6.6MB) | Local arm64 | 168.08 (165.07–173.99) | 107.44 | 60.74 | baseline |
| `mem_json` (50k/6.6MB) | AX warm | **358.15** (336.88–423.77) | **176.07** | **187.73** | 1.64× compute, +126.99ms overhead |

## AX Task Lifecycle (Medians in ms, 5 Runs, `--server http://127.0.0.1:54380`)

- **Suspend & resume (`noop`)**: suspend **96.93** | resume **105.65** | ssh **129.04** | resume + exec **233.40** | full cycle **331.34**
- **Cold task (`noop`)**: apply **43.83** + resume **91.19** + ssh **176.59** = **317.43** to output (+ delete **43.08** = **360.55** total)
- **Cold task (`cpu_sieve`)**: apply **46.26** + resume **111.86** + ssh **279.52** (in-Python **110.86**) = **449.86** to output (+ delete **66.80** = **512.89** total)

## Sanity Checks

- All 127 workload runs and 4 environment checks in `results.json` exited `0` with empty `stderr`, zero retries, and identical output checksums across every environment.
- Follow-up spot-checks (4 runs each) matched the reported medians closely (e.g., AX `noop` 127.29ms, `cpu_sieve` 226.62ms, `mem_json` 331.53ms; local arm64 `noop` 27.45ms, `cpu_sieve` 105.42ms).

## Takeaways

1. **Per-call overhead**: Warm `ax ssh` adds ~104–107ms over local Python startup (~137ms total overhead vs ~30–33ms locally).
2. **Compute & memory**: Inside AX gVisor (arm64), CPU-heavy Python runs **1.17–1.19×** slower than native macOS arm64. Allocation-heavy `mem_json` runs **1.64×** slower due to gVisor syscall/memory overhead and runtime differences (Python 3.12 GCC vs 3.13 Clang).
3. **Cold start & suspend/resume**: Creating a new task from scratch (`apply` → `resume` → `ssh`) takes **~317ms** for `noop`, while resuming a suspended task and running a command takes **~233ms**.
