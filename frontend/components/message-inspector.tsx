"use client";

import { useState } from "react";
import { Check, Copy } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { useSessionStore } from "@/store/use-session-store";
import { FieldsTable } from "@/components/fields-table";

type View = "SUMMARY" | "FIELDS" | "RAW" | "JSON";

const VIEWS: View[] = ["SUMMARY", "FIELDS", "RAW", "JSON"];

// Message inspector (spec §22): SUMMARY / FIELDS / RAW / JSON views of
// the selected FIX message, with copy-to-clipboard for RAW.
export function MessageInspector() {
  const messages = useSessionStore((s) => s.messages);
  const selectedId = useSessionStore((s) => s.selectedId);
  const [view, setView] = useState<View>("SUMMARY");
  const [copied, setCopied] = useState(false);

  const message = messages.find((m) => m.id === selectedId) ?? null;

  const copyRaw = async () => {
    if (!message) return;
    try {
      await navigator.clipboard.writeText(message.rawFix.replaceAll("|", "\x01"));
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* ignore */
    }
  };

  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between py-3">
        <CardTitle>Message Inspector</CardTitle>
        <div className="flex gap-1">
          {VIEWS.map((v) => (
            <button
              key={v}
              onClick={() => setView(v)}
              className={cn(
                "rounded px-2 py-1 font-mono text-xs",
                view === v ? "bg-accent/15 text-accent" : "text-muted hover:text-white"
              )}
            >
              {v}
            </button>
          ))}
        </div>
      </CardHeader>
      <CardContent>
        {!message && (
          <p className="py-6 text-center text-sm text-muted">
            Select a message in the feed to inspect it.
          </p>
        )}
        {message && view === "SUMMARY" && <SummaryView messageId={message.id} />}
        {message && view === "FIELDS" && <FieldsView messageId={message.id} />}
        {message && view === "RAW" && (
          <div>
            <div className="mb-2 flex justify-end">
              <Button size="sm" variant="secondary" onClick={copyRaw}>
                {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                {copied ? "Copied" : "Copy RAW"}
              </Button>
            </div>
            <pre className="whitespace-pre-wrap break-all rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
              {message.rawFix}
            </pre>
          </div>
        )}
        {message && view === "JSON" && (
          <pre className="max-h-96 overflow-auto rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
            {JSON.stringify(message, null, 2)}
          </pre>
        )}
      </CardContent>
    </Card>
  );
}

function SummaryView({ messageId }: { messageId: string }) {
  const message = useSessionStore((s) => s.messages.find((m) => m.id === messageId))!;
  const rows: [string, string][] = [
    ["MsgType", `35=${message.msgType} (${message.msgName || "unknown"})`],
    ["Direction", message.direction],
    ["MsgSeqNum", String(message.msgSeqNum)],
    ["SenderCompID", message.senderCompId],
    ["TargetCompID", message.targetCompId],
    ["Timestamp", new Date(message.timestamp).toISOString()],
    ["Fields", String(message.fields.length)],
  ];
  if (message.latencyMs !== null) rows.push(["Server→browser", `${message.latencyMs} ms`]);
  return (
    <dl className="grid grid-cols-1 gap-x-6 gap-y-2 sm:grid-cols-2">
      {rows.map(([k, v]) => (
        <div key={k} className="flex items-baseline justify-between gap-4 border-b border-border/50 py-1.5">
          <dt className="text-xs uppercase tracking-wider text-muted">{k}</dt>
          <dd className="font-mono text-xs text-white">{v}</dd>
        </div>
      ))}
    </dl>
  );
}

function FieldsView({ messageId }: { messageId: string }) {
  const message = useSessionStore((s) => s.messages.find((m) => m.id === messageId))!;
  return (
    <div>
      <FieldsTable fields={message.fields} />
      <div className="mt-2 flex gap-2">
        <Badge variant={message.direction === "INBOUND" ? "in" : "out"}>{message.direction}</Badge>
        <Badge variant="default">35={message.msgType}</Badge>
      </div>
    </div>
  );
}
