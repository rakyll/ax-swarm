// @ts-check
import { DesiredState, OwnerKind, Phase } from "./model.js";

export class Reconciler {
  /**
   * @param {import("./store.js").FileStore} store
   * @param {any} ax
   * @param {{ pollIntervalMs?: number, suspendGraceMs?: number, swarmApiUrl: string, defaultImage?: string }} cfg
   */
  constructor(store, ax, cfg) {
    this.store = store;
    this.ax = ax;
    this.pollIntervalMs = cfg.pollIntervalMs ?? 2000;
    this.suspendGraceMs = cfg.suspendGraceMs ?? 15000;
    this.swarmApiUrl = cfg.swarmApiUrl;
    this.defaultImage = cfg.defaultImage;
    /** @type {ReturnType<typeof setInterval> | undefined} */
    this.timer = undefined;
  }

  start() {
    if (this.timer) return;
    void this.reconcileOnce().catch((e) => console.error("reconcile error:", e));
    this.timer = setInterval(() => {
      void this.reconcileOnce().catch((e) => console.error("reconcile error:", e));
    }, this.pollIntervalMs);
  }

  stop() {
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
  }

  async reconcileOnce() {
    for (const sw of this.store.listSwarms()) {
      try {
        await this.reconcileSwarm(sw);
      } catch (err) {
        console.error(`reconcile swarm ${sw.swarm_id}:`, err);
      }
    }
  }

  /** @param {any} sw */
  async reconcileSwarm(sw) {
    const tasks = await this.ax.listTasks(sw.atespace);
    /** @type {Map<string, string>} */
    const byName = new Map(tasks.map((/** @type {any} */ t) => [t.name, t.state]));
    const now = Date.now();

    for (const m of Object.values(sw.members)) {
      if (m.role === "coordinator" && m.owner_kind === OwnerKind.Human) continue;
      const taskName = m.task_name || m.name;
      let state = byName.get(taskName);

      if (m.desired_state === DesiredState.Killed) {
        if (state !== undefined) {
          await this.ax.deleteTask(sw.atespace, taskName);
        }
        this.store.updateMemberPhase(sw.swarm_id, m.name, Phase.Killed);
      } else if (m.desired_state === DesiredState.Running) {
        if (state === undefined) {
          await this.ax.createTask({
            name: taskName,
            atespace: sw.atespace,
            workspace: m.workspace,
            image: m.image || this.defaultImage,
            goal: m.brief,
            commands: ["ax-swarm-worker"],
            env: {
              SWARM_API_URL: this.swarmApiUrl,
              SWARM_ID: sw.swarm_id,
              SWARM_MEMBER: m.name,
              SWARM_RUNTIME: m.runtime || "",
            },
            debug: true,
          });
          state = "Suspended";
        }
        if (state !== "Working") {
          await this.ax.resumeTask(sw.atespace, taskName);
        }
        this.store.updateMemberPhase(sw.swarm_id, m.name, Phase.Running);
      } else if (m.desired_state === DesiredState.Waiting) {
        if (state === "Working") {
          const parkedMs = m.parked_at ? Date.parse(m.parked_at) : 0;
          if (!parkedMs || now - parkedMs >= this.suspendGraceMs) {
            await this.ax.suspendTask(sw.atespace, taskName);
          }
        }
        this.store.updateMemberPhase(sw.swarm_id, m.name, Phase.Waiting);
      }
    }
  }
}
