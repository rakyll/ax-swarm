import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { mkdtempSync, rmSync } from "node:fs";
import type { IncomingMessage, ServerResponse } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable } from "node:stream";
import { test } from "node:test";
import { MockAXClient } from "../src/axclient.ts";
import { DesiredState, Phase } from "../src/model.ts";
import type { WatchSnapshot } from "../src/model.ts";
import { Reconciler } from "../src/reconciler.ts";
import { createSwarmHandler } from "../src/server.ts";
import { FileStore } from "../src/store.ts";

function makeTempDir(): string {
  return mkdtempSync(join(tmpdir(), "ax-swarm-test-"));
}

test("FileStore: wake-on-@mention, park race protection, reload, and watch", () => {
  const dir = makeTempDir();
  try {
    const st = new FileStore(dir);
    const sw = st.createSwarm({ swarm_id: "sw1", title: "parser rewrite", workspace_ref: "golang" });
    assert.equal(sw.atespace, "swarm-sw1");

    const { member, token, brief_message } = st.addMember({
      swarm_id: "sw1",
      name: "parser-1",
      brief: "Fix parser tests",
      channels: ["parser"],
    });
    assert.ok(token.length > 0);
    assert.equal(brief_message?.seq, 1);
    assert.equal(member.last_read_seq, 0);

    const read1 = st.readBoard("sw1", "parser-1", 0);
    assert.equal(read1.head, 1);
    assert.equal(read1.messages.length, 1);

    const park1 = st.parkMember({ swarm_id: "sw1", name: "parser-1", since: 1 });
    assert.equal(park1.park_blocked, false);
    assert.equal(park1.member.desired_state, DesiredState.Waiting);

    const post2 = st.postMessage({
      swarm_id: "sw1",
      agent: "verifier",
      text: "@parser please check edge case",
    });
    assert.equal(post2.message.seq, 2);
    assert.deepEqual(post2.woken, ["parser-1"]);

    // Parking with stale since=1 is blocked and returns unread #2.
    const park2 = st.parkMember({ swarm_id: "sw1", name: "parser-1", since: 1 });
    assert.equal(park2.park_blocked, true);
    assert.equal(park2.unread?.length, 1);
    assert.equal(park2.unread?.[0].seq, 2);

    // Watch emits full snapshot then delta.
    const snaps: WatchSnapshot[] = [];
    const unsub = st.watch("sw1", (s) => snaps.push(s));
    st.postMessage({ swarm_id: "sw1", agent: "coordinator", text: "@all status check" });
    unsub();
    assert.equal(snaps.length, 2);
    assert.equal(snaps[0].since, 0);
    assert.equal(snaps[1].head, 3);
    assert.equal(snaps[1].messages.length, 1);

    // Reload from disk preserves tokens and messages.
    const st2 = new FileStore(dir);
    const auth = st2.authenticateToken(token);
    assert.deepEqual(auth, { swarm_id: "sw1", member_name: "parser-1" });
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("Reconciler: scale-to-zero AX Task lifecycle", async () => {
  const dir = makeTempDir();
  try {
    const st = new FileStore(dir);
    const mockAX = new MockAXClient();
    const rec = new Reconciler(st, mockAX, {
      suspendGraceMs: 0,
      swarmApiUrl: "http://127.0.0.1:8787",
      defaultImage: "ghcr.io/example/worker:latest",
    });

    st.createSwarm({ swarm_id: "sw1", workspace_ref: "golang" });
    st.addMember({ swarm_id: "sw1", name: "parser", brief: "Fix bugs", runtime: "claude-code" });

    // 1. Add -> Create + Resume.
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), "Working");

    // 2. Park -> Suspend.
    st.parkMember({ swarm_id: "sw1", name: "parser", since: 1 });
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), "Suspended");

    // 3. @mention -> Resume.
    st.postMessage({ swarm_id: "sw1", agent: "verifier", text: "@parser fix lint" });
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), "Working");

    // 4. Kill -> Delete.
    st.killMember("sw1", "parser");
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), undefined);
    assert.equal(st.getSwarm("sw1").members["parser"].phase, Phase.Killed);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

async function invokeHandler(
  handler: (req: IncomingMessage, res: ServerResponse) => Promise<void>,
  method: string,
  url: string,
  body?: unknown,
  headers: Record<string, string> = {},
): Promise<{ status: number; json: unknown }> {
  const payload = body !== undefined ? JSON.stringify(body) : "";
  const req = Readable.from(payload ? [Buffer.from(payload)] : []) as unknown as IncomingMessage;
  req.method = method;
  req.url = url;
  req.headers = headers;

  let status = 200;
  let out = "";
  const emitter = new EventEmitter();
  const res = Object.assign(emitter, {
    writeHead(code: number) {
      status = code;
      return this;
    },
    write(chunk: string | Buffer) {
      out += chunk.toString();
      return true;
    },
    end(chunk?: string | Buffer) {
      if (chunk) out += chunk.toString();
      emitter.emit("finish");
      return this;
    },
  }) as unknown as ServerResponse;

  await handler(req, res);
  return { status, json: out ? JSON.parse(out) : null };
}

test("Server: HTTP API and Bearer token anti-spoofing", async () => {
  const dir = makeTempDir();
  try {
    const st = new FileStore(dir);
    const handler = createSwarmHandler(st);

    const c = await invokeHandler(handler, "POST", "/v1/swarms", { swarm_id: "s1", title: "http test" });
    assert.equal(c.status, 201);

    const added = await invokeHandler(handler, "POST", "/v1/swarms/s1/members", {
      name: "parser",
      brief: "do work",
    });
    assert.equal(added.status, 201);
    const token = (added.json as { token: string }).token;

    // Worker tries to spoof agent="coordinator", server enforces "parser".
    const posted = await invokeHandler(
      handler,
      "POST",
      "/v1/swarms/s1/messages",
      { agent: "coordinator", text: "@coordinator [done] finished" },
      { authorization: `Bearer ${token}` },
    );
    assert.equal(posted.status, 201);
    assert.equal((posted.json as { message: { agent: string } }).message.agent, "parser");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
