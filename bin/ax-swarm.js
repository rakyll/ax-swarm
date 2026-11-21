#!/usr/bin/env node
// @ts-check
import { mkdirSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join, resolve } from "node:path";
import { parseArgs } from "node:util";
import { CLIAXClient, MockAXClient } from "../src/axclient.js";
import { SwarmClient } from "../src/client.js";
import { DEFAULT_CHANNEL, WakeMode } from "../src/model.js";
import { Reconciler } from "../src/reconciler.js";
import { createSwarmServer } from "../src/server.js";
import { FileStore } from "../src/store.js";

function defaultServerUrl() {
  return process.env.SWARM_API_URL || "http://127.0.0.1:8788";
}

function defaultDataDir() {
  return process.env.AX_SWARM_DATA_DIR || join(homedir(), ".ax-swarm", "data");
}

function usage() {
  console.error(`ax-swarm — external swarm coordinator & wake-on-@mention control plane for AX

Usage:
  ax-swarm serve   [--addr 127.0.0.1:8788] [--data-dir ~/.ax-swarm/data] [--ax cli|mock] [--suspend-grace 15000]
  ax-swarm create  <swarm-id> [--title "..."] [--atespace swarm-<id>] [--workspace <ws>]
  ax-swarm title   <swarm-id> <title...>
  ax-swarm add     <swarm-id> <name> --brief "..." [--workspace <ws>] [--runtime <rt>] [--model <m>] [--image <img>]
  ax-swarm kill    <swarm-id> <name>
  ax-swarm status  <swarm-id>
  ax-swarm post    <swarm-id> [--as coordinator] [--channel main] <message...>
  ax-swarm read    <swarm-id> [--since 0] [--as <member>] [--channel <ch>]
  ax-swarm wait         <swarm-id> --as <member> [--since 0] [--wake-mode mentions|any]
  ax-swarm swarm        --json [--watch | --roster-only] [--db <db>] <swarm-id...>
  ax-swarm remote-board [--backend ax-swarm] [--label "AX Swarm"] [--cli <cmd>] [--cli-env AX_SWARM_CLI] [--out <path>]`);
}

/** @param {string} serverUrl */
function newClient(serverUrl) {
  return new SwarmClient(serverUrl, process.env.SWARM_TOKEN);
}

/** @param {string[]} args */
async function cmdServe(args) {
  const { values } = parseArgs({
    args,
    options: {
      addr: { type: "string", default: "127.0.0.1:8788" },
      "data-dir": { type: "string", default: defaultDataDir() },
      ax: { type: "string", default: "cli" },
      "ax-bin": { type: "string", default: "ax" },
      "ax-context": { type: "string" },
      "ax-server": { type: "string" },
      "swarm-url": { type: "string" },
      "default-image": { type: "string" },
      "poll-interval": { type: "string", default: "2000" },
      "suspend-grace": { type: "string", default: "15000" },
    },
  });

  const store = new FileStore(/** @type {string} */ (values["data-dir"]));
  const ax =
    values.ax === "mock"
      ? new MockAXClient()
      : values.ax === "cli"
        ? new CLIAXClient({
            binary: values["ax-bin"],
            context: values["ax-context"],
            server: values["ax-server"],
          })
        : (() => {
            throw new Error(`invalid --ax mode ${values.ax} (want cli or mock)`);
          })();

  const addr = /** @type {string} */ (values.addr);
  const lastColon = addr.lastIndexOf(":");
  const host = lastColon > 0 ? addr.slice(0, lastColon) : "127.0.0.1";
  const port = Number(lastColon >= 0 ? addr.slice(lastColon + 1) : addr);

  const rec = new Reconciler(store, ax, {
    pollIntervalMs: Number(values["poll-interval"]),
    suspendGraceMs: Number(values["suspend-grace"]),
    swarmApiUrl: values["swarm-url"] || `http://${host}:${port}`,
    defaultImage: values["default-image"],
  });
  rec.start();

  const srv = createSwarmServer(store);
  const shutdown = () => {
    rec.stop();
    srv.close(() => process.exit(0));
  };
  process.on("SIGINT", shutdown);
  process.on("SIGTERM", shutdown);

  await new Promise((resolve) => {
    srv.listen(port, host, () => {
      console.log(`ax-swarm listening on http://${host}:${port} (data=${values["data-dir"]}, ax=${values.ax})`);
      resolve(undefined);
    });
  });
}

