import type { Metadata } from "next";
import { OrderStateExplorer } from "./explorer";

export const metadata: Metadata = {
  title: "Order State Explorer — FIX Order Lifecycle — FixLab",
  description:
    "Interactive reference for the FIX order lifecycle: which order state transitions are valid (NEW → PARTIALLY_FILLED → FILLED), which FIX messages drive them (ExecutionReport 35=8, OrderCancelRequest 35=F), and which transitions are impossible.",
};

// Order State Explorer (spec §30, phase 6): interactive FIX order
// lifecycle reference. Also serves as an SEO page — the copy below
// explains the lifecycle in plain language.
export default function OrderStatePage() {
  return (
    <div className="flex flex-col gap-8">
      <div className="flex max-w-3xl flex-col gap-3">
        <h1 className="text-3xl font-bold tracking-tight">Order State Explorer</h1>
        <p className="text-muted">
          Every FIX order moves through a lifecycle of states —{" "}
          <span className="font-mono">NEW</span>,{" "}
          <span className="font-mono">PARTIALLY_FILLED</span>,{" "}
          <span className="font-mono">FILLED</span>,{" "}
          <span className="font-mono">REJECTED</span>,{" "}
          <span className="font-mono">PENDING_CANCEL</span>,{" "}
          <span className="font-mono">CANCELED</span>,{" "}
          <span className="font-mono">REPLACED</span> — driven by{" "}
          <span className="font-mono">ExecutionReport</span> (35=8) messages
          from the venue and cancel/replace requests from your engine. Pick
          a <em>from</em> state and a <em>to</em> state to check whether the
          transition is valid and see the exact FIX messages behind it.
        </p>
      </div>

      <OrderStateExplorer />

      <div className="flex max-w-3xl flex-col gap-4 text-sm leading-relaxed text-muted">
        <h2 className="text-xl font-semibold text-white">
          The FIX order lifecycle, explained
        </h2>
        <p>
          A <span className="font-mono">NewOrderSingle</span> (35=D) starts
          life as <span className="font-mono">NEW</span> once the venue
          acknowledges it (35=8, ExecType=0). From there it can be partially
          filled (ExecType=1), fully filled (ExecType=F), or rejected
          outright (ExecType=8). <span className="font-mono">35=F</span>{" "}
          (OrderCancelRequest) moves a working order to{" "}
          <span className="font-mono">PENDING_CANCEL</span> — a limbo state
          where fills can still arrive — until the venue confirms with
          ExecType=4 (<span className="font-mono">CANCELED</span>) or refuses
          with a 35=9 OrderCancelReject, which drops the order back to its
          prior working state. A <span className="font-mono">35=G</span>{" "}
          (OrderCancelReplaceRequest) accepted by the venue produces{" "}
          <span className="font-mono">REPLACED</span> (ExecType=5): the order
          keeps its OrderID and stays working, so it follows the same flows
          as a new order.
        </p>
        <p>
          Three states are <strong className="text-white">terminal</strong>:{" "}
          <span className="font-mono">FILLED</span>,{" "}
          <span className="font-mono">REJECTED</span> and{" "}
          <span className="font-mono">CANCELED</span>. Nothing leaves them —
          a cancel request against a filled order is the classic invalid
          transition (<span className="font-mono">FILLED → PENDING_CANCEL</span>),
          and a robust OMS must reject it client-side rather than sending a
          35=F the venue will refuse. Try it above.
        </p>
      </div>
    </div>
  );
}
