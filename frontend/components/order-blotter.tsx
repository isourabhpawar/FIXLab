"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { RefreshCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { executeOrder, getOrders } from "@/lib/api";
import { useSessionStore } from "@/store/use-session-store";
import type { ExecuteAction, Order } from "@/types/fixlab";

const STATUS_VARIANT: Record<Order["status"], "live" | "warn" | "in" | "danger" | "default" | "out"> = {
  NEW: "live",
  PARTIALLY_FILLED: "warn",
  FILLED: "in",
  REJECTED: "danger",
  PENDING_CANCEL: "warn",
  CANCELED: "default",
  REPLACED: "out",
};

const WORKING = new Set(["NEW", "PARTIALLY_FILLED", "REPLACED"]);

function sideLabel(side: string): string {
  if (side === "1") return "Buy";
  if (side === "2") return "Sell";
  return side || "—";
}

function fmtNum(v: number): string {
  return Number.isFinite(v) ? String(v) : "—";
}

function fmtTime(ts: string): string {
  const d = new Date(ts);
  return Number.isNaN(d.getTime())
    ? "—"
    : d.toLocaleTimeString("en-GB", { hour12: false });
}

interface DialogState {
  order: Order;
  action: ExecuteAction;
}

// Order blotter (spec §23/§24): live table of orders with per-order
// execution controls. Rows update over the WebSocket (ORDER_CREATED /
// ORDER_UPDATED); a 5s poll covers any dropped events.
export function OrderBlotter({ token }: { token: string }) {
  const orders = useSessionStore((s) => s.orders);
  const setOrders = useSessionStore((s) => s.setOrders);
  const upsertOrder = useSessionStore((s) => s.upsertOrder);
  const [dialog, setDialog] = useState<DialogState | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [refreshing, setRefreshing] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const res = await getOrders(token);
      setOrders(res.orders);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load orders");
    }
  }, [token, setOrders]);

  useEffect(() => {
    setRefreshing(true);
    refresh().finally(() => setRefreshing(false));
    const t = setInterval(refresh, 5000);
    return () => clearInterval(t);
  }, [token, refresh]);

  const list = useMemo(
    () => Object.values(orders).sort((a, b) => +new Date(b.createdAt) - +new Date(a.createdAt)),
    [orders]
  );

  const run = useCallback(
    async (order: Order, action: ExecuteAction, body: Record<string, string | number>) => {
      setBusy(true);
      setError(null);
      try {
        const res = await executeOrder(token, order.clOrdId, { action, ...body });
        upsertOrder(res.order);
        setDialog(null);
      } catch (err) {
        setError(err instanceof Error ? err.message : "Execution failed");
      } finally {
        setBusy(false);
      }
    },
    [token, upsertOrder]
  );

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between py-3">
        <CardTitle className="flex items-center gap-2">
          Order Blotter
          <Badge variant="default">{list.length}</Badge>
        </CardTitle>
        <Button
          size="sm"
          variant="secondary"
          onClick={() => {
            setRefreshing(true);
            refresh().finally(() => setRefreshing(false));
          }}
          disabled={refreshing}
          title="Refresh blotter"
        >
          <RefreshCw className={cn("h-3.5 w-3.5", refreshing && "animate-spin")} />
        </Button>
      </CardHeader>
      <CardContent>
        {error && (
          <p className="mb-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
            {error}
          </p>
        )}
        {list.length === 0 ? (
          <p className="py-6 text-center text-sm text-muted">
            No orders yet — send a NewOrderSingle (35=D) from your FIX engine
            and it will appear here.
          </p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left font-mono text-xs">
              <thead>
                <tr className="border-b border-border text-[10px] uppercase tracking-wider text-muted">
                  {["ClOrdID", "Symbol", "Side", "Qty", "Price", "Type", "CumQty", "Leaves", "AvgPx", "Status", "Time", "Actions"].map(
                    (h) => (
                      <th key={h} className="px-2 py-2 font-medium">
                        {h}
                      </th>
                    )
                  )}
                </tr>
              </thead>
              <tbody>
                {list.map((o) => (
                  <OrderRow
                    key={o.clOrdId}
                    order={o}
                    onAction={(action) => setDialog({ order: o, action })}
                    onResolve={(action) =>
                      run(o, action, {})
                    }
                    busy={busy}
                  />
                ))}
              </tbody>
            </table>
          </div>
        )}
        {dialog && (
          <ExecDialog
            state={dialog}
            busy={busy}
            onClose={() => setDialog(null)}
            onSubmit={(body) => run(dialog.order, dialog.action, body)}
          />
        )}
      </CardContent>
    </Card>
  );
}

