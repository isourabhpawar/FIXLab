import { TriangleAlert } from "lucide-react";

// The product-positioning banner (spec §3). Rendered on every page.
export function SimulationBanner() {
  return (
    <div className="flex items-center justify-center gap-2 border-b border-warn/30 bg-warn/10 px-4 py-2 text-center">
      <TriangleAlert className="h-4 w-4 shrink-0 text-warn" />
      <p className="text-xs font-semibold tracking-widest text-warn">
        SIMULATION ENVIRONMENT — NOT A LIVE TRADING VENUE
      </p>
    </div>
  );
}
