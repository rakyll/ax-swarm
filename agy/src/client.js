// @ts-check

export class SwarmClient {
  /**
   * @param {string} baseUrl
   * @param {string} [token]
   */
  constructor(baseUrl, token) {
    this.baseUrl = baseUrl.replace(/\/+$/, "");
    this.token = token;
  }

  /** @param {boolean} hasBody */
  headers(hasBody) {
    /** @type {Record<string, string>} */
    const h = {};
    if (hasBody) h["Content-Type"] = "application/json";
    if (this.token) h["Authorization"] = `Bearer ${this.token}`;
    return h;
  }

  /**
   * @param {string} method
   * @param {string} path
   * @param {unknown} [body]
   */
  async request(method, path, body) {
    const resp = await fetch(`${this.baseUrl}${path}`, {
      method,
      headers: this.headers(body !== undefined),
      body: body !== undefined ? JSON.stringify(body) : undefined,
    });
    if (!resp.ok) {
      throw new Error(`HTTP ${resp.status}: ${(await resp.text()).trim()}`);
    }
    return await resp.json();
  }

  /** @param {any} p */
  createSwarm(p) {
    return this.request("POST", "/v1/swarms", p);
  }

  /** @param {string} swarmId */
  getSwarm(swarmId) {
    return this.request("GET", `/v1/swarms/${encodeURIComponent(swarmId)}`);
  }

  /**
   * @param {string} swarmId
   * @param {string} title
   */
  setTitle(swarmId, title) {
    return this.request("PUT", `/v1/swarms/${encodeURIComponent(swarmId)}/title`, { title });
  }

  /** @param {any} p */
  addMember(p) {
    const { swarm_id, ...rest } = p;
    return this.request("POST", `/v1/swarms/${encodeURIComponent(swarm_id)}/members`, rest);
  }

  /**
   * @param {string} swarmId
   * @param {string} name
   */
  killMember(swarmId, name) {
    return this.request(
      "DELETE",
      `/v1/swarms/${encodeURIComponent(swarmId)}/members/${encodeURIComponent(name)}`,
    );
  }

  /** @param {any} p */
  postMessage(p) {
    const { swarm_id, ...rest } = p;
    return this.request("POST", `/v1/swarms/${encodeURIComponent(swarm_id)}/messages`, rest);
  }

  /**
   * @param {string} swarmId
   * @param {{ member?: string, since?: number, channel?: string }} [opts]
   */
  readBoard(swarmId, opts = {}) {
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

  /** @param {any} p */
  parkMember(p) {
    const { swarm_id, name, ...rest } = p;
    return this.request(
      "POST",
      `/v1/swarms/${encodeURIComponent(swarm_id)}/members/${encodeURIComponent(name)}/park`,
      rest,
    );
  }

  /**
   * @param {string} swarmId
   * @param {(snap: any) => void} onSnapshot
   * @param {AbortSignal} [signal]
   */
  async watch(swarmId, onSnapshot, signal) {
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
        let idx;
        while ((idx = buf.indexOf("\n")) >= 0) {
          const line = buf.slice(0, idx).trim();
          buf = buf.slice(idx + 1);
          if (line) onSnapshot(JSON.parse(line));
        }
      }
    } catch (err) {
      if (signal?.aborted) return;
      throw err;
    }
  }
}
