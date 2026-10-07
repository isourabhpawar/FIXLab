"use client";

import { use, useCallback, useEffect, useState } from "react";
import Link from "next/link";
import {
  ArrowLeft,
  BookOpenText,
  Dices,
  Film,
  MessagesSquare,
  ScrollText,
  SlidersHorizontal,
  Stethoscope,
  Trash2,
} from "lucide-react";
import { SimulationBanner } from "@/components/banner";
import { ConnectionCard, QuickFixConfigCard } from "@/components/connection-card";
import { DictionaryPanel } from "@/components/dictionary-panel";
import { InitiatorPanel } from "@/components/inject-order-panel";
import { MessageFeed } from "@/components/message-feed";
import { MessageInspector } from "@/components/message-inspector";
import { OrderBlotter } from "@/components/order-blotter";
import { KillSwitchBanner, RulesPanel } from "@/components/rules-panel";
import { ScenarioPanel } from "@/components/scenario-panel";
import { StochasticPanel } from "@/components/stochastic-panel";
import { StatusPill } from "@/components/status-pill";
import { TtlCountdown } from "@/components/ttl-countdown";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Separator } from "@/components/ui/separator";
import { cn } from "@/lib/utils";
import { deleteSession, getMessages, getSession, setKillSwitch } from "@/lib/api";
import { useFixLabSocket } from "@/lib/use-fixlab-socket";
import { useSessionStore } from "@/store/use-session-store";

type Tab = "messages" | "orders" | "rules" | "simulation" | "scenario" | "dictionary" | "diagnostics";

const TABS: { id: Tab; label: string; icon: React.ReactNode }[] = [
  { id: "messages", label: "Messages", icon: <MessagesSquare className="h-4 w-4" /> },
  { id: "orders", label: "Orders", icon: <ScrollText className="h-4 w-4" /> },
  { id: "rules", label: "Rules", icon: <SlidersHorizontal className="h-4 w-4" /> },
  { id: "simulation", label: "Simulation", icon: <Dices className="h-4 w-4" /> },
  { id: "scenario", label: "Scenario", icon: <Film className="h-4 w-4" /> },
  { id: "dictionary", label: "Dictionary", icon: <BookOpenText className="h-4 w-4" /> },
  { id: "diagnostics", label: "Diagnostics", icon: <Stethoscope className="h-4 w-4" /> },
];

