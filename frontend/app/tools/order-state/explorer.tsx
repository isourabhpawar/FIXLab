"use client";

import { useState } from "react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import {
  STATES,
  TERMINAL_STATES,
  describeTransition,
  outgoing,
  type OrderState,
} from "@/lib/order-state";

export function OrderStateExplorer() {
  const [from, setFrom] = useState<OrderState>("NEW");
  const [to, setTo] = useState<OrderState>("PARTIALLY_FILLED");
  const info = describeTransition(from, to);

  return (
    <div className="flex flex-col gap-6">
      <Card>
        <CardHeader>
          <CardTitle className="text-base">Try a transition</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-5">
          <div className="grid gap-5 sm:grid-cols-2">
            <div>
              <p className="mb-2 text-xs uppercase tracking-wider text-muted">From</p>
              <div className="flex flex-wrap gap-2">
                {STATES.map((s) => (
                  <StateChip key={s} state={s} selected={from === s} onClick={() => setFrom(s)} />
                ))}
              </div>
            </div>
            <div>
              <p className="mb-2 text-xs uppercase tracking-wider text-muted">To</p>
              <div className="flex flex-wrap gap-2">
                {STATES.map((s) => (
                  <StateChip key={s} state={s} selected={to === s} onClick={() => setTo(s)} />
                ))}
              </div>
            </div>
          </div>

          <div
            className={cn(
              "rounded-md border p-4",
              info.valid ? "border-accent/40 bg-accent/5" : "border-danger/40 bg-danger/5"
            )}
          >
            <div className="mb-2 flex flex-wrap items-center gap-2">
              <Badge variant={info.valid ? "in" : "danger"}>
                {info.valid ? "VALID TRANSITION" : "INVALID TRANSITION"}
              </Badge>
              <code className="font-mono text-sm text-white">
                {from} → {to}
              </code>
            </div>
            <p className="font-mono text-xs text-info">{info.fix}</p>
            <p className="mt-1 text-sm text-muted">{info.why}</p>
          </div>

          <div>
            <Button
              variant="secondary"
              size="sm"
              onClick={() => {
                setFrom("FILLED");
                setTo("PENDING_CANCEL");
              }}
            >
              Try an invalid one: FILLED → PENDING_CANCEL
            </Button>
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-base">All valid flows</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-4 sm:grid-cols-2">
          {STATES.map((s) => (
            <div key={s} className="rounded-md border border-border/60 p-3">
              <p className="mb-2 flex items-center gap-2 font-mono text-xs text-white">
                {s}
                {TERMINAL_STATES.includes(s) && (
                  <Badge variant="default">terminal</Badge>
                )}
              </p>
              {outgoing(s).length === 0 ? (
                <p className="text-xs text-muted">No outgoing transitions.</p>
              ) : (
                <ul className="flex flex-col gap-1">
                  {outgoing(s).map((t) => (
                    <li key={t} className="font-mono text-xs text-muted">
                      → <span className="text-info">{t}</span>
                    </li>
                  ))}
                </ul>
              )}
            </div>
          ))}
        </CardContent>
      </Card>
    </div>
  );
}

function StateChip({
  state,
  selected,
  onClick,
}: {
  state: OrderState;
  selected: boolean;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      className={cn(
        "rounded-md border px-2.5 py-1.5 font-mono text-xs transition-colors",
        selected
          ? "border-accent/60 bg-accent/15 text-accent"
          : "border-border bg-background text-muted hover:border-muted hover:text-white",
        TERMINAL_STATES.includes(state) && !selected && "border-dashed"
      )}
      title={TERMINAL_STATES.includes(state) ? "terminal state" : "working state"}
    >
      {state}
    </button>
  );
}