function OrderRow({
  order: o,
  onAction,
  onResolve,
  busy,
}: {
  order: Order;
  onAction: (action: ExecuteAction) => void;
  onResolve: (action: ExecuteAction) => void;
  busy: boolean;
}) {
  // Browser executions only apply to INBOUND orders (acceptor mode);
  // OUTBOUND orders are driven by the remote counterparty (phase 5).
  const working = WORKING.has(o.status) && o.direction !== "OUTBOUND";
  return (
    <>
      <tr className="border-b border-border/50 align-top hover:bg-panel/40">
        <td className="px-2 py-2 text-white">
          {o.clOrdId}
          {o.direction === "OUTBOUND" && (
            <Badge variant="out" className="ml-1.5 font-mono text-[10px]" title="Injected outbound into the remote counterparty (phase 5)">
              OUT
            </Badge>
          )}
        </td>
        <td className="px-2 py-2">{o.symbol || "—"}</td>
        <td className="px-2 py-2">{sideLabel(o.side)}</td>
        <td className="px-2 py-2">{fmtNum(o.orderQty)}</td>
        <td className="px-2 py-2">{o.price > 0 ? fmtNum(o.price) : "—"}</td>
        <td className="px-2 py-2">{o.ordType || "—"}</td>
        <td className="px-2 py-2">{fmtNum(o.cumQty)}</td>
        <td className="px-2 py-2">{fmtNum(o.leavesQty)}</td>
        <td className="px-2 py-2">{o.cumQty > 0 ? fmtNum(o.avgPx) : "—"}</td>
        <td className="px-2 py-2">
          <Badge variant={STATUS_VARIANT[o.status] ?? "default"}>
            {o.status.replaceAll("_", " ")}
          </Badge>
        </td>
        <td className="px-2 py-2 text-muted">{fmtTime(o.updatedAt)}</td>
        <td className="px-2 py-2">
          {working && (
            <div className="flex flex-wrap gap-1">
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => onAction("FILL")}>
                Fill
              </Button>
              <Button size="sm" variant="secondary" disabled={busy} onClick={() => onAction("PARTIAL_FILL")}>
                Partial Fill
              </Button>
              <Button size="sm" variant="destructive" disabled={busy} onClick={() => onAction("REJECT")}>
                Reject
              </Button>
            </div>
          )}
          {!working && o.status !== "PENDING_CANCEL" && (
            <span className="text-muted">—</span>
          )}
        </td>
      </tr>
      {o.status === "PENDING_CANCEL" && (
        <tr className="border-b border-border/50 bg-warn/5">
          <td colSpan={12} className="px-2 py-2">
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <Badge variant="warn">CANCEL REQUESTED</Badge>
              <span className="text-muted">
                35=F {o.cancelReqClOrdId} — accept to cancel the order, or reject
                the cancel request.
              </span>
              <span className="ml-auto flex gap-1">
                <Button size="sm" variant="secondary" disabled={busy} onClick={() => onResolve("CANCEL_ACCEPT")}>
                  Accept Cancel
                </Button>
                <Button size="sm" variant="destructive" disabled={busy} onClick={() => onAction("CANCEL_REJECT")}>
                  Reject Cancel
                </Button>
              </span>
            </div>
          </td>
        </tr>
      )}
      {o.pendingReplace && (
        <tr className="border-b border-border/50 bg-info/5">
          <td colSpan={12} className="px-2 py-2">
            <div className="flex flex-wrap items-center gap-2 text-xs">
              <Badge variant="out">REPLACE REQUESTED</Badge>
              <span className="text-muted">
                35=G {o.pendingReplace.clOrdId} → Qty {fmtNum(o.pendingReplace.orderQty)}
                {o.pendingReplace.price > 0 && <> @ {fmtNum(o.pendingReplace.price)}</>} —
                accept to apply, or reject.
              </span>
              <span className="ml-auto flex gap-1">
                <Button size="sm" variant="secondary" disabled={busy} onClick={() => onResolve("REPLACE_ACCEPT")}>
                  Accept Replace
                </Button>
                <Button size="sm" variant="destructive" disabled={busy} onClick={() => onAction("REPLACE_REJECT")}>
                  Reject Replace
                </Button>
              </span>
            </div>
          </td>
        </tr>
      )}
    </>
  );
}

const ORD_REJ_REASONS = [
  ["", "Select reason…"],
  ["2", "2 — Exchange closed"],
  ["3", "3 — Order exceeds limit"],
  ["5", "5 — Unknown order"],
  ["6", "6 — Duplicate order"],
  ["0", "0 — Broker option"],
];

const CXL_REJ_REASONS = [
  ["", "Select reason…"],
  ["0", "0 — Too late to cancel"],
  ["1", "1 — Unknown order"],
  ["2", "2 — Broker option"],
  ["6", "6 — Duplicate ClOrdID"],
];

