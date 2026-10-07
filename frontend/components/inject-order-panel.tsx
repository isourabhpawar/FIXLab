"use client";

import { useState } from "react";
import { Loader2, Send } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Badge } from "@/components/ui/badge";
import { sendOrder } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { InjectMsgType, SessionInfo } from "@/types/fixlab";

// Field definitions per message type, mirroring the backend's required
// sets (session/inject.go). Tag -> {label, placeholder, required}.
const FIELDS: Record<InjectMsgType, { tag: string; label: string; placeholder: string; required: boolean }[]> = {
  D: [
    { tag: "11", label: "ClOrdID", placeholder: "ORD-1", required: true },
    { tag: "55", label: "Symbol", placeholder: "TEST", required: true },
    { tag: "54", label: "Side (1=Buy 2=Sell)", placeholder: "1", required: true },
    { tag: "38", label: "OrderQty", placeholder: "100", required: true },
    { tag: "40", label: "OrdType (1=Mkt 2=Lmt)", placeholder: "2", required: true },
    { tag: "44", label: "Price", placeholder: "50.25", required: false },
  ],
  F: [
    { tag: "11", label: "ClOrdID (new)", placeholder: "CX-1", required: true },
    { tag: "41", label: "OrigClOrdID", placeholder: "ORD-1", required: true },
    { tag: "55", label: "Symbol", placeholder: "TEST", required: true },
    { tag: "54", label: "Side", placeholder: "1", required: true },
    { tag: "38", label: "OrderQty", placeholder: "100", required: true },
  ],
  G: [
    { tag: "11", label: "ClOrdID (new)", placeholder: "RP-1", required: true },
    { tag: "41", label: "OrigClOrdID", placeholder: "ORD-1", required: true },
    { tag: "55", label: "Symbol", placeholder: "TEST", required: true },
    { tag: "54", label: "Side", placeholder: "1", required: true },
    { tag: "38", label: "OrderQty (new)", placeholder: "200", required: true },
    { tag: "40", label: "OrdType", placeholder: "2", required: true },
    { tag: "44", label: "Price (new)", placeholder: "51", required: false },
  ],
};

const TABS: { id: InjectMsgType; label: string; hint: string }[] = [
  { id: "D", label: "NewOrderSingle", hint: "35=D" },
  { id: "F", label: "Cancel", hint: "35=F" },
  { id: "G", label: "Replace", hint: "35=G" },
];

const CHAIN = ["CONNECTING", "TCP_CONNECTED", "LOGON_SENT", "LOGON_ACCEPTED"];

