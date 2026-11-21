import { randomBytes } from "node:crypto";
import {
  appendFileSync,
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import {
  DEFAULT_CHANNEL,
  DesiredState,
  OwnerKind,
  Phase,
  WakeMode,
  memberMatchesMessage,
  parseMentions,
} from "./model.ts";
import type {
  Member,
  Message,
  Swarm,
  WatchSnapshot,
} from "./model.ts";

export class StoreError extends Error {
  readonly code: "NOT_FOUND" | "ALREADY_EXISTS" | "INVALID_ARG";

  constructor(code: "NOT_FOUND" | "ALREADY_EXISTS" | "INVALID_ARG", message: string) {
    super(message);
    this.code = code;
  }
}

export interface CreateSwarmParams {
  swarm_id: string;
  atespace?: string;
  title?: string;
  workspace_ref?: string;
}

export interface AddMemberParams {
  swarm_id: string;
  name: string;
  role?: string;
  model?: string;
  runtime?: string;
  image?: string;
  workspace?: string;
  brief?: string;
  channels?: string[];
  node_id?: string;
  owner_kind?: OwnerKind;
  owner_label?: string;
  posted_by?: string;
}

export interface PostMessageParams {
  swarm_id: string;
  channel?: string;
  agent: string;
  text: string;
}

export interface ParkMemberParams {
  swarm_id: string;
  name: string;
  since?: number;
  wake_mode?: WakeMode;
}

export interface ParkResult {
  member: Member;
  park_blocked: boolean;
  unread?: Message[];
}

interface TokenEntry {
  swarm_id: string;
  member_name: string;
}

interface Subscriber {
  lastSeq: number;
  onSnapshot: (snap: WatchSnapshot) => void;
}

interface SwarmData {
  swarm: Swarm;
  messages: Message[];
  subs: Set<Subscriber>;
}

function writeAtomic(path: string, data: string, mode?: number): void {
  const tmp = `${path}.tmp`;
  writeFileSync(tmp, data, { encoding: "utf8", mode });
  renameSync(tmp, path);
}

function cloneSwarm(sw: Swarm): Swarm {
  return structuredClone(sw);
}

export class FileStore {
  private readonly dir: string;
  private readonly swarms = new Map<string, SwarmData>();
  private readonly tokens = new Map<string, TokenEntry>();

  constructor(dir: string) {
    this.dir = dir;
    mkdirSync(dir, { recursive: true });
    this.loadAll();
  }

  private loadAll(): void {
    const tokensPath = join(this.dir, "tokens.json");
    if (existsSync(tokensPath)) {
      const raw = JSON.parse(readFileSync(tokensPath, "utf8")) as Record<string, TokenEntry>;
      for (const [k, v] of Object.entries(raw)) {
        this.tokens.set(k, v);
      }
    }

    for (const entry of readdirSync(this.dir, { withFileTypes: true })) {
      if (!entry.isDirectory()) continue;
      const statePath = join(this.dir, entry.name, "state.json");
      if (!existsSync(statePath)) continue;

      const swarm = JSON.parse(readFileSync(statePath, "utf8")) as Swarm;
      swarm.members ??= {};

      const boardPath = join(this.dir, entry.name, "board.jsonl");
      const messages: Message[] = [];
      if (existsSync(boardPath)) {
        for (const line of readFileSync(boardPath, "utf8").split("\n")) {
          const trimmed = line.trim();
          if (!trimmed) continue;
          messages.push(JSON.parse(trimmed) as Message);
        }
      }
      if (messages.length > 0 && messages[messages.length - 1].seq > swarm.head_seq) {
        swarm.head_seq = messages[messages.length - 1].seq;
      }
      this.swarms.set(swarm.swarm_id, { swarm, messages, subs: new Set() });
    }
  }

  private saveSwarm(sw: Swarm): void {
    const dir = join(this.dir, sw.swarm_id);
    mkdirSync(dir, { recursive: true });
    writeAtomic(join(dir, "state.json"), JSON.stringify(sw, null, 2));
  }

  private appendMessage(swarmId: string, msg: Message): void {
    const dir = join(this.dir, swarmId);
    mkdirSync(dir, { recursive: true });
    appendFileSync(join(dir, "board.jsonl"), `${JSON.stringify(msg)}\n`, "utf8");
  }

  private saveTokens(): void {
    const obj: Record<string, TokenEntry> = {};
    for (const [k, v] of this.tokens.entries()) {
      obj[k] = v;
    }
    writeAtomic(join(this.dir, "tokens.json"), JSON.stringify(obj, null, 2), 0o600);
  }

  private notify(sd: SwarmData): void {
    for (const sub of sd.subs) {
      const delta = sd.messages.filter((m) => m.seq > sub.lastSeq);
      const snap: WatchSnapshot = {
        version: 1,
        swarm_id: sd.swarm.swarm_id,
        title: sd.swarm.title,
        head: sd.swarm.head_seq,
        since: sub.lastSeq,
        channels: sd.swarm.channels ? structuredClone(sd.swarm.channels) : undefined,
        members: structuredClone(sd.swarm.members),
        messages: delta,
      };
      sub.lastSeq = sd.swarm.head_seq;
      sub.onSnapshot(snap);
    }
  }

  createSwarm(p: CreateSwarmParams): Swarm {
    if (!p.swarm_id) throw new StoreError("INVALID_ARG", "swarm_id is required");
    if (this.swarms.has(p.swarm_id)) {
      throw new StoreError("ALREADY_EXISTS", `swarm ${p.swarm_id} already exists`);
    }
    const now = new Date().toISOString();
    const sw: Swarm = {
      swarm_id: p.swarm_id,
      atespace: p.atespace || `swarm-${p.swarm_id}`,
      title: p.title ?? "",
      workspace_ref: p.workspace_ref,
      head_seq: 0,
      members: {},
      created_at: now,
      updated_at: now,
    };
    this.saveSwarm(sw);
    this.swarms.set(p.swarm_id, { swarm: sw, messages: [], subs: new Set() });
    return cloneSwarm(sw);
  }

  getSwarm(swarmId: string): Swarm {
    const sd = this.swarms.get(swarmId);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${swarmId} not found`);
    return cloneSwarm(sd.swarm);
  }

  listSwarms(): Swarm[] {
    return [...this.swarms.values()]
      .map((sd) => cloneSwarm(sd.swarm))
      .sort((a, b) => a.created_at.localeCompare(b.created_at));
  }

  setTitle(swarmId: string, title: string): Swarm {
    const sd = this.swarms.get(swarmId);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${swarmId} not found`);
    sd.swarm.title = title;
    sd.swarm.updated_at = new Date().toISOString();
    this.saveSwarm(sd.swarm);
    this.notify(sd);
    return cloneSwarm(sd.swarm);
  }

  addMember(p: AddMemberParams): { member: Member; token: string; brief_message?: Message } {
    if (!p.swarm_id || !p.name) {
      throw new StoreError("INVALID_ARG", "swarm_id and name are required");
    }
    const name = p.name.toLowerCase();
    if (name === "all") throw new StoreError("INVALID_ARG", `"all" is reserved`);

    const sd = this.swarms.get(p.swarm_id);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${p.swarm_id} not found`);
    if (sd.swarm.members[name]) {
      throw new StoreError("ALREADY_EXISTS", `member ${name} already exists`);
    }

    const now = new Date().toISOString();
    const token = randomBytes(24).toString("hex");
    let briefMsg: Message | undefined;

    if (p.brief?.trim()) {
      sd.swarm.head_seq += 1;
      const text = `@${name} ${p.brief.trim()}`;
      briefMsg = {
        seq: sd.swarm.head_seq,
        channel: p.channels?.[0] || DEFAULT_CHANNEL,
        agent: (p.posted_by || "coordinator").toLowerCase(),
        text,
        mentions: parseMentions(text),
        time: now,
      };
      sd.messages.push(briefMsg);
      this.appendMessage(p.swarm_id, briefMsg);
    }

    const startSeq = briefMsg ? briefMsg.seq - 1 : sd.swarm.head_seq;
    const member: Member = {
      name,
      task_name: name,
      conversation_id: `ax-swarm:${sd.swarm.atespace}:${name}`,
      role: p.role || "worker",
      model: p.model,
      runtime: p.runtime,
      image: p.image,
      workspace: p.workspace || sd.swarm.workspace_ref,
      brief: p.brief,
      channels: p.channels ? [...p.channels] : undefined,
      wake_mode: WakeMode.Mentions,
      wait_since: startSeq,
      last_read_seq: startSeq,
      phase: Phase.Queued,
      desired_state: DesiredState.Running,
      node_id: p.node_id,
      owner_kind: p.owner_kind || OwnerKind.Agent,
      owner_label: p.owner_label || name,
      killed: false,
      updated_at: now,
    };

    sd.swarm.members[name] = member;
    sd.swarm.updated_at = now;
    this.tokens.set(token, { swarm_id: p.swarm_id, member_name: name });
    this.saveSwarm(sd.swarm);
    this.saveTokens();
    this.notify(sd);

    return { member: structuredClone(member), token, brief_message: briefMsg };
  }

  killMember(swarmId: string, rawName: string): Member {
    const sd = this.swarms.get(swarmId);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${swarmId} not found`);
    const name = rawName.toLowerCase();
    const m = sd.swarm.members[name];
    if (!m) throw new StoreError("NOT_FOUND", `member ${name} not found`);

    const now = new Date().toISOString();
    m.killed = true;
    m.desired_state = DesiredState.Killed;
    m.phase = Phase.Killed;
    m.updated_at = now;
    sd.swarm.updated_at = now;
    this.saveSwarm(sd.swarm);
    this.notify(sd);
    return structuredClone(m);
  }

  updateMemberPhase(swarmId: string, rawName: string, phase: Phase): Member {
    const sd = this.swarms.get(swarmId);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${swarmId} not found`);
    const name = rawName.toLowerCase();
    const m = sd.swarm.members[name];
    if (!m) throw new StoreError("NOT_FOUND", `member ${name} not found`);
    if (m.phase === phase) return structuredClone(m);

    const now = new Date().toISOString();
    m.phase = phase;
    m.updated_at = now;
    sd.swarm.updated_at = now;
    this.saveSwarm(sd.swarm);
    this.notify(sd);
    return structuredClone(m);
  }

  authenticateToken(token: string): TokenEntry | undefined {
    return this.tokens.get(token);
  }

  postMessage(p: PostMessageParams): { message: Message; woken: string[] } {
    if (!p.swarm_id || !p.agent || !p.text.trim()) {
      throw new StoreError("INVALID_ARG", "swarm_id, agent, and text are required");
    }
    const sd = this.swarms.get(p.swarm_id);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${p.swarm_id} not found`);

    const now = new Date().toISOString();
    sd.swarm.head_seq += 1;
    const msg: Message = {
      seq: sd.swarm.head_seq,
      channel: p.channel || DEFAULT_CHANNEL,
      agent: p.agent.toLowerCase(),
      text: p.text,
      mentions: parseMentions(p.text),
      time: now,
    };
    sd.messages.push(msg);

    const woken: string[] = [];
    for (const [name, m] of Object.entries(sd.swarm.members)) {
      if (memberMatchesMessage(m, msg)) {
        if (m.desired_state !== DesiredState.Running) {
          m.desired_state = DesiredState.Running;
          m.parked_at = undefined;
          m.updated_at = now;
        }
        woken.push(name);
      }
    }
    woken.sort();
    sd.swarm.updated_at = now;

    this.appendMessage(p.swarm_id, msg);
    this.saveSwarm(sd.swarm);
    this.notify(sd);
    return { message: msg, woken };
  }

  readBoard(
    swarmId: string,
    memberName = "",
    since = 0,
    channel = "",
  ): { head: number; messages: Message[] } {
    const sd = this.swarms.get(swarmId);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${swarmId} not found`);

    const messages = sd.messages.filter((m) => {
      if (m.seq <= since) return false;
      if (channel && channel !== "*" && m.channel !== channel) return false;
      return true;
    });

    if (memberName) {
      const m = sd.swarm.members[memberName.toLowerCase()];
      if (m && sd.swarm.head_seq > m.last_read_seq) {
        m.last_read_seq = sd.swarm.head_seq;
        m.updated_at = new Date().toISOString();
        this.saveSwarm(sd.swarm);
      }
    }
    return { head: sd.swarm.head_seq, messages };
  }

  parkMember(p: ParkMemberParams): ParkResult {
    if (!p.swarm_id || !p.name) {
      throw new StoreError("INVALID_ARG", "swarm_id and name are required");
    }
    const wakeMode = p.wake_mode || WakeMode.Mentions;
    if (wakeMode !== WakeMode.Mentions && wakeMode !== WakeMode.Any) {
      throw new StoreError("INVALID_ARG", `invalid wake_mode ${wakeMode}`);
    }
    const sd = this.swarms.get(p.swarm_id);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${p.swarm_id} not found`);
    const name = p.name.toLowerCase();
    const m = sd.swarm.members[name];
    if (!m) throw new StoreError("NOT_FOUND", `member ${name} not found`);
    if (m.killed) return { member: structuredClone(m), park_blocked: false };

    const since = p.since ?? 0;
    const candidate: Member = { ...m, wake_mode: wakeMode };
    const unread = sd.messages.filter(
      (msg) => msg.seq > since && memberMatchesMessage(candidate, msg),
    );
    if (unread.length > 0) {
      return { member: structuredClone(m), park_blocked: true, unread };
    }

    const now = new Date().toISOString();
    m.wake_mode = wakeMode;
    m.wait_since = since;
    if (since > m.last_read_seq) m.last_read_seq = since;
    m.desired_state = DesiredState.Waiting;
    m.phase = Phase.Waiting;
    m.parked_at = now;
    m.updated_at = now;
    sd.swarm.updated_at = now;

    this.saveSwarm(sd.swarm);
    this.notify(sd);
    return { member: structuredClone(m), park_blocked: false };
  }

  watch(swarmId: string, onSnapshot: (snap: WatchSnapshot) => void): () => void {
    const sd = this.swarms.get(swarmId);
    if (!sd) throw new StoreError("NOT_FOUND", `swarm ${swarmId} not found`);

    const sub: Subscriber = { lastSeq: sd.swarm.head_seq, onSnapshot };
    sd.subs.add(sub);

    // Emit initial full snapshot (since = 0).
    onSnapshot({
      version: 1,
      swarm_id: sd.swarm.swarm_id,
      title: sd.swarm.title,
      head: sd.swarm.head_seq,
      since: 0,
      channels: sd.swarm.channels ? structuredClone(sd.swarm.channels) : undefined,
      members: structuredClone(sd.swarm.members),
      messages: [...sd.messages],
    });

    return () => {
      sd.subs.delete(sub);
    };
  }
}