function ExecDialog({
  state,
  busy,
  onClose,
  onSubmit,
}: {
  state: DialogState;
  busy: boolean;
  onClose: () => void;
  onSubmit: (body: Record<string, string | number>) => void;
}) {
  const { order: o, action } = state;
  const [qty, setQty] = useState(
    action === "FILL" ? String(o.leavesQty) : ""
  );
  const [price, setPrice] = useState(o.price > 0 ? String(o.price) : "");
  const [reason, setReason] = useState("");
  const [text, setText] = useState("");
  const [localError, setLocalError] = useState<string | null>(null);

  const title =
    action === "FILL"
      ? "Full Fill"
      : action === "PARTIAL_FILL"
        ? "Partial Fill"
        : action === "REJECT"
          ? "Reject Order"
          : action === "CANCEL_REJECT"
            ? "Reject Cancel Request"
            : "Reject Replace Request";

  const submit = () => {
    setLocalError(null);
    const q = parseFloat(qty);
    const p = parseFloat(price);
    switch (action) {
      case "FILL":
        if (!(q > 0)) return setLocalError("Quantity must be > 0.");
        if (!(p > 0)) return setLocalError("Price must be > 0.");
        if (q !== o.leavesQty)
          return setLocalError(
            `Full fill quantity must equal the remaining ${o.leavesQty} (use Partial Fill for less).`
          );
        return onSubmit({ qty: q, price: p });
      case "PARTIAL_FILL":
        if (!(q > 0)) return setLocalError("LastQty must be > 0.");
        if (!(p > 0)) return setLocalError("LastPx must be > 0.");
        if (q > o.leavesQty)
          return setLocalError(`LastQty cannot exceed leaves quantity ${o.leavesQty}.`);
        return onSubmit({ qty: q, price: p });
      case "REJECT":
        if (!reason && !text.trim())
          return setLocalError("A reject reason or text is required.");
        return onSubmit({ ordRejReason: reason, text: text.trim() });
      case "CANCEL_REJECT":
      case "REPLACE_REJECT":
        if (!reason && !text.trim())
          return setLocalError("A reject reason or text is required.");
        return onSubmit({ cxlRejReason: reason, text: text.trim() });
    }
  };

  const reasons = action === "REJECT" ? ORD_REJ_REASONS : CXL_REJ_REASONS;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-md rounded-lg border border-border bg-surface p-5"
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className="text-sm font-semibold text-white">
          {title}{" "}
          <span className="font-mono text-muted">· {o.clOrdId}</span>
        </h3>
        <p className="mt-1 font-mono text-xs text-muted">
          {o.symbol} {sideLabel(o.side)} {o.orderQty}
          {o.price > 0 && <> @ {o.price}</>} · leaves {o.leavesQty}
        </p>

        <div className="mt-4 flex flex-col gap-3">
          {(action === "FILL" || action === "PARTIAL_FILL") && (
            <>
              <label className="flex flex-col gap-1 text-xs text-muted">
                {action === "FILL" ? "Quantity (must equal leaves)" : "LastQty"}
                <Input
                  type="number"
                  min="0"
                  step="any"
                  value={qty}
                  onChange={(e) => setQty(e.target.value)}
                  placeholder={action === "FILL" ? String(o.leavesQty) : "e.g. 400"}
                />
              </label>
              <label className="flex flex-col gap-1 text-xs text-muted">
                {action === "FILL" ? "Price" : "LastPx"}
                <Input
                  type="number"
                  min="0"
                  step="any"
                  value={price}
                  onChange={(e) => setPrice(e.target.value)}
                  placeholder="e.g. 310.50"
                />
              </label>
            </>
          )}
          {(action === "REJECT" || action === "CANCEL_REJECT" || action === "REPLACE_REJECT") && (
            <>
              <label className="flex flex-col gap-1 text-xs text-muted">
                Reason code
                <select
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  className="h-10 rounded-md border border-border bg-panel px-3 text-sm text-white"
                >
                  {reasons.map(([v, label]) => (
                    <option key={v} value={v}>
                      {label}
                    </option>
                  ))}
                </select>
              </label>
              <label className="flex flex-col gap-1 text-xs text-muted">
                Text (tag 58)
                <Input
                  value={text}
                  onChange={(e) => setText(e.target.value)}
                  placeholder="Human-readable reason"
                />
              </label>
            </>
          )}
        </div>

        {localError && (
          <p className="mt-3 rounded-md border border-danger/40 bg-danger/10 px-3 py-2 text-xs text-danger">
            {localError}
          </p>
        )}

        <div className="mt-4 flex justify-end gap-2">
          <Button size="sm" variant="secondary" onClick={onClose} disabled={busy}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant={action === "REJECT" ? "destructive" : "default"}
            onClick={submit}
            disabled={busy}
          >
            {busy ? "Sending…" : "Send"}
          </Button>
        </div>
      </div>
    </div>
  );
}
