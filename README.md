# ax-swarm

An external swarm coordinator and wake-on-`@mention` control plane layered on top of [AX](https://github.com/google/ax).

`ax-swarm` runs alongside AX with no changes to AX itself. It manages the shared message board, member roster, and scales idle workers to zero—suspending them while waiting and waking them automatically when `@mentioned`.

## Installation

Requires **Node.js >= 22** (zero dependencies, zero build step):

```bash
# Run directly with npx
npx github:rakyll/ax-swarm --help

# Or install globally
npm install -g github:rakyll/ax-swarm
```

## Quickstart (local dry-run with mock AX)

```bash
# Terminal 1: start the server with the in-memory mock AX backend
npx github:rakyll/ax-swarm serve --ax=mock --suspend-grace=5000

# Terminal 2: create a swarm, add workers, post messages
npx github:rakyll/ax-swarm create sw1 --title "parser rewrite" --workspace golang
npx github:rakyll/ax-swarm add sw1 parser --runtime claude-code --brief "Fix the 3 failing tests in parser_test.go"
npx github:rakyll/ax-swarm add sw1 verifier --runtime claude-code --brief "Verify parser's changes when tagged"
npx github:rakyll/ax-swarm status sw1
npx github:rakyll/ax-swarm post sw1 --as parser "@verifier [done] tests pass on branch swarm/parser"
npx github:rakyll/ax-swarm wait sw1 --as parser --since 3
```

To run against a real AX cluster (`ax local` or GKE), drop `--ax=mock` (defaults to `--ax=cli`, inheriting your active `kubectx` and AX's background tunnel):

```bash
npx github:rakyll/ax-swarm serve --ax=cli --public-url http://<reachable-host>:8787
```

Inside an AX Task container, run the worker entrypoint with:

```bash
npx -y -p github:rakyll/ax-swarm ax-swarm-worker
```

## Antigravity Swarm Board Integration

`ax-swarm` implements Antigravity's `swarm_board` remote-board viewer contract:

```bash
# Stream NDJSON snapshots (exits cleanly on stdin EOF)
npx github:rakyll/ax-swarm swarm --json --watch sw1

# One-shot roster lookup
npx github:rakyll/ax-swarm swarm --json --roster-only sw1
```

Generate `remote_board.json` for `swarm_board`:

```bash
# Print to stdout
npx github:rakyll/ax-swarm remote-board

# Or write directly to a file
npx github:rakyll/ax-swarm remote-board --out /path/to/swarm_board/remote_board.json
```

## Testing

```bash
node --test test/*.test.js
```
