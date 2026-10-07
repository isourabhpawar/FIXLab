"use client";

import { useEffect, useState } from "react";
import { Timer } from "lucide-react";
import { cn } from "@/lib/utils";

// TTL countdown to session expiry (spec §4: 4h default).
export function TtlCountdown({ expiresAt }: { expiresAt: string }) {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(t);
  }, []);

  const remaining = Math.max(0, Date.parse(expiresAt) - now);
  const h = Math.floor(remaining / 3_600_000);
  const m = Math.floor((remaining % 3_600_000) / 60_000);
  const s = Math.floor((remaining % 60_000) / 1000);
  const urgent = remaining < 10 * 60_000;

  const text = [h, m, s].map((n) => String(n).padStart(2, "0")).join(":");

  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 font-mono text-sm",
        urgent ? "text-danger" : "text-muted"
      )}
      title="Session time-to-live"
    >
      <Timer className="h-4 w-4" />
      TTL {text}
    </span>
  );
}
