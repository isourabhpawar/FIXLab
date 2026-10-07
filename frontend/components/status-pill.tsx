"use client";

import { Badge } from "@/components/ui/badge";
import { cn } from "@/lib/utils";
import type { ConnectionStatus } from "@/types/fixlab";

const DOT: Record<string, string> = {
  WAITING_FOR_CONNECTION: "bg-warn",
  TCP_CONNECTED: "bg-info",
  LOGON_RECEIVED: "bg-info",
  CONNECTED: "bg-accent",
  DISCONNECTED: "bg-muted",
  EXPIRED: "bg-danger",
  DESTROYED: "bg-danger",
  // Initiator chain (phase 5, spec §8).
  CONNECTING: "bg-warn",
  LOGON_SENT: "bg-info",
  LOGON_ACCEPTED: "bg-accent",
  CONNECTION_FAILED: "bg-danger",
};

const LABEL: Record<string, string> = {
  WAITING_FOR_CONNECTION: "WAITING FOR CONNECTION",
  TCP_CONNECTED: "TCP CONNECTED",
  LOGON_RECEIVED: "FIX LOGON RECEIVED",
  CONNECTED: "CONNECTED",
  DISCONNECTED: "DISCONNECTED",
  EXPIRED: "EXPIRED",
  DESTROYED: "DESTROYED",
  // Initiator chain (phase 5, spec §8).
  CONNECTING: "CONNECTING",
  LOGON_SENT: "LOGON SENT",
  LOGON_ACCEPTED: "CONNECTED",
  CONNECTION_FAILED: "CONNECTION FAILED",
};

export function StatusPill({ status }: { status: ConnectionStatus | string }) {
  return (
    <Badge variant="default" className="gap-2 px-3 py-1 text-xs">
      <span
        className={cn(
          "h-2 w-2 rounded-full",
          DOT[status] ?? "bg-muted",
          (status === "CONNECTED" || status === "LOGON_ACCEPTED") && "animate-pulse"
        )}
      />
      {LABEL[status] ?? status}
    </Badge>
  );
}