/** @param {string[]} args */
async function cmdCreate(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      server: { type: "string", default: defaultServerUrl() },
      title: { type: "string" },
      atespace: { type: "string" },
      workspace: { type: "string" },
    },
  });
  if (positionals.length < 1) throw new Error("usage: ax-swarm create <swarm-id>");
  const sw = await newClient(/** @type {string} */ (values.server)).createSwarm({
    swarm_id: positionals[0],
    atespace: values.atespace,
    title: values.title,
    workspace_ref: values.workspace,
  });
  console.log(`created swarm ${sw.swarm_id} (atespace=${sw.atespace})`);
}

/** @param {string[]} args */
async function cmdTitle(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: { server: { type: "string", default: defaultServerUrl() } },
  });
  if (positionals.length < 2) throw new Error("usage: ax-swarm title <swarm-id> <title...>");
  const sw = await newClient(/** @type {string} */ (values.server)).setTitle(
    positionals[0],
    positionals.slice(1).join(" "),
  );
  console.log(`swarm ${sw.swarm_id} title: ${sw.title}`);
}

/** @param {string[]} args */
async function cmdAdd(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      server: { type: "string", default: defaultServerUrl() },
      brief: { type: "string", default: "" },
      role: { type: "string", default: "worker" },
      model: { type: "string" },
      runtime: { type: "string" },
      image: { type: "string" },
      workspace: { type: "string" },
      "node-id": { type: "string", default: "ax" },
    },
  });
  if (positionals.length < 2) throw new Error("usage: ax-swarm add <swarm-id> <name> --brief '...'");
  const resp = await newClient(/** @type {string} */ (values.server)).addMember({
    swarm_id: positionals[0],
    name: positionals[1],
    role: values.role,
    model: values.model,
    runtime: values.runtime,
    image: values.image,
    workspace: values.workspace,
    brief: values.brief,
    node_id: values["node-id"],
  });
  console.log(
    `added ${resp.member.name} (task=${resp.member.task_name}, phase=${resp.member.phase}, seq=#${resp.brief_message?.seq ?? 0})`,
  );
}

/** @param {string[]} args */
async function cmdKill(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: { server: { type: "string", default: defaultServerUrl() } },
  });
  if (positionals.length < 2) throw new Error("usage: ax-swarm kill <swarm-id> <name>");
  const m = await newClient(/** @type {string} */ (values.server)).killMember(positionals[0], positionals[1]);
  console.log(`killed ${m.name} (phase=${m.phase})`);
}

/** @param {string[]} args */
async function cmdStatus(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: { server: { type: "string", default: defaultServerUrl() } },
  });
  if (positionals.length < 1) throw new Error("usage: ax-swarm status <swarm-id>");
  const sw = await newClient(/** @type {string} */ (values.server)).getSwarm(positionals[0]);
  console.log(`Swarm: ${sw.swarm_id}  Title: "${sw.title}"  Atespace: ${sw.atespace}  Head: #${sw.head_seq}\n`);
  console.log("NAME\tROLE\tPHASE\tDESIRED\tWAKE\tREAD\tRUNTIME");
  for (const name of Object.keys(sw.members).sort()) {
    const m = sw.members[name];
    console.log(
      `${m.name}\t${m.role}\t${m.phase}\t${m.desired_state}\t${m.wake_mode}\t#${m.last_read_seq}\t${m.runtime ?? ""}`,
    );
  }
}

/** @param {string[]} args */
async function cmdPost(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      server: { type: "string", default: defaultServerUrl() },
      as: { type: "string", default: "coordinator" },
      channel: { type: "string", default: DEFAULT_CHANNEL },
    },
  });
  if (positionals.length < 2) throw new Error("usage: ax-swarm post <swarm-id> <message...>");
  const resp = await newClient(/** @type {string} */ (values.server)).postMessage({
    swarm_id: positionals[0],
    agent: values.as,
    channel: values.channel,
    text: positionals.slice(1).join(" "),
  });
  console.log(`#${resp.message.seq} [${resp.message.agent}] ${resp.message.text} (woken: ${resp.woken.join(",")})`);
}

/** @param {string[]} args */
async function cmdRead(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      server: { type: "string", default: defaultServerUrl() },
      since: { type: "string", default: "0" },
      as: { type: "string" },
      channel: { type: "string" },
    },
  });
  if (positionals.length < 1) throw new Error("usage: ax-swarm read <swarm-id>");
  const resp = await newClient(/** @type {string} */ (values.server)).readBoard(positionals[0], {
    since: Number(values.since),
    member: values.as,
    channel: values.channel,
  });
  for (const m of resp.messages) {
    console.log(`#${m.seq} (${m.time}) [${m.agent}] ${m.text}`);
  }
}

