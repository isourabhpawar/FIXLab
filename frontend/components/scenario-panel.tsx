"use client";

import { useCallback, useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { Download, Film, Play, RefreshCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Select } from "@/components/ui/select";
import { cn } from "@/lib/utils";
import { getScenario, getSession, replayScenario } from "@/lib/api";
import type { ReplayStatus, Scenario, ScenarioStep } from "@/types/fixlab";

const MSG_NAMES: Record<string, string> = {
  D: "NewOrderSingle",
  F: "OrderCancelRequest",
  G: "OrderCancelReplaceRequest",
  "8": "ExecutionReport",
  "9": "OrderCancelReject",
};

function sideName(v: string): string {
  if (v === "1") return "Buy";
  if (v === "2") return "Sell";
  return v;
}

function fmtOffset(atMs: number): string {
  const s = atMs / 1000;
  if (s < 60) return `T+${s.toFixed(1)}s`;
  return `T+${Math.floor(s / 60)}m${(s % 60).toFixed(0)}s`;
}

function describeStep(st: ScenarioStep): { label: string; detail: string } {
  const p = st.payload ?? {};
  switch (st.kind) {
    case "logon":
      return { label: "Logon", detail: "FIX session logged on" };
    case "logout":
      return { label: "Logout", detail: "FIX session logged out" };
    case "disconnect":
      return { label: "Disconnect", detail: p.reason ? `TCP dropped: ${p.reason}` : "TCP connection dropped" };
    case "inbound": {
      const f: Record<string, string> = p.fields ?? {};
      const name = MSG_NAMES[p.msgType] ?? `35=${p.msgType}`;
      if (p.msgType === "D") {
        return {
          label: name,
          detail: `${f["55"] ?? "?"} ${sideName(f["54"] ?? "")} ${f["38"] ?? "?"}${f["44"] ? ` @ ${f["44"]}` : ""} (${f["11"] ?? "?"})`,
        };
      }
      return { label: name, detail: `for ${f["41"] ?? "?"}` };
    }
    case "execution": {
      const bits: string[] = [];
      if (p.qty) bits.push(`${p.qty}${p.price ? ` @ ${p.price}` : ""}`);
      if (p.ordRejReason) bits.push(`reason ${p.ordRejReason}`);
      if (p.text) bits.push(`"${p.text}"`);
      return {
        label: String(p.action ?? "EXECUTION").replace(/_/g, " "),
        detail: `${p.clOrdId ?? ""}${bits.length ? ` — ${bits.join(", ")}` : ""}`,
      };
    }
    default:
      return { label: st.kind, detail: "" };
  }
}

const KIND_STYLE: Record<string, string> = {
  logon: "bg-emerald-500/15 text-emerald-400",
  inbound: "bg-sky-500/15 text-sky-400",
  execution: "bg-amber-500/15 text-amber-400",
  logout: "bg-zinc-500/15 text-zinc-400",
  disconnect: "bg-red-500/15 text-red-400",
};

// Scenario timeline + replay (phase 2.3, spec §48): the session's
// application-level story, exportable as JSON and replayable into a
// fresh session.
export function ScenarioPanel({ token }: { token: string }) {
  const router = useRouter();
  const [scenario, setScenario] = useState<Scenario | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [speed, setSpeed] = useState("1");
  const [replaying, setReplaying] = useState(false);
  const [replayStatus, setReplayStatus] = useState<ReplayStatus | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const sc = await getScenario(token);
      setScenario(sc);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load scenario");
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    void load();
  }, [load]);

  // Poll the session for replay progress while one is running here.
  useEffect(() => {
    if (!replayStatus?.inProgress) return;
    const id = setInterval(async () => {
      try {
        const info = await getSession(token);
        if (info.replay) setReplayStatus(info.replay);
      } catch {
        /* session may be gone; stop polling */
        clearInterval(id);
      }
    }, 1000);
    return () => clearInterval(id);
  }, [token, replayStatus?.inProgress]);

  // Pick up replay state when landing on a fresh replay target.
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const info = await getSession(token);
        if (!cancelled && info.replay && (info.replay.inProgress || info.replay.failed)) {
          setReplayStatus(info.replay);
        }
      } catch {
        /* ignore */
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [token]);

  const exportJson = useCallback(() => {
    if (!scenario) return;
    const blob = new Blob([JSON.stringify(scenario, null, 2)], {
      type: "application/json",
    });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `fixlab-scenario-${token.slice(7, 15)}.json`;
    a.click();
    URL.revokeObjectURL(url);
  }, [scenario, token]);

  const replay = useCallback(async () => {
    setReplaying(true);
    setError(null);
    try {
      const res = await replayScenario({
        sourceToken: token,
        speed: speed === "instant" ? 0 : Number(speed),
      });
      router.push(`/session/${res.sessionToken}`);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Replay failed");
      setReplaying(false);
    }
  }, [token, speed, router]);

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Film className="h-4 w-4" />
            Scenario
            {scenario && (
              <Badge variant="live">
                {scenario.steps.length} step{scenario.steps.length === 1 ? "" : "s"}
              </Badge>
            )}
            {scenario && (scenario.dropped ?? 0) > 0 && (
              <Badge variant="warn">{scenario.dropped} dropped (cap)</Badge>
            )}
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <p className="text-xs text-muted">
            Recording is automatic: every logon, inbound order and execution is
            captured (heartbeats and other admin traffic are not). Export the
            JSON, or replay the whole story into a fresh session — replayed
            steps are marked <code className="text-[11px]">replayed:true</code>{" "}
            and nothing is transmitted on the wire.
          </p>

          {replayStatus && (replayStatus.inProgress || replayStatus.failed || replayStatus.doneSteps > 0) && (
            <div
              className={cn(
                "rounded-md border px-3 py-2 text-xs",
                replayStatus.failed
                  ? "border-red-500/40 bg-red-500/10"
                  : "border-sky-500/40 bg-sky-500/10"
              )}
            >
              {replayStatus.inProgress
                ? `Replaying… ${replayStatus.doneSteps}/${replayStatus.totalSteps} steps`
                : replayStatus.failed
                  ? `Replay failed: ${replayStatus.error}`
                  : `Replay finished: ${replayStatus.doneSteps}/${replayStatus.totalSteps} steps applied`}
            </div>
          )}

          <div className="flex flex-wrap items-center gap-2">
            <Button variant="secondary" size="sm" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={cn("h-3.5 w-3.5", loading && "animate-spin")} />
              Refresh
            </Button>
            <Button variant="secondary" size="sm" onClick={exportJson} disabled={!scenario}>
              <Download className="h-3.5 w-3.5" />
              Export JSON
            </Button>
            <div className="ml-auto flex items-end gap-2">
              <Select
                id="replay-speed"
                label="Speed"
                className="w-28"
                value={speed}
                onChange={(e) => setSpeed(e.target.value)}
                options={[
                  { value: "instant", label: "Instant" },
                  { value: "0.5", label: "0.5×" },
                  { value: "1", label: "1×" },
                  { value: "2", label: "2×" },
                  { value: "4", label: "4×" },
                ]}
              />
              <Button size="sm" onClick={() => void replay()} disabled={replaying || !scenario || scenario.steps.length === 0}>
                <Play className="h-3.5 w-3.5" />
                {replaying ? "Replaying…" : "Replay into new session"}
              </Button>
            </div>
          </div>

          {error && <p className="text-xs text-red-400">{error}</p>}

          {loading ? (
            <p className="text-xs text-muted">Loading scenario…</p>
          ) : scenario && scenario.steps.length > 0 ? (
            <ol className="flex flex-col gap-1.5">
              {scenario.steps.map((st) => {
                const { label, detail } = describeStep(st);
                return (
                  <li
                    key={st.seq}
                    className="flex items-center gap-3 rounded-md border border-border/60 px-3 py-1.5"
                  >
                    <span className="w-16 shrink-0 font-mono text-[11px] text-muted">
                      {fmtOffset(st.atMs)}
                    </span>
                    <Badge className={cn("shrink-0", KIND_STYLE[st.kind] ?? "")}>
                      {st.kind}
                    </Badge>
                    <span className="truncate text-xs">
                      <span className="font-medium">{label}</span>
                      {detail && <span className="text-muted"> — {detail}</span>}
                    </span>
                  </li>
                );
              })}
            </ol>
          ) : (
            <p className="text-xs text-muted">
              No steps recorded yet — connect a FIX engine and trade, and the
              story will appear here.
            </p>
          )}
        </CardContent>
      </Card>
    </div>
  );
}
