// @ts-check
import { createServer } from "node:http";
import { StoreError } from "./store.js";

/** @param {import("node:http").IncomingMessage} req */
async function readJSON(req) {
  const chunks = [];
  for await (const chunk of req) {
    chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  return raw ? JSON.parse(raw) : {};
}

/**
 * @param {import("node:http").ServerResponse} res
 * @param {number} status
 * @param {unknown} body
 */
function writeJSON(res, status, body) {
  const payload = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(payload);
}

/**
 * @param {import("node:http").ServerResponse} res
 * @param {unknown} err
 */
function writeError(res, err) {
  if (err instanceof StoreError) {
    const status =
      err.code === "NOT_FOUND" ? 404 : err.code === "ALREADY_EXISTS" ? 409 : 400;
    writeJSON(res, status, { error: err.message });
    return;
  }
  const message = err instanceof Error ? err.message : String(err);
  writeJSON(res, 500, { error: message });
}

/** @param {import("./store.js").FileStore} store */
export function createSwarmHandler(store) {
  /** @param {import("node:http").IncomingMessage} req */
  function bearerIdentity(req) {
    const auth = req.headers.authorization;
    if (!auth?.startsWith("Bearer ")) return { hasToken: false, entry: undefined };
    const token = auth.slice("Bearer ".length).trim();
    const entry = token ? store.authenticateToken(token) : undefined;
    return { hasToken: true, entry };
  }

  /**
   * @param {import("node:http").IncomingMessage} req
   * @param {import("node:http").ServerResponse} res
   */
  return async function handle(req, res) {
    try {
      const method = req.method || "GET";
      const url = new URL(req.url || "/", "http://localhost");
      const parts = url.pathname.split("/").filter(Boolean).map(decodeURIComponent);

      if (method === "GET" && url.pathname === "/healthz") {
        writeJSON(res, 200, { status: "ok" });
        return;
      }

      if (parts[0] !== "v1" || parts[1] !== "swarms") {
        writeJSON(res, 404, { error: "not found" });
        return;
      }

      if (parts.length === 2) {
        if (method === "POST") {
          writeJSON(res, 201, store.createSwarm(await readJSON(req)));
          return;
        }
        if (method === "GET") {
          writeJSON(res, 200, { swarms: store.listSwarms() });
          return;
        }
      }

      const swarmId = parts[2];
      if (parts.length === 3 && method === "GET") {
        writeJSON(res, 200, store.getSwarm(swarmId));
        return;
      }

      if (parts.length === 4 && parts[3] === "title" && method === "PUT") {
        const body = await readJSON(req);
        writeJSON(res, 200, store.setTitle(swarmId, body.title));
        return;
      }

      if (parts.length === 4 && parts[3] === "members" && method === "POST") {
        const body = await readJSON(req);
        writeJSON(res, 201, store.addMember({ ...body, swarm_id: swarmId }));
        return;
      }

      if (parts.length === 5 && parts[3] === "members" && method === "DELETE") {
        writeJSON(res, 200, store.killMember(swarmId, parts[4]));
        return;
      }

      if (parts.length === 6 && parts[3] === "members" && parts[5] === "park" && method === "POST") {
        const memberName = parts[4];
        const auth = bearerIdentity(req);
        if (auth.hasToken) {
          if (
            !auth.entry ||
            auth.entry.swarm_id !== swarmId ||
            auth.entry.member_name.toLowerCase() !== memberName.toLowerCase()
          ) {
            writeJSON(res, 401, { error: "invalid member token" });
            return;
          }
        }
        const body = await readJSON(req);
        writeJSON(res, 200, store.parkMember({ ...body, swarm_id: swarmId, name: memberName }));
        return;
      }

      if (parts.length === 4 && parts[3] === "messages") {
        if (method === "POST") {
          const body = await readJSON(req);
          const auth = bearerIdentity(req);
          if (auth.hasToken) {
            if (!auth.entry || auth.entry.swarm_id !== swarmId) {
              writeJSON(res, 401, { error: "invalid member token" });
              return;
            }
            body.agent = auth.entry.member_name;
          }
          writeJSON(res, 201, store.postMessage({ ...body, swarm_id: swarmId }));
          return;
        }
        if (method === "GET") {
          const since = Number(url.searchParams.get("since") || "0");
          const channel = url.searchParams.get("channel") || "";
          let member = url.searchParams.get("member") || "";
          const auth = bearerIdentity(req);
          if (auth.hasToken) {
            if (!auth.entry || auth.entry.swarm_id !== swarmId) {
              writeJSON(res, 401, { error: "invalid member token" });
              return;
            }
            member = auth.entry.member_name;
          }
          writeJSON(res, 200, store.readBoard(swarmId, member, since, channel));
          return;
        }
      }

      if (parts.length === 4 && parts[3] === "watch" && method === "GET") {
        res.writeHead(200, {
          "Content-Type": "application/x-ndjson",
          "Cache-Control": "no-cache",
        });
        const unsub = store.watch(swarmId, (snap) => {
          res.write(`${JSON.stringify(snap)}\n`);
        });
        req.on("close", unsub);
        return;
      }

      writeJSON(res, 404, { error: "not found" });
    } catch (err) {
      writeError(res, err);
    }
  };
}

/** @param {import("./store.js").FileStore} store */
export function createSwarmServer(store) {
  return createServer(createSwarmHandler(store));
}
