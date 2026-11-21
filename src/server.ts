import { createServer } from "node:http";
import type { IncomingMessage, Server, ServerResponse } from "node:http";
import { StoreError } from "./store.ts";
import type {
  AddMemberParams,
  CreateSwarmParams,
  FileStore,
  ParkMemberParams,
  PostMessageParams,
} from "./store.ts";

async function readJSON<T>(req: IncomingMessage): Promise<T> {
  const chunks: Buffer[] = [];
  for await (const chunk of req) {
    chunks.push(typeof chunk === "string" ? Buffer.from(chunk) : chunk);
  }
  const raw = Buffer.concat(chunks).toString("utf8");
  return (raw ? JSON.parse(raw) : {}) as T;
}

function writeJSON(res: ServerResponse, status: number, body: unknown): void {
  const payload = JSON.stringify(body);
  res.writeHead(status, { "Content-Type": "application/json" });
  res.end(payload);
}

function writeError(res: ServerResponse, err: unknown): void {
  if (err instanceof StoreError) {
    const status =
      err.code === "NOT_FOUND" ? 404 : err.code === "ALREADY_EXISTS" ? 409 : 400;
    writeJSON(res, status, { error: err.message });
    return;
  }
  const message = err instanceof Error ? err.message : String(err);
  writeJSON(res, 500, { error: message });
}

export function createSwarmHandler(store: FileStore) {
  function bearerIdentity(req: IncomingMessage) {
    const auth = req.headers.authorization;
    if (!auth?.startsWith("Bearer ")) return { hasToken: false as const };
    const token = auth.slice("Bearer ".length).trim();
    const entry = token ? store.authenticateToken(token) : undefined;
    return { hasToken: true as const, entry };
  }

  return async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
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

      // /v1/swarms
      if (parts.length === 2) {
        if (method === "POST") {
          const body = await readJSON<CreateSwarmParams>(req);
          writeJSON(res, 201, store.createSwarm(body));
          return;
        }
        if (method === "GET") {
          writeJSON(res, 200, { swarms: store.listSwarms() });
          return;
        }
      }

      const swarmId = parts[2];
      // /v1/swarms/:id
      if (parts.length === 3 && method === "GET") {
        writeJSON(res, 200, store.getSwarm(swarmId));
        return;
      }

      // /v1/swarms/:id/title
      if (parts.length === 4 && parts[3] === "title" && method === "PUT") {
        const body = await readJSON<{ title: string }>(req);
        writeJSON(res, 200, store.setTitle(swarmId, body.title));
        return;
      }

      // /v1/swarms/:id/members
      if (parts.length === 4 && parts[3] === "members" && method === "POST") {
        const body = await readJSON<Omit<AddMemberParams, "swarm_id">>(req);
        writeJSON(res, 201, store.addMember({ ...body, swarm_id: swarmId }));
        return;
      }

      // /v1/swarms/:id/members/:name
      if (parts.length === 5 && parts[3] === "members" && method === "DELETE") {
        writeJSON(res, 200, store.killMember(swarmId, parts[4]));
        return;
      }

      // /v1/swarms/:id/members/:name/park
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
        const body = await readJSON<Omit<ParkMemberParams, "swarm_id" | "name">>(req);
        writeJSON(res, 200, store.parkMember({ ...body, swarm_id: swarmId, name: memberName }));
        return;
      }

      // /v1/swarms/:id/messages
      if (parts.length === 4 && parts[3] === "messages") {
        if (method === "POST") {
          const body = await readJSON<Omit<PostMessageParams, "swarm_id">>(req);
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

      // /v1/swarms/:id/watch (NDJSON stream)
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

export function createSwarmServer(store: FileStore): Server {
  return createServer(createSwarmHandler(store));
}
