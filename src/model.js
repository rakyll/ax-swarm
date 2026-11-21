// @ts-check

export const Phase = Object.freeze({
  Queued: "QUEUED",
  Running: "RUNNING",
  Waiting: "WAITING",
  Stopped: "STOPPED",
  Killed: "KILLED",
});

export const DesiredState = Object.freeze({
  Running: "RUNNING",
  Waiting: "WAITING",
  Killed: "KILLED",
});

export const WakeMode = Object.freeze({
  Mentions: "mentions",
  Any: "any",
});

export const OwnerKind = Object.freeze({
  Human: "HUMAN",
  Agent: "AGENT",
});

export const DEFAULT_CHANNEL = "main";

const MENTION_RE = /(?:^|[^a-zA-Z0-9_-])@(#?[a-zA-Z0-9][a-zA-Z0-9_-]*)/g;

/**
 * Extracts unique lowercase @mentions (including @all and @#channel) at word boundaries.
 * @param {string} text
 * @returns {string[]}
 */
export function parseMentions(text) {
  const seen = new Set();
  /** @type {string[]} */
  const out = [];
  for (const match of text.matchAll(MENTION_RE)) {
    const target = match[1].toLowerCase();
    if (!seen.has(target)) {
      seen.add(target);
      out.push(target);
    }
  }
  return out;
}

/**
 * @param {{ channels?: string[] }} m
 * @param {string | undefined} ch
 */
function memberInChannel(m, ch) {
  if (!ch || ch === DEFAULT_CHANNEL) return true;
  return (m.channels ?? []).some((c) => c.toLowerCase() === ch.toLowerCase());
}

/**
 * Reports whether a message should wake the given member.
 * @param {{ name: string, killed: boolean, desired_state: string, wake_mode: string, channels?: string[] }} m
 * @param {{ agent: string, channel?: string, mentions?: string[] }} msg
 * @returns {boolean}
 */
export function memberMatchesMessage(m, msg) {
  if (m.killed || m.desired_state === DesiredState.Killed) return false;
  if (m.name.toLowerCase() === msg.agent.toLowerCase()) return false;

  const name = m.name.toLowerCase();
  for (const target of msg.mentions ?? []) {
    if (target === "all") return true;
    if (target === name) return true;
    if (name.startsWith(`${target}-`)) return true;
    if (target.startsWith("#") && memberInChannel(m, target.slice(1))) return true;
  }

  if (m.wake_mode === WakeMode.Any) {
    return memberInChannel(m, msg.channel);
  }
  return false;
}
