"use client";

import { useEffect, useRef } from "react";
import { Pause, Play } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { useSessionStore } from "@/store/use-session-store";
import type { FeedMessage } from "@/types/fixlab";

function formatTime(ts: string): string {
  const d = new Date(ts);
  return d.toLocaleTimeString("en-GB", { hour12: false });
}

// Live FIX message feed (spec §21/§22). Messages stream in over the
// WebSocket within milliseconds of receipt; the feed auto-scrolls unless
// paused.
export function MessageFeed() {
  const messages = useSessionStore((s) => s.messages);
  const paused = useSessionStore((s) => s.paused);
  const setPaused = useSessionStore((s) => s.setPaused);
  const selectedId = useSessionStore((s) => s.selectedId);
  const select = useSessionStore((s) => s.select);
  const lastLatencyMs = useSessionStore((s) => s.lastLatencyMs);
  const wsConnected = useSessionStore((s) => s.wsConnected);
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (!paused && scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [messages, paused]);

  return (
    <Card className="flex min-h-0 flex-1 flex-col">
      <CardHeader className="flex flex-row items-center justify-between py-3">
        <CardTitle className="flex items-center gap-2">
          Live FIX Messages
          <Badge variant={wsConnected ? "live" : "warn"}>
            {wsConnected ? "STREAMING" : "CONNECTING…"}
          </Badge>
        </CardTitle>
        <div className="flex items-center gap-2">
          {lastLatencyMs !== null && (
            <span className="font-mono text-xs text-muted" title="Server receipt → browser render">
              {lastLatencyMs} ms
            </span>
          )}
          <Button
            size="sm"
            variant="secondary"
            onClick={() => setPaused(!paused)}
            title={paused ? "Resume auto-scroll" : "Pause auto-scroll"}
          >
            {paused ? <Play className="h-3.5 w-3.5" /> : <Pause className="h-3.5 w-3.5" />}
            {paused ? "Resume" : "Pause"}
          </Button>
        </div>
      </CardHeader>
      <CardContent className="min-h-0 flex-1 p-0">
        <div ref={scrollRef} className="feed-scroll h-[420px] overflow-y-auto px-2 pb-2">
          {messages.length === 0 && (
            <p className="px-2 py-8 text-center text-sm text-muted">
              No messages yet. Connect your FIX engine — logon, heartbeats and
              orders will stream here live.
            </p>
          )}
          {messages.map((m) => (
            <MessageRow
              key={m.id}
              message={m}
              selected={m.id === selectedId}
              onClick={() => select(m.id === selectedId ? null : m.id)}
            />
          ))}
        </div>
      </CardContent>
    </Card>
  );
}

function MessageRow({
  message: m,
  selected,
  onClick,
}: {
  message: FeedMessage;
  selected: boolean;
  onClick: () => void;
}) {
  const inbound = m.direction === "INBOUND";
  return (
    <button
      onClick={onClick}
      className={cn(
        "flex w-full items-center gap-2 rounded px-2 py-1.5 text-left font-mono text-xs hover:bg-panel",
        selected && "bg-panel ring-1 ring-accent/50"
      )}
    >
      <Badge variant={inbound ? "in" : "out"} className="w-11 shrink-0 justify-center">
        {inbound ? "IN" : "OUT"}
      </Badge>
      <Badge variant="default" className="w-14 shrink-0 justify-center">
        35={m.msgType}
      </Badge>
      <span className="shrink-0 text-white">{m.msgName || m.msgType}</span>
      <span className="truncate text-muted">
        {m.senderCompId}→{m.targetCompId} #{m.msgSeqNum}
      </span>
      <span className="ml-auto shrink-0 text-muted">{formatTime(m.timestamp)}</span>
    </button>
  );
}
