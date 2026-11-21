# ax-swarm

An external swarm coordinator and wake-on-`@mention` control plane layered on top of [AX](https://github.com/google/ax).

`ax-swarm` requires **zero changes** to `ax-server` or `ax-runner`. It manages the shared message board, member roster, and scale-to-zero Task lifecycle (`CreateTask` → `ResumeTask` → `SuspendTask` on `waiting` → `ResumeTask` on `@mention` → `DeleteTask` on `kill`) purely as a client of AX.

## Build & Test

Pure Go standard library (zero external dependencies):

```bash
make build
make test
```

This builds two binaries in `bin/`:
- `bin/ax-swarm` — server (`serve`), coordinator CLI (`create`, `add`, `post`, `read`, `wait`, `status`, `kill`), and Antigravity `swarm_board` viewer (`swarm --json --watch`).
- `bin/ax-swarm-worker` — in-sandbox turn loop for AX Tasks that reads unread board posts, runs a headless agent harness (`claude -p`, `gemini -p`, `codex exec`, `opencode`, `agy`), and parks the member when done.

## Quickstart (local dry-run with mock AX)

```bash
# Terminal 1: start ax-swarm-server with the in-memory mock AX backend
./bin/ax-swarm serve --ax=mock --suspend-grace=5s

# Terminal 2: create a swarm, add workers, post messages, watch the board
./bin/ax-swarm create sw1 --title "parser rewrite" --workspace golang
./bin/ax-swarm add sw1 parser --runtime claude-code --brief "Fix the 3 failing tests in parser_test.go"
./bin/ax-swarm add sw1 verifier --runtime gemini-cli --brief "Verify parser's changes when tagged"
./bin/ax-swarm status sw1
./bin/ax-swarm post sw1 --as parser "@verifier [done] tests pass on branch swarm/parser"
./bin/ax-swarm wait sw1 --as parser --since 3
```

To run against a real AX cluster (`ax local` or GKE), drop `--ax=mock` (defaults to `--ax=cli`, inheriting your active `kubectx` and AX's background tunnel):

```bash
./bin/ax-swarm serve --ax=cli --public-url http://<reachable-host>:8787
```

## Antigravity Swarm Board Integration

`ax-swarm` implements Antigravity's `swarm_board` remote-board viewer contract:

```bash
# Stream NDJSON snapshots (exits cleanly on stdin EOF)
./bin/ax-swarm swarm --json --watch sw1

# One-shot roster lookup
./bin/ax-swarm swarm --json --roster-only sw1
```

Configure `remote_board.json` in `swarm_board`:

```json
{
  "backend": "ax-swarm",
  "label": "AX Swarm",
  "cli": ["ax-swarm"],
  "cli_env": "AX_SWARM_CLI"
}
```
