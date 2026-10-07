// Order lifecycle state machine (spec §30, phase 6) — the reference
// behind the Order State Explorer. Pure logic, zero imports, so it runs
// under plain node (type-stripped) for the assertion script in
// order-state.test.ts.
//
// Terminal states (FILLED, REJECTED, CANCELED) have no outgoing
// transitions. REPLACED orders stay working, so they follow the same
// flows as NEW.

export type OrderState =
  | "NEW"
  | "PARTIALLY_FILLED"
  | "FILLED"
  | "REJECTED"
  | "PENDING_CANCEL"
  | "CANCELED"
  | "REPLACED";

export const STATES: OrderState[] = [
  "NEW",
  "PARTIALLY_FILLED",
  "FILLED",
  "REJECTED",
  "PENDING_CANCEL",
  "CANCELED",
  "REPLACED",
];

export const TERMINAL_STATES: OrderState[] = ["FILLED", "REJECTED", "CANCELED"];

const OUTGOING: Record<OrderState, OrderState[]> = {
  NEW: ["PARTIALLY_FILLED", "FILLED", "REJECTED", "PENDING_CANCEL", "REPLACED"],
  PARTIALLY_FILLED: ["PARTIALLY_FILLED", "FILLED", "PENDING_CANCEL"],
  REPLACED: ["PARTIALLY_FILLED", "FILLED", "REJECTED", "PENDING_CANCEL", "REPLACED"],
  PENDING_CANCEL: ["CANCELED", "NEW"],
  FILLED: [],
  REJECTED: [],
  CANCELED: [],
};

export function outgoing(from: OrderState): OrderState[] {
  return OUTGOING[from] ?? [];
}

export function isValidTransition(from: OrderState, to: OrderState): boolean {
  return outgoing(from).includes(to);
}

export interface TransitionInfo {
  valid: boolean;
  fix: string;
  why: string;
}

const FLOWS: Record<string, { fix: string; why: string }> = {
  "NEW->PARTIALLY_FILLED": {
    fix: "35=8 · 150=1 (Partial Fill) / 39=1 · 14=CumQty 151=LeavesQty",
    why: "First partial fill: part of the order traded, the rest stays working.",
  },
  "NEW->FILLED": {
    fix: "35=8 · 150=F (Trade) / 39=2 · 151=0",
    why: "Full fill: the whole OrderQty traded in one execution.",
  },
  "NEW->REJECTED": {
    fix: "35=8 · 150=8 (Rejected) / 39=8 · 103=OrdRejReason",
    why: "The venue refused the order (bad symbol, limit breach, kill switch, …).",
  },
  "NEW->PENDING_CANCEL": {
    fix: "inbound 35=F OrderCancelRequest accepted into the pending state",
    why: "A cancel was requested but not yet confirmed — fills can still arrive.",
  },
  "NEW->REPLACED": {
    fix: "35=G → 35=8 · 150=5 (Replace) / 39=5",
    why: "Replace accepted: qty/price updated, the order stays working with the same OrderID.",
  },
  "PARTIALLY_FILLED->PARTIALLY_FILLED": {
    fix: "35=8 · 150=1 / 39=1 · CumQty/LeavesQty/AvgPx updated",
    why: "Another partial fill against the remaining LeavesQty.",
  },
  "PARTIALLY_FILLED->FILLED": {
    fix: "35=8 · 150=F / 39=2 · 151=0",
    why: "Final fill for the remaining LeavesQty.",
  },
  "PARTIALLY_FILLED->PENDING_CANCEL": {
    fix: "inbound 35=F OrderCancelRequest",
    why: "Cancel requested while partially filled — the filled part is already done.",
  },
  "PENDING_CANCEL->CANCELED": {
    fix: "35=8 · 150=4 (Canceled) / 39=4",
    why: "Cancel confirmed: the working remainder is dead.",
  },
  "PENDING_CANCEL->NEW": {
    fix: "35=9 OrderCancelReject (102=CxlRejReason)",
    why: "Cancel rejected: too late or invalid — the order resumes its prior working state.",
  },
};

export function describeTransition(from: OrderState, to: OrderState): TransitionInfo {
  if (!STATES.includes(from) || !STATES.includes(to)) {
    return { valid: false, fix: "—", why: `Unknown state: ${from} → ${to}.` };
  }
  if (isValidTransition(from, to)) {
    // REPLACED orders stay working: they reuse NEW's flows with a note.
    const flow = FLOWS[`${from}->${to}`] ?? FLOWS[`NEW->${to}`];
    const note =
      from === "REPLACED"
        ? "A replaced order stays working, so it follows the same flows as NEW. "
        : "";
    return { valid: true, fix: flow.fix, why: note + flow.why };
  }
  if (TERMINAL_STATES.includes(from)) {
    return {
      valid: false,
      fix: "—",
      why: `${from} is a terminal state: the order lifecycle ends here, and no FIX message can move it to ${to}.`,
    };
  }
  return {
    valid: false,
    fix: "—",
    why: `No FIX flow moves an order from ${from} to ${to}. Terminal states are FILLED, REJECTED and CANCELED; everything else must follow the working-order flows.`,
  };
}
