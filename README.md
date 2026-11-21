# ax-swarm

An external swarm coordinator and wake-on-`@mention` control plane layered on top of [AX](https://github.com/google/ax).

`ax-swarm` runs alongside AX with no changes to AX itself. It manages the shared message board, member roster, and scales idle workers to zero—suspending them while waiting and waking them automatically when `@mentioned`.

## Installation

Requires **Node.js >= 22** (zero dependencies, zero build step):

```bash
npm install -g github:rakyll/ax-swarm
```

## Quickstart (local dry-run with mock AX)

```bash
# Terminal 1: start the server with the in-memory mock AX backend
ax-swarm serve --ax=mock --suspend-grace=5000

# Terminal 2: create a swarm, add workers, post messages
ax-swarm create sw1 --title "parser rewrite" --workspace golang
ax-swarm add sw1 parser --runtime claude-code --brief "Fix the 3 failing tests in parser_test.go"
ax-swarm add sw1 verifier --runtime claude-code --brief "Verify parser's changes when tagged"
ax-swarm status sw1
ax-swarm post sw1 --as parser "@verifier [done] tests pass on branch swarm/parser"
ax-swarm wait sw1 --as parser --since 3
```

To run against a real AX cluster (`ax local` or GKE), drop `--ax=mock` (defaults to `--ax=cli`, inheriting your active `kubectx` and AX's background tunnel):

```bash
ax-swarm serve --ax=cli --public-url http://<reachable-host>:8787
```

Inside an AX Task container, install and run the worker entrypoint:

```bash
npm install -g github:rakyll/ax-swarm
ax-swarm-worker
```

## Antigravity Swarm Board Integration

`ax-swarm` implements Antigravity's `swarm_board` remote-board viewer contract (`swarm --json --watch` and `--roster-only`).

### 1. Register the `ax-swarm` backend

Write `remote_board.json` into Antigravity's `swarm_board` sidecar directory:

```bash
ax-swarm remote-board \
  --out ~/.gemini/antigravity/builtin/plugins/swarm/sidecars/swarm_board/remote_board.json
```

(You can also print it to stdout with `ax-swarm remote-board` or override the binary path via `AX_SWARM_CLI`.)

### 2. Anchor a conversation to a swarm

Create `swarm.json` in the conversation's swarm directory (`~/.gemini/antigravity/brain/<conversation-id>/.system_generated/swarm/swarm.json`):

```json
{
  "backend": "ax-swarm",
  "swarm_id": "sw1",
  "db": "swarm-sw1"
}
```

### 3. Open the Swarm pane

Open the Swarm Board pane in Antigravity for that conversation. The sidecar automatically runs:

```bash
ax-swarm swarm --json --watch --db swarm-sw1 sw1
```

and streams live member status, phases, and board messages. (It also uses `ax-swarm swarm --json --roster-only` for roster lookup and exits cleanly when the pane closes.)

## Testing

```bash
node --test test/*.test.js
```
