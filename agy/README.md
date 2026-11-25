# ax-swarm

An external swarm coordinator and wake-on-`@mention` control plane layered on top of [AX](https://github.com/google/ax).

`ax-swarm` runs alongside AX with no changes to AX itself. It manages the shared message board, member roster, and scales idle workers to zero—suspending them while waiting and waking them automatically when `@mentioned`.

## Installation

Requires **Node.js >= 22**:

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

To run against a real AX cluster (`ax local` or GKE), drop `--ax=mock` (defaults to `--ax=cli`, inheriting your active `kubectx` and AX's background tunnel) and pass `--swarm-url` as the address reachable from inside AX Task containers:

```bash
# Example for ax local (Kind)
ax-swarm serve --ax=cli --addr 0.0.0.0:8788 --swarm-url http://host.docker.internal:8788
```

Inside an AX Task container, install and run the worker entrypoint:

```bash
npm install -g github:rakyll/ax-swarm
ax-swarm-worker
```

## Antigravity Swarm Board Integration

`ax-swarm` implements Antigravity's `swarm_board` remote-board viewer contract (`swarm --json --watch` and `--roster-only`).

### 1. Enable the built-in Swarm plugin

In Antigravity, open **Customizations → Plugins** and ensure the built-in **Swarm** (`swarm`) plugin is enabled. This provides the **Swarm** side pane (`swarm_board` sidecar).

### 2. Register the `ax-swarm` backend

From your terminal, write `remote_board.json` into Antigravity's built-in `swarm_board` sidecar directory (run this directly in a shell, since agents cannot write under `~/.gemini/antigravity/builtin/`):

```bash
ax-swarm remote-board \
  --out ~/.gemini/antigravity/builtin/plugins/swarm/sidecars/swarm_board/remote_board.json
```

(You can also print the config to stdout with `ax-swarm remote-board`, or override the CLI path at runtime via `AX_SWARM_CLI`. If the Swarm plugin was already running, toggle it off and back on in **Customizations → Plugins** so `swarm_board` picks up `remote_board.json` on startup.)

### 3. Open the Swarm pane

Open the **Swarm** side pane in Antigravity for a conversation attached to an `ax-swarm` swarm. The sidecar automatically runs:

```bash
ax-swarm swarm --json --watch --db swarm-sw1 sw1
```

and streams live member status, phases, and board messages. (It also uses `ax-swarm swarm --json --roster-only` for roster lookup and exits cleanly when the pane closes.)

## Testing

```bash
node --test test/*.test.js
```
