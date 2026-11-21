#!/usr/bin/env node
// @ts-check
import { spawn } from "node:child_process";
import { existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { parseArgs } from "node:util";
import { SwarmClient } from "../src/client.js";
import { WakeMode } from "../src/model.js";

/**
 * @param {string} member
 * @param {any[]} msgs
 */
function formatPrompt(member, msgs) {
  const lines = [`You are swarm member @${member}. New board messages:\n`];
  for (const m of msgs) {
    lines.push(`#${m.seq} [${m.agent}]: ${m.text}`);
  }
  lines.push("\nPost updates with `ax-swarm post` and tag the next peer or @coordinator when done.\n");
  return lines.join("\n");
}

/**
 * @param {string} harnessCmd
 * @param {string} prompt
 * @returns {Promise<void>}
 */
function runHarness(harnessCmd, prompt) {
  if (!harnessCmd.trim()) {
    console.log(`no SWARM_HARNESS_CMD configured; received prompt:\n${prompt}`);
    return Promise.resolve();
  }
  return new Promise((resolve, reject) => {
    const child = spawn("sh", ["-c", harnessCmd], { stdio: ["pipe", "inherit", "inherit"] });
    child.stdin.end(prompt);
    child.on("error", reject);
    child.on("close", (code) => {
      if (code === 0) resolve();
      else reject(new Error(`harness exited with code ${code}`));
    });
  });
}

async function main() {
  const { values } = parseArgs({
    options: {
      "api-url": { type: "string", default: process.env.SWARM_API_URL || "" },
      swarm: { type: "string", default: process.env.SWARM_ID || "" },
      member: { type: "string", default: process.env.SWARM_MEMBER || "" },
      token: { type: "string", default: process.env.SWARM_TOKEN || "" },
      harness: { type: "string", default: process.env.SWARM_HARNESS_CMD || "" },
      "state-dir": { type: "string", default: "/workspace/.ax-swarm" },
    },
  });

  const apiUrl = /** @type {string} */ (values["api-url"]);
  const swarmId = /** @type {string} */ (values.swarm);
  const member = /** @type {string} */ (values.member);
  if (!apiUrl || !swarmId || !member) {
    throw new Error("SWARM_API_URL, SWARM_ID, and SWARM_MEMBER are required");
  }

  const stateDir = /** @type {string} */ (values["state-dir"]);
  mkdirSync(stateDir, { recursive: true });
  const cursorPath = join(stateDir, "last_seq");
  let since = existsSync(cursorPath) ? Number(readFileSync(cursorPath, "utf8").trim()) || 0 : 0;

  const client = new SwarmClient(apiUrl, values.token || undefined);

  while (true) {
    const resp = await client.readBoard(swarmId, { member, since });
    if (resp.messages.length > 0) {
      const prompt = formatPrompt(member, resp.messages);
      try {
        await runHarness(/** @type {string} */ (values.harness), prompt);
      } catch (err) {
        await client
          .postMessage({
            swarm_id: swarmId,
            agent: member,
            text: `@coordinator [error] worker ${member} failed: ${err instanceof Error ? err.message : String(err)}`,
          })
          .catch(() => {});
        process.exit(1);
      }
      since = resp.head;
      writeFileSync(cursorPath, String(since), "utf8");
    }

    const park = await client.parkMember({
      swarm_id: swarmId,
      name: member,
      since,
      wake_mode: WakeMode.Mentions,
    });
    if (park.park_blocked) continue;

    console.log(`worker ${member} parked at #${since}; exiting cleanly for AX suspend`);
    return;
  }
}

main().catch((err) => {
  console.error(`ax-swarm-worker: ${err instanceof Error ? err.message : String(err)}`);
  process.exit(1);
});