// Remote endpoint card + status chain + order injection (phase 5,
// spec §8). Injected orders appear on the blotter with direction
// OUTBOUND; the remote's ExecutionReports drive their status.
export function InitiatorPanel({ info, token }: { info: SessionInfo; token: string }) {
  const [tab, setTab] = useState<InjectMsgType>("D");
  const [values, setValues] = useState<Record<string, string>>({});
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [lastSent, setLastSent] = useState<string | null>(null);

  const connected = info.status === "LOGON_ACCEPTED";

  const set = (tag: string, v: string) =>
    setValues((prev) => ({ ...prev, [`${tab}:${tag}`]: v }));
  const get = (tag: string) => values[`${tab}:${tag}`] ?? "";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setPending(true);
    setError(null);
    setLastSent(null);
    try {
      const fields: Record<string, string> = {};
      for (const f of FIELDS[tab]) {
        const v = get(f.tag).trim();
        if (v) fields[f.tag] = v;
        else if (f.required) throw new Error(`Field ${f.tag} (${f.label}) is required`);
      }
      const res = await sendOrder(token, { msgType: tab, fields });
      setLastSent(`${tab === "D" ? "NewOrderSingle" : tab === "F" ? "Cancel" : "Replace"} sent — ${res.order.clOrdId} (${res.order.status})`);
      setValues({});
    } catch (err) {
      setError(err instanceof Error ? err.message : "Injection failed");
    } finally {
      setPending(false);
    }
  };

  const chainIdx = CHAIN.indexOf(info.status);
  const failed = info.status === "CONNECTION_FAILED";

  return (
    <div className="flex flex-col gap-4">
      <div className="grid gap-4 lg:grid-cols-2">
        <Card>
          <CardHeader>
            <CardTitle>Remote counterparty</CardTitle>
          </CardHeader>
          <CardContent className="grid grid-cols-2 gap-x-6 gap-y-3">
            <Field label="Host" value={info.endpoint.host} />
            <Field label="Port" value={String(info.endpoint.port)} />
            <Field label="Remote TargetCompID" value={info.identifiers.targetCompId} />
            <Field label="Our SenderCompID" value={info.identifiers.senderCompId} />
            {info.remoteIp && <Field label="Pinned dial IP" value={info.remoteIp} note="SSRF-validated" />}
            <Field label="Transport" value="TCP" />
          </CardContent>
        </Card>
        <Card>
          <CardHeader>
            <CardTitle>Connection chain</CardTitle>
          </CardHeader>
          <CardContent>
            <ol className="flex flex-col gap-1.5">
              {CHAIN.map((s, i) => {
                const reached = !failed && chainIdx >= 0 && i <= chainIdx;
                const current = !failed && CHAIN[chainIdx] === s;
                return (
                  <li key={s} className="flex items-center gap-2 font-mono text-xs">
                    <span
                      className={cn(
                        "h-2 w-2 rounded-full",
                        failed ? "bg-muted" : reached ? "bg-accent" : "bg-muted/40"
                      )}
                    />
                    <span className={current ? "text-white" : "text-muted"}>
                      {s.replaceAll("_", " ")}
                      {current && " ←"}
                    </span>
                  </li>
                );
              })}
              {failed && (
                <li className="flex items-center gap-2 font-mono text-xs">
                  <span className="h-2 w-2 rounded-full bg-danger" />
                  <span className="text-danger">CONNECTION FAILED</span>
                </li>
              )}
            </ol>
            {!connected && !failed && (
              <p className="mt-3 text-xs text-muted">
                Injection is available once the session reaches LOGON ACCEPTED.
              </p>
            )}
          </CardContent>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            Inject order
            <Badge variant={connected ? "live" : "warn"}>
              {connected ? "ready" : info.status.replaceAll("_", " ")}
            </Badge>
          </CardTitle>
        </CardHeader>
        <CardContent>
          <div className="mb-4 flex gap-1">
            {TABS.map((t) => (
              <button
                key={t.id}
                onClick={() => setTab(t.id)}
                className={cn(
                  "rounded-md px-3 py-1.5 font-mono text-xs",
                  tab === t.id ? "bg-panel text-white" : "text-muted hover:text-white"
                )}
                title={t.hint}
              >
                {t.label}
              </button>
            ))}
          </div>
          <form onSubmit={submit} className="flex flex-col gap-3">
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-3">
              {FIELDS[tab].map((f) => (
                <div key={f.tag} className="flex flex-col gap-1.5">
                  <label
                    htmlFor={`inj-${tab}-${f.tag}`}
                    className="text-xs font-medium uppercase tracking-wider text-muted"
                  >
                    <span className="font-mono text-white">{f.tag}</span> {f.label}
                    {f.required && <span className="text-danger"> *</span>}
                  </label>
                  <Input
                    id={`inj-${tab}-${f.tag}`}
                    value={get(f.tag)}
                    onChange={(e) => set(f.tag, e.target.value)}
                    placeholder={f.placeholder}
                  />
                </div>
              ))}
            </div>
            {error && (
              <p className="rounded-md border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
                {error}
              </p>
            )}
            {lastSent && (
              <p className="rounded-md border border-accent/30 bg-accent/10 px-3 py-2 text-sm text-accent">
                {lastSent}
              </p>
            )}
            <div>
              <Button type="submit" disabled={pending || !connected}>
                {pending ? <Loader2 className="h-4 w-4 animate-spin" /> : <Send className="h-4 w-4" />}
                {pending ? "Sending…" : `Send ${TABS.find((t) => t.id === tab)?.hint}`}
              </Button>
            </div>
            {!connected && (
              <p className="text-xs text-muted">
                The session must be connected before orders can be injected.
              </p>
            )}
          </form>
        </CardContent>
      </Card>
    </div>
  );
}

function Field({ label, value, note }: { label: string; value: string; note?: string }) {
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs font-medium uppercase tracking-wider text-muted">
        {label}
        {note && <span className="normal-case text-muted/60"> · {note}</span>}
      </span>
      <code className="font-mono text-sm text-white">{value}</code>
    </div>
  );
}
