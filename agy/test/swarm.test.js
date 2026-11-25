// @ts-check
import assert from "node:assert/strict";
import { EventEmitter } from "node:events";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { Readable } from "node:stream";
import { test } from "node:test";
import { MockAXClient } from "../src/axclient.js";
import { DesiredState, Phase } from "../src/model.js";
import { Reconciler } from "../src/reconciler.js";
import { createSwarmHandler } from "../src/server.js";
import { FileStore } from "../src/store.js";

function makeTempDir() {
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

    const park2 = st.parkMember({ swarm_id: "sw1", name: "parser-1", since: 1 });
    assert.equal(park2.park_blocked, true);
    assert.equal(park2.unread?.length, 1);
    assert.equal(park2.unread?.[0].seq, 2);

    /** @type {any[]} */
    const snaps = [];
    const unsub = st.watch("sw1", (s) => snaps.push(s));
    st.postMessage({ swarm_id: "sw1", agent: "coordinator", text: "@all status check" });
    unsub();
    assert.equal(snaps.length, 2);
    assert.equal(snaps[0].since, 0);
    assert.equal(snaps[1].head, 3);
    assert.equal(snaps[1].messages.length, 1);

    const st2 = new FileStore(dir);
    assert.deepEqual(st2.authenticateToken(token), { swarm_id: "sw1", member_name: "parser-1" });
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

    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), "Working");

    st.parkMember({ swarm_id: "sw1", name: "parser", since: 1 });
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), "Suspended");

    st.postMessage({ swarm_id: "sw1", agent: "verifier", text: "@parser fix lint" });
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), "Working");

    st.killMember("sw1", "parser");
    await rec.reconcileOnce();
    assert.equal(mockAX.state("swarm-sw1", "parser"), undefined);
    assert.equal(st.getSwarm("sw1").members["parser"].phase, Phase.Killed);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

/**
 * @param {any} handler
 * @param {string} method
 * @param {string} url
 * @param {unknown} [body]
 * @param {Record<string, string>} [headers]
 */
async function invokeHandler(handler, method, url, body, headers = {}) {
  const payload = body !== undefined ? JSON.stringify(body) : "";
  const req = /** @type {any} */ (Readable.from(payload ? [Buffer.from(payload)] : []));
  req.method = method;
  req.url = url;
  req.headers = headers;

  let status = 200;
  let out = "";
  const emitter = new EventEmitter();
  const res = Object.assign(emitter, {
    /** @param {number} code */
    writeHead(code) {
      status = code;
      return this;
    },
    /** @param {string | Buffer} chunk */
    write(chunk) {
      out += chunk.toString();
      return true;
    },
    /** @param {string | Buffer} [chunk] */
    end(chunk) {
      if (chunk) out += chunk.toString();
      emitter.emit("finish");
      return this;
    },
  });

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
    const token = added.json.token;

    const posted = await invokeHandler(
      handler,
      "POST",
      "/v1/swarms/s1/messages",
      { agent: "coordinator", text: "@coordinator [done] finished" },
      { authorization: `Bearer ${token}` },
    );
    assert.equal(posted.status, 201);
    assert.equal(posted.json.message.agent, "parser");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});
