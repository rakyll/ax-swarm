import type { Member, Message, Swarm, WatchSnapshot } from "./model.ts";
import type {
  AddMemberParams,
  CreateSwarmParams,
  ParkMemberParams,
  ParkResult,
  PostMessageParams,
} from "./store.ts";

export class SwarmClient {
  private readonly baseUrl: string;
  private readonly token?: string;

  constructor(baseUrl: string, token?: string) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.token = token;
  }

  private headers(hasBody: boolean): Record<string, string> {
    const h: Record<string, string> = {};
    if (hasBody) h["Content-Type"] = "application/json";
    if (this.token) h["Authorization"] = `Bearer ${this.token}`;
    return h;
  }

  private async request<T>(method: string, path: string, body?: unknown): Promise<T> {
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers: this.headers(body !== undefined),
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (!resp.ok) {
      throw new Error(`HTTP ${resp.status}: ${(await resp.text()).trim()}`);
    }
    return (await resp.json()) as T;
  }

  createSwarm(p: CreateSwarmParams): Promise<Swarm> {
    return this.request("POST", "/v1/swarms", p);
  }

  getSwarm(swarmId: string): Promise<Swarm> {
    return this.request("GET", `/v1/swarms/${encodeURIComponent(swarmId)}`);
  }

  setTitle(swarmId: string, title: string): Promise<Swarm> {
    return this.request("PUT", `/v1/swarms/${encodeURIComponent(swarmId)}/title`, { title });
  }

  addMember(p: AddMemberParams): Promise<{ member: Member; token: string; brief_message?: Message }> {
    const { swarm_id, ...rest } = p;
    return this.request("POST", `/v1/swarms/${encodeURIComponent(swarm_id)}/members`, rest);
  }

  killMember(swarmId: string, name: string): Promise<Member> {
    return this.request(
      "DELETE",
      `/v1/swarms/${encodeURIComponent(swarmId)}/members/${encodeURIComponent(name)}`,
    );
  }

  postMessage(p: PostMessageParams): Promise<{ message: Message; woken: string[] }> {
    const { swarm_id, ...rest } = p;
    return this.request("POST", `/v1/swarms/${encodeURIComponent(swarm_id)}/messages`, rest);
  }

  readBoard(
    swarmId: string,
    opts: { member?: string; since?: number; channel?: string } = {},
  ): Promise<{ head: number; messages: Message[] }> {
    const q = new URLSearchParams();
    if (opts.since) q.set("since", String(opts.since));
    if (opts.member) q.set("member", opts.member);
    if (opts.channel) q.set("channel", opts.channel);
    const qs = q.toString();
    return this.request(
      "GET",
      `/v1/swarms/${encodeURIComponent(swarmId)}/messages${qs ? `?${qs}` : ""}`,
    );
  }

  parkMember(p: ParkMemberParams): Promise<ParkResult> {
    const { swarm_id, name, ...rest } = p;
    return this.request(
      "POST",
      `/v1/swarms/${encodeURIComponent(swarm_id)}/members/${encodeURIComponent(name)}/park`,
      rest,
    );
  }

  async watch(
    swarmId: string,
    onSnapshot: (snap: WatchSnapshot) => void,
    signal?: AbortSignal,
  ): Promise<void> {
    const resp = await fetch(`${this.baseUrl}/v1/swarms/${encodeURIComponent(swarmId)}/watch`, {
      headers: this.headers(false),
      signal,
    });
    if (!resp.ok || !resp.body) {
      throw new Error(`HTTP ${resp.status}: ${(await resp.text()).trim()}`);
    }
    const reader = resp.body.getReader();
    const decoder = new TextDecoder();
    let buf = "";
    try {
      while (true) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx: number;
        while ((idx = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, idx).trim();
          buf = buf.slice(idx + 1);
          if (line) onSnapshot(JSON.parse(line) as WatchSnapshot);
        }
      }
    } catch (err) {
      if (signal?.aborted) return;
      throw err;
    }
  }
}
