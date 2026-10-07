import Link from "next/link";
import { Zap, Radio, Eye } from "lucide-react";
import { SimulationBanner } from "@/components/banner";
import { CreateSandboxForm } from "@/components/create-sandbox-form";

export default function Home() {
  return (
    <div className="flex min-h-screen flex-col">
      <SimulationBanner />
      <header className="border-b border-border">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-6 py-4">
          <span className="text-xl font-bold tracking-tight">
            Fix<span className="text-accent">Lab</span>
          </span>
          <nav className="flex gap-4 text-sm text-muted">
            <Link href="/tools" className="hover:text-white">
              Tools
            </Link>
            <Link href="/sandbox" className="hover:text-white">
              Create sandbox
            </Link>
          </nav>
        </div>
      </header>

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col items-center gap-12 px-6 py-12">
        <div className="flex max-w-2xl flex-col items-center gap-4 text-center">
          <h1 className="text-4xl font-bold tracking-tight sm:text-5xl">
            Connect your FIX engine to a live counterparty in{" "}
            <span className="text-accent">under 5 minutes</span>.
          </h1>
          <p className="text-lg text-muted">
            FixLab is an ephemeral FIX protocol sandbox. Create a session,
            point your engine at it, and watch real FIX messages flow — no
            account, no install, no broker test environment.
          </p>
        </div>

        <CreateSandboxForm />

        <div className="grid w-full gap-4 sm:grid-cols-3">
          <Step
            icon={<Zap className="h-5 w-5 text-accent" />}
            title="1. Create sandbox"
            text="Pick FIX 4.4, get a host, port and CompIDs instantly. Anonymous and tokenized."
          />
          <Step
            icon={<Radio className="h-5 w-5 text-info" />}
            title="2. Connect your engine"
            text="Point any FIX initiator at the sandbox. Logon, heartbeats and resends handled for you."
          />
          <Step
            icon={<Eye className="h-5 w-5 text-warn" />}
            title="3. Watch it live"
            text="Every message streams to your browser in milliseconds — raw, parsed and inspectable."
          />
        </div>
      </main>

      <footer className="border-t border-border py-6 text-center text-xs text-muted">
        <div className="mb-2 flex flex-wrap justify-center gap-x-4 gap-y-1">
          <Link href="/tools" className="hover:text-white">
            Tools
          </Link>
          <Link href="/tools/decoder" className="hover:text-white">
            Decoder
          </Link>
          <Link href="/tools/checksum" className="hover:text-white">
            Checksum
          </Link>
          <Link href="/tools/timestamp" className="hover:text-white">
            Timestamps
          </Link>
          <Link href="/tools/config" className="hover:text-white">
            Config generator
          </Link>
          <Link href="/tools/order-state" className="hover:text-white">
            Order states
          </Link>
        </div>
        FixLab — simulation environment, not a live trading venue.
      </footer>
    </div>
  );
}

function Step({ icon, title, text }: { icon: React.ReactNode; title: string; text: string }) {
  return (
    <div className="rounded-lg border border-border bg-surface p-5">
      <div className="mb-3">{icon}</div>
      <h3 className="mb-1 font-semibold">{title}</h3>
      <p className="text-sm text-muted">{text}</p>
    </div>
  );
}
