# ax-swarm

Run an [AX](https://github.com/google/ax) workspace swarm driven by Antigravity CLI's native `/swarm` capability (`agy --output-format stream-json -p "/swarm ..."`).

`ax-swarm` provisions **one shared AX `Gateway` and one shared AX `Workspace` per swarm**, runs `agy /swarm` in the background while streaming only its outputs, and cleans up all `<swarm-name>-*` tasks plus the Workspace and Gateway when done.

![ax-swarm demo](assets/demo.gif)

## How It Works

1. **Setup**: Creates one AX `Gateway` and one AX `Workspace` for the swarm, clones any `--repo` URLs, runs any `--setup-cmd` commands, and waits until the Workspace is ready.
2. **Run**: Starts `agy /swarm` in the background (`--output-format stream-json -p`) and streams only outputs from `agy`. The swarm splits up the work and uses `ax` directly to create, resume, run commands in (`ax ssh`), and delete parallel Tasks—all sharing that one Workspace and Gateway.
3. **Cleanup**: When `agy` finishes (or you press `Ctrl+C`), stops any running background `agy` process and deletes any remaining `<swarm-name>-*` Tasks, the Workspace, and the Gateway unless `--keep` is set.

## Prerequisites

- **Go 1.26+**
- **`ax` CLI** configured against a running AX cluster (e.g. `ax local start` or GKE) and a digest-pinned runner image (`$AX_TASK_IMAGE` or `$AX_ROOT/bin/ax-runner.digest`)
- **`agy` CLI** installed and authenticated (`agy` on `PATH` or via `AGY_BIN`). Install it with `curl -fsSL https://antigravity.google/cli/install.sh | bash` ([docs](https://antigravity.google/docs/getting-started)).

## Install

```bash
go install github.com/rakyll/ax-swarm@latest
```

## Usage

### Quickstart

```bash
# Launch with a repository and goal
ax-swarm --repo https://github.com/chalk/chalk \
  "Why would I use this library, and what are its weak spots?"

# Omit the goal to enter it interactively in the Bubble Tea prompt
ax-swarm --repo https://github.com/chalk/chalk
```

### Examples

#### 1. Pre-warmed Workspace snapshot for parallel security & test audits

Install dependencies once during Workspace setup (`--setup-cmd`) so every AX Task forked from the single Workspace snapshot starts with dependencies already installed—no repeated `npm ci` or `pip install` per task:

```bash
ax-swarm \
  --repo https://github.com/expressjs/express \
  --setup-cmd "cd express && npm ci --ignore-scripts" \
  --parallel 6 \
  --model gemini-4-neon-high \
  --effort high \
  "Run a parallel audit of Express: (1) trace request routing and middleware error propagation, (2) audit header/path parsing for edge-case security issues, (3) run the test suite and identify flaky or slow specs, and (4) synthesize concrete fixes with file/line references."
```

#### 2. Multi-repository comparative architecture & benchmark study

Clone multiple repositories into the same shared AX Workspace (`--repo` is repeatable) so parallel swarm agents can compare implementations, run cross-library benchmarks, and validate migration paths inside isolated Tasks:

```bash
ax-swarm \
  --repo https://github.com/chalk/chalk \
  --repo https://github.com/alexeyraspopov/picocolors \
  --setup-cmd "cd chalk && npm ci && cd ../picocolors && npm ci" \
  --parallel 4 \
  "Compare chalk and picocolors across bundle footprint, ANSI/color-level detection edge cases, TypeScript types, and microbenchmark throughput under Node in /workspace. Produce a migration matrix with measured numbers."
```

#### 3. Competing-hypothesis refactoring with preserved sandboxes (`--keep`)

Because each AX Task receives its own isolated copy of `/workspace` from the single Workspace snapshot, swarm agents can prototype and benchmark competing approaches in parallel without stepping on each other's edits. Use `--name` and `--keep` to inspect the winning Task afterward via `ax ssh`:

```bash
ax-swarm \
  --name parser-opt \
  --repo https://github.com/example/json-parser \
  --setup-cmd "cd json-parser && go test ./..." \
  --parallel 4 \
  --keep \
  "Profile the parser benchmarks in /workspace/json-parser, then test 3 independent optimization strategies in separate AX Tasks (zero-alloc scanner, table-driven whitespace skip, pooled AST nodes). Compare ns/op and allocs/op across all tasks and recommend the best patch."

# Inspect the winning task's diff after exiting agy
ax -a default get tasks | grep '^parser-opt-'
ax -a default ssh parser-opt-<winning-step> -- sh -lc 'cd /workspace/json-parser && git diff'
```

#### 4. Remote cluster, dedicated atespace, and custom Gateway egress

Target a non-default kubeconfig context and atespace, allow extra HTTPS registries through the swarm's single Gateway (`--allow-host`), and raise `--task-timeout` for heavier Workspace builds:

```bash
ax-swarm \
  --context gke-ax-prod \
  --atespace perf-lab \
  --name rust-bench \
  --repo https://github.com/BurntSushi/ripgrep \
  --allow-host crates.io \
  --allow-host static.crates.io \
  --allow-host index.crates.io \
  --setup-cmd "cd ripgrep && cargo fetch" \
  --task-timeout 45m \
  --parallel 8 \
  "Build release binaries and investigate regex engine dispatch performance across literal, Unicode, and multiline search workloads in /workspace/ripgrep."
```

#### 5. Frictionless `ax` CLI execution and prompt dry-run

When running a high-parallelism swarm where subagents execute many `ax apply` / `ax resume` / `ax ssh` commands, you can inspect the exact `/swarm` prompt with `--dry-run` first, then launch with `--mode auto` (or `--dangerously-skip-permissions` in trusted environments):

```bash
# Inspect the generated /swarm prompt and agy command without creating AX resources
ax-swarm --dry-run \
  --name ci-triage \
  --repo https://github.com/example/service \
  --parallel 8 \
  "Reproduce the failing integration tests, bisect the regression commit, and verify a minimal fix."

# Launch with auto mode and higher parallelism
ax-swarm \
  --name ci-triage \
  --repo https://github.com/example/service \
  --parallel 8 \
  --mode auto \
  --dangerously-skip-permissions \
  "Reproduce the failing integration tests, bisect the regression commit, and verify a minimal fix."
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--repo` | | Git repository URL cloned into `/workspace` (repeatable) |
| `--setup-cmd` | | Shell command run during AX Workspace setup (repeatable) |
| `--allow-host` | | Extra HTTPS hostname or `*.domain` allowed through the AX Gateway (repeatable) |
| `--image` | `$AX_TASK_IMAGE` / `$AX_ROOT/bin/ax-runner.digest` | Digest-pinned runner image (`...@sha256:<64 hex>`) |
| `--atespace` | `default` | AX atespace name |
| `--context` | active context | Kubeconfig context passed to `ax --context` |
| `--server` | `$AX_SERVER` | AX API server address passed to `ax --server` |
| `--name` | `swarm-<random>` | Swarm name prefix for all AX resources (max 38 chars) |
| `--parallel` | `8` | Target max concurrent AX Tasks hint for the swarm |
| `--task-timeout` | `30m` | Workspace setup timeout as Go duration or integer seconds |
| `--keep` | `false` | Keep AX Tasks, Workspace, and Gateway on exit |
| `--model` | | Model passed to `agy --model` |
| `--effort` | | Reasoning effort passed to `agy --effort` (`low\|medium\|high\|xhigh\|max`) |
| `--mode` | | Agent mode passed to `agy --mode` (`accept-edits\|auto\|plan`) |
| `--project` | | Project ID or name passed to `agy --project` |
| `--dangerously-skip-permissions` | `false` | Forward `--dangerously-skip-permissions` to `agy` |
| `--allow` | | Extra `agy` permission rule for this run only, e.g. `mcp(server/*)` (repeatable). The swarm's `ax` binary is always allowed. Never written to your `settings.json` |
| `--auto-resume` | `true` | Resume newly created swarm tasks that are waiting in `Suspended` |
| `--ax-bin` | `$AX_BIN` / `$AX_ROOT/bin/ax` / `ax` | Path to `ax` CLI binary |
| `--agy-bin` | `$AGY_BIN` / `agy` | Path to `agy` CLI binary |
| `--color` | `auto` | Color output: `auto`, `always`, `never` (respects `NO_COLOR`) |
| `--dry-run` | `false` | Print the `agy` command and `/swarm` prompt without creating AX resources or launching `agy` |

### Permissions

`agy` runs in the background, so it can't stop to ask you for approval: any tool call that would normally ask (for example an MCP tool) is denied. `ax-swarm` passes per-run permissions to `agy` with `--settings` (never saved to your `settings.json`). The swarm's `ax` binary is always allowed; allow anything else with `--allow`:

```bash
ax-swarm --allow 'mcp(github/*)' --allow 'read_url(docs.python.org)' "..."
```

Rules use `agy`'s syntax: `command(prefix)`, `mcp(server/tool)`, `mcp(server/*)`, `read_url(domain)`, and so on.

### Environment Variables

- `AX_ROOT`: AX checkout used to locate default `bin/ax` and `bin/ax-runner.digest` (default: `~/ax`)
- `AX_BIN`: Override path to the `ax` binary
- `AX_TASK_IMAGE`: Default digest-pinned runner image
- `AX_SERVER`: Default AX API server address
- `AGY_BIN`: Override path to the `agy` binary
- `NO_COLOR`: Disable color output when `--color=auto`

## Development & Testing

```bash
go test -v -race ./...
go vet ./...
```

## Repository Layout

- Root (`*.go`, `go.mod`, `Makefile`): Go `ax-swarm` CLI with Bubble Tea UI and `agy /swarm` integration.