// Live workspace (spec §21): header, left nav, connection panel, live
// FIX feed and inspector.
export default function SessionPage({ params }: { params: Promise<{ token: string }> }) {
  const { token } = use(params);
  const [tab, setTab] = useState<Tab>("messages");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [destroying, setDestroying] = useState(false);

  const info = useSessionStore((s) => s.info);
  const status = useSessionStore((s) => s.status);
  const expired = useSessionStore((s) => s.expired);
  const killSwitch = useSessionStore((s) => s.killSwitch);
  const setInfo = useSessionStore((s) => s.setInfo);
  const setKillSwitchState = useSessionStore((s) => s.setKillSwitch);
  const seedMessages = useSessionStore((s) => s.seedMessages);
  const reset = useSessionStore((s) => s.reset);
  const [toggling, setToggling] = useState(false);

  useFixLabSocket(token);

  const load = useCallback(async () => {
    try {
      const [sessionInfo, history] = await Promise.all([
        getSession(token),
        getMessages(token, 100),
      ]);
      setInfo(sessionInfo);
      seedMessages(history.messages);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Session not found");
    } finally {
      setLoading(false);
    }
  }, [token, setInfo, seedMessages]);

  useEffect(() => {
    reset();
    load();
    // Refresh counters every 15s; the live feed itself is WebSocket-driven.
    const t = setInterval(() => {
      getSession(token).then(setInfo).catch(() => {});
    }, 15000);
    return () => {
      clearInterval(t);
      reset();
    };
  }, [token, load, setInfo, reset]);

  const destroy = async () => {
    if (!confirm("Destroy this sandbox? The FIX port closes immediately.")) return;
    setDestroying(true);
    try {
      await deleteSession(token);
      window.location.href = "/";
    } catch (err) {
      setError(err instanceof Error ? err.message : "Destroy failed");
      setDestroying(false);
    }
  };

  const toggleKillSwitch = async () => {
    setToggling(true);
    try {
      const res = await setKillSwitch(token, !killSwitch);
      setKillSwitchState(res.enabled);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Kill switch toggle failed");
    } finally {
      setToggling(false);
    }
  };

  return (
    <div className="flex min-h-screen flex-col">
      <SimulationBanner />

      {/* Header */}
      <header className="border-b border-border">
        <div className="mx-auto flex max-w-7xl items-center gap-4 px-6 py-3">
          <Link href="/" className="text-muted hover:text-white" aria-label="Back home">
            <ArrowLeft className="h-5 w-5" />
          </Link>
          <span className="text-lg font-bold tracking-tight">
            Fix<span className="text-accent">Lab</span>
          </span>
          <StatusPill status={status} />
          <Button
            size="sm"
            variant={killSwitch ? "destructive" : "secondary"}
            onClick={toggleKillSwitch}
            disabled={toggling || loading}
            title="Kill switch: while on, every inbound NewOrderSingle is rejected immediately"
          >
            {killSwitch ? "🔴 Halted" : "Kill switch"}
          </Button>
          <div className="ml-auto flex items-center gap-4">
            {info && <TtlCountdown expiresAt={info.expiresAt} />}
            <Button size="sm" variant="destructive" onClick={destroy} disabled={destroying}>
              <Trash2 className="h-3.5 w-3.5" />
              {destroying ? "Destroying…" : "Destroy"}
            </Button>
          </div>
        </div>
      </header>

      <div className="mx-auto flex w-full max-w-7xl flex-1 gap-6 px-6 py-6">
        {/* Left nav */}
        <nav className="hidden w-48 shrink-0 flex-col gap-1 md:flex">
          {TABS.map((t) => (
            <button
              key={t.id}
              onClick={() => setTab(t.id)}
              className={cn(
                "flex items-center gap-2 rounded-md px-3 py-2 text-sm",
                tab === t.id ? "bg-panel text-white" : "text-muted hover:text-white"
              )}
            >
              {t.icon}
              {t.label}
            </button>
          ))}
        </nav>

        {/* Main panel */}
        <main className="flex min-w-0 flex-1 flex-col gap-4">
          {loading && <p className="text-sm text-muted">Loading session…</p>}
          {error && (
            <Card>
              <CardContent className="pt-4">
                <p className="text-sm text-danger">{error}</p>
                <p className="mt-1 text-xs text-muted">
                  The token may be wrong, or the session may have expired and
                  been destroyed.
                </p>
              </CardContent>
            </Card>
          )}

          {!loading && !error && info && (
            <>
              {killSwitch && <KillSwitchBanner />}
              {expired && (
                <Card className="border-danger/40">
                  <CardContent className="pt-4">
                    <p className="text-sm text-danger">
                      This session has expired and was destroyed. Create a new
                      sandbox to continue.
                    </p>
                  </CardContent>
                </Card>
              )}

              {tab === "messages" && (
                <>
                  {info.role === "INITIATOR" ? (
                    <InitiatorPanel info={info} token={token} />
                  ) : (
                    <div className="grid gap-4 lg:grid-cols-2">
                      <ConnectionCard info={info} />
                      <QuickFixConfigCard info={info} token={token} />
                    </div>
                  )}
                  <MessageFeed />
                  <MessageInspector />
                </>
              )}

              {tab === "orders" && <OrderBlotter token={token} />}
              {tab === "rules" && <RulesPanel token={token} />}
              {tab === "simulation" && <StochasticPanel token={token} />}
              {tab === "scenario" && <ScenarioPanel token={token} />}
              {tab === "dictionary" && <DictionaryPanel token={token} />}

              {tab === "diagnostics" && <DiagnosticsPanel />}
            </>
          )}
        </main>
      </div>
    </div>
  );
}

function DiagnosticsPanel() {
  const info = useSessionStore((s) => s.info);
  const transitions = useSessionStore((s) => s.transitions);
  const messages = useSessionStore((s) => s.messages);
  if (!info) return null;

  const rows: [string, string][] = [
    ["Role", info.role],
    ["BeginString", info.identifiers.beginString],
    ["FixLab SenderCompID", info.identifiers.senderCompId],
    ["Your TargetCompID", info.identifiers.targetCompId],
    ["FIX port", String(info.endpoint.port)],
    ["Transport", info.endpoint.tls ? "TLS" : "TCP"],
    ["Status", info.status],
    ["Application messages", String(info.appMessages)],
    ["Messages in history", String(info.messageCount)],
    ["Session token", `${info.sessionToken.slice(0, 14)}…`],
    ["Created", new Date(info.createdAt).toLocaleString()],
    ["Expires", new Date(info.expiresAt).toLocaleString()],
  ];

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle>Session diagnostics</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid grid-cols-1 gap-x-8 sm:grid-cols-2">
            {rows.map(([k, v]) => (
              <div key={k} className="flex items-baseline justify-between gap-4 border-b border-border/50 py-1.5">
                <dt className="text-xs uppercase tracking-wider text-muted">{k}</dt>
                <dd className="font-mono text-xs text-white">{v}</dd>
              </div>
            ))}
          </dl>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Status timeline</CardTitle>
        </CardHeader>
        <CardContent>
          {transitions.length === 0 && messages.length === 0 && (
            <p className="text-sm text-muted">
              Waiting for connection — point your FIX engine at the host/port
              above and log on.
            </p>
          )}
          <ul className="flex flex-col gap-1 font-mono text-xs">
            {transitions.map((t, i) => (
              <li key={i} className="flex gap-3 text-muted">
                <span>{new Date(t.at).toLocaleTimeString("en-GB", { hour12: false })}</span>
                <span className="text-white">{t.status.replaceAll("_", " ")}</span>
              </li>
            ))}
          </ul>
          <Separator className="my-3" />
          <p className="text-xs text-muted">
            {messages.length} messages streamed this session.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
