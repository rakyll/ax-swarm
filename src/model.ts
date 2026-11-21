export const Phase = {
  Queued: "QUEUED",
  Running: "RUNNING",
  Waiting: "WAITING",
  Stopped: "STOPPED",
  Killed: "KILLED",
} as const;
export type Phase = (typeof Phase)[keyof typeof Phase];

export const DesiredState = {
  Running: "RUNNING",
  Waiting: "WAITING",
  Killed: "KILLED",
} as const;
export type DesiredState = (typeof DesiredState)[keyof typeof DesiredState];

export const WakeMode = {
  Mentions: "mentions",
  Any: "any",
} as const;
export type WakeMode = (typeof WakeMode)[keyof typeof WakeMode];

export const OwnerKind = {
  Human: "HUMAN",
  Agent: "AGENT",
} as const;
export type OwnerKind = (typeof OwnerKind)[keyof typeof OwnerKind];

export const DEFAULT_CHANNEL = "main";

export interface Channel {
  name: string;
  topic?: string;
  created_by?: string;
  archived?: boolean;
}

export interface Member {
  name: string;
  task_name?: string;
  conversation_id: string;
  role: string;
  model?: string;
  runtime?: string;
  image?: string;
  workspace?: string;
  brief?: string;
  channels?: string[];
  wake_mode: WakeMode;
  wait_since: number;
  last_read_seq: number;
  phase: Phase;
  desired_state: DesiredState;
  node_id?: string;
  owner_kind?: OwnerKind;
  owner_label?: string;
  killed: boolean;
  parked_at?: string;
  updated_at: string;
}

export interface Message {
  seq: number;
  channel?: string;
  agent: string;
  text: string;
  mentions?: string[];
  time: string;
}

export interface Swarm {
  swarm_id: string;
  atespace: string;
  title: string;
  workspace_ref?: string;
  head_seq: number;
  channels?: Channel[];
  members: Record<string, Member>;
  created_at: string;
  updated_at: string;
}

// Wire format matching Antigravity's swarm_board remote_board contract.
export interface WatchSnapshot {
  version: number;
  swarm_id: string;
  title: string;
  head: number;
  since: number;
  channels?: Channel[];
  members: Record<string, Member>;
  messages: Message[];
}

const MENTION_RE = /(?:^|[^a-zA-Z0-9_-])@(#?[a-zA-Z0-9][a-zA-Z0-9_-]*)/g;

export function parseMentions(text: string): string[] {
  const seen = new Set<string>();
  const out: string[] = [];
  for (const match of text.matchAll(MENTION_RE)) {
    const target = match[1].toLowerCase();
    if (!seen.has(target)) {
      seen.add(target);
      out.push(target);
    }
  }
  return out;
}

function memberInChannel(m: Member, ch?: string): boolean {
  if (!ch || ch === DEFAULT_CHANNEL) return true;
  return (m.channels ?? []).some((c) => c.toLowerCase() === ch.toLowerCase());
}

export function memberMatchesMessage(m: Member, msg: Message): boolean {
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
