import { create } from "zustand";
import type {
  ConnectionStatus,
  FeedMessage,
  FixMessage,
  Order,
  SessionInfo,
} from "@/types/fixlab";

export interface StatusTransition {
  status: string;
  at: number;
}

interface SessionState {
  info: SessionInfo | null;
  messages: FeedMessage[];
  status: ConnectionStatus;
  transitions: StatusTransition[];
  paused: boolean;
  selectedId: string | null;
  wsConnected: boolean;
  lastLatencyMs: number | null;
  expired: boolean;
  orders: Record<string, Order>;
  killSwitch: boolean;

  setInfo: (info: SessionInfo) => void;
  seedMessages: (msgs: FixMessage[]) => void;
  addEvent: (type: string, payload: any, serverTs: string) => void;
  setStatus: (status: ConnectionStatus) => void;
  setPaused: (paused: boolean) => void;
  select: (id: string | null) => void;
  setWsConnected: (connected: boolean) => void;
  setOrders: (orders: Order[]) => void;
  upsertOrder: (order: Order) => void;
  setKillSwitch: (on: boolean) => void;
  reset: () => void;
}

const MAX_FEED = 500;

function toFeedMessage(m: FixMessage, serverTs: string): FeedMessage {
  const recvAt = Date.now();
  const eventAt = Date.parse(serverTs);
  return {
    ...m,
    id: `${m.direction}-${m.msgSeqNum}-${m.timestamp}`,
    latencyMs: Number.isFinite(eventAt) ? Math.max(0, recvAt - eventAt) : null,
  };
}

function pushUnique(list: FeedMessage[], m: FeedMessage): FeedMessage[] {
  if (list.some((x) => x.id === m.id)) return list;
  const next = [...list, m];
  return next.length > MAX_FEED ? next.slice(next.length - MAX_FEED) : next;
}

export const useSessionStore = create<SessionState>((set) => ({
  info: null,
  messages: [],
  status: "WAITING_FOR_CONNECTION",
  transitions: [],
  paused: false,
  selectedId: null,
  wsConnected: false,
  lastLatencyMs: null,
  expired: false,
  orders: {},
  killSwitch: false,

  setInfo: (info) =>
    set((s) => ({
      info,
      status: (info.status as ConnectionStatus) ?? s.status,
      killSwitch: info.killSwitch ?? s.killSwitch,
    })),

  seedMessages: (msgs) =>
    set((s) => {
      let list = s.messages;
      for (const m of msgs) list = pushUnique(list, toFeedMessage(m, m.timestamp));
      return { messages: list };
    }),

  addEvent: (type, payload, serverTs) =>
    set((s) => {
      // Order events update the blotter even when the message feed is
      // paused — the pause only freezes message rendering/auto-scroll.
      if (type === "ORDER_CREATED" || type === "ORDER_UPDATED") {
        const order = payload as Order;
        if (!order || !order.clOrdId) return s;
        return { orders: { ...s.orders, [order.clOrdId]: order } };
      }
      if (s.paused) return s;
      if (type === "FIX_MSG_IN" || type === "FIX_MSG_OUT") {
        const m = toFeedMessage(payload as FixMessage, serverTs);
        return {
          messages: pushUnique(s.messages, m),
          lastLatencyMs: m.latencyMs,
        };
      }
      if (type === "CONNECTION_STATUS") {
        const status = payload?.status as ConnectionStatus;
        if (!status || status === s.status) return s;
        return {
          status,
          transitions: [...s.transitions, { status, at: Date.now() }].slice(-50),
        };
      }
      if (type === "SESSION_EXPIRED") {
        return { expired: true, status: "EXPIRED" as ConnectionStatus };
      }
      if (type === "KILL_SWITCH") {
        const enabled = payload?.enabled;
        if (typeof enabled !== "boolean" || enabled === s.killSwitch) return s;
        return { killSwitch: enabled };
      }
      return s;
    }),

  setStatus: (status) =>
    set((s) =>
      status === s.status
        ? s
        : {
            status,
            transitions: [...s.transitions, { status, at: Date.now() }].slice(-50),
          }
    ),

  setPaused: (paused) => set({ paused }),
  select: (selectedId) => set({ selectedId }),
  setWsConnected: (wsConnected) => set({ wsConnected }),
  setOrders: (orders) =>
    set(() => ({
      orders: Object.fromEntries(orders.map((o) => [o.clOrdId, o])),
    })),
  upsertOrder: (order) =>
    set((s) => ({ orders: { ...s.orders, [order.clOrdId]: order } })),
  setKillSwitch: (killSwitch) => set({ killSwitch }),
  reset: () =>
    set({
      info: null,
      messages: [],
      status: "WAITING_FOR_CONNECTION",
      transitions: [],
      paused: false,
      selectedId: null,
      wsConnected: false,
      lastLatencyMs: null,
      expired: false,
      orders: {},
      killSwitch: false,
    }),
}));