/** @param {string[]} args */
async function cmdWait(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      server: { type: "string", default: defaultServerUrl() },
      as: { type: "string", default: process.env.SWARM_MEMBER || "" },
      since: { type: "string", default: "0" },
      "wake-mode": { type: "string", default: WakeMode.Mentions },
    },
  });
  if (positionals.length < 1 || !values.as) {
    throw new Error("usage: ax-swarm wait <swarm-id> --as <member> [--since N]");
  }
  const res = await newClient(/** @type {string} */ (values.server)).parkMember({
    swarm_id: positionals[0],
    name: values.as,
    since: Number(values.since),
    wake_mode: values["wake-mode"],
  });
  if (res.park_blocked) {
    console.log(`park blocked (${res.unread?.length ?? 0} unread matching messages)`);
    for (const m of res.unread ?? []) {
      console.log(`#${m.seq} [${m.agent}] ${m.text}`);
    }
    return;
  }
  console.log(`parked ${res.member.name} (waiting since #${res.member.wait_since}, wake=${res.member.wake_mode})`);
}

/** @param {string[]} args */
async function cmdSwarmViewer(args) {
  const { values, positionals } = parseArgs({
    args,
    allowPositionals: true,
    options: {
      server: { type: "string", default: defaultServerUrl() },
      json: { type: "boolean", default: false },
      watch: { type: "boolean", default: false },
      "roster-only": { type: "boolean", default: false },
      db: { type: "string" },
    },
  });
  if (!values.json || positionals.length < 1) {
    throw new Error("usage: ax-swarm swarm --json [--watch|--roster-only] [--db <db>] <swarm-id...>");
  }

  const client = newClient(/** @type {string} */ (values.server));

  if (values.watch) {
    const ac = new AbortController();
    process.stdin.resume();
    process.stdin.on("end", () => ac.abort());
    process.stdin.on("close", () => ac.abort());
    await client.watch(
      positionals[0],
      (snap) => {
        process.stdout.write(`${JSON.stringify(snap)}\n`);
      },
      ac.signal,
    );
    return;
  }

  if (values["roster-only"]) {
    for (const id of positionals) {
      try {
        const sw = await client.getSwarm(id);
        process.stdout.write(
          `${JSON.stringify({
            version: 1,
            swarm_id: sw.swarm_id,
            title: sw.title,
            head: sw.head_seq,
            since: 0,
            channels: sw.channels,
            members: sw.members,
          })}\n`,
        );
      } catch {
        // Skip unknown swarms in batch roster probe.
      }
    }
    return;
  }

  const sw = await client.getSwarm(positionals[0]);
  const read = await client.readBoard(positionals[0]);
  process.stdout.write(
    `${JSON.stringify({
      version: 1,
      swarm_id: sw.swarm_id,
      title: sw.title,
      head: read.head,
      since: 0,
      channels: sw.channels,
      members: sw.members,
      messages: read.messages,
    })}\n`,
  );
}

/** @param {string[]} args */
async function cmdRemoteBoard(args) {
  const { values } = parseArgs({
    args,
    options: {
      backend: { type: "string", default: "ax-swarm" },
      label: { type: "string", default: "AX Swarm" },
      cli: { type: "string", multiple: true },
      "cli-env": { type: "string", default: "AX_SWARM_CLI" },
      out: { type: "string" },
    },
  });

  const cliEntries =
    values.cli && values.cli.length > 0
      ? values.cli
      : ["ax-swarm", resolve(process.argv[1])];

  const doc = {
    backend: values.backend,
    label: values.label,
    cli: [...new Set(cliEntries)],
    cli_env: values["cli-env"],
  };
  const json = `${JSON.stringify(doc, null, 2)}\n`;

  if (values.out) {
    mkdirSync(dirname(resolve(values.out)), { recursive: true });
    writeFileSync(values.out, json, "utf8");
    console.log(`wrote ${values.out}`);
    return;
  }
  process.stdout.write(json);
}

const [, , cmd, ...rest] = process.argv;
if (!cmd || cmd === "-h" || cmd === "--help" || cmd === "help") {
  usage();
  process.exit(cmd ? 0 : 2);
}

/** @type {Record<string, (args: string[]) => Promise<void>>} */
const commands = {
  serve: cmdServe,
  create: cmdCreate,
  title: cmdTitle,
  add: cmdAdd,
  kill: cmdKill,
  status: cmdStatus,
  post: cmdPost,
  read: cmdRead,
  wait: cmdWait,
  swarm: cmdSwarmViewer,
  "remote-board": cmdRemoteBoard,
};

const fn = commands[cmd];
if (!fn) {
  console.error(`unknown command "${cmd}"\n`);
  usage();
  process.exit(2);
}

fn(rest).catch((err) => {
  console.error(`ax-swarm ${cmd}: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
