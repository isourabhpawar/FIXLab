import Link from "next/link";
import { SimulationBanner } from "@/components/banner";

// Shared chrome for the free developer tools (spec §36, phase 6):
// no account needed, no session needed.
export default function ToolsLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <div className="flex min-h-screen flex-col">
      <SimulationBanner />
      <header className="border-b border-border">
        <div className="mx-auto flex max-w-6xl items-center justify-between px-6 py-4">
          <Link href="/" className="text-xl font-bold tracking-tight">
            Fix<span className="text-accent">Lab</span>
          </Link>
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

      <main className="mx-auto w-full max-w-6xl flex-1 px-6 py-10">
        {children}
      </main>

      <footer className="border-t border-border py-6 text-center text-xs text-muted">
        FixLab — simulation environment, not a live trading venue.
      </footer>
    </div>
  );
}
