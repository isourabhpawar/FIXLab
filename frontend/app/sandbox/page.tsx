import Link from "next/link";
import { ArrowLeft } from "lucide-react";
import { SimulationBanner } from "@/components/banner";
import { CreateSandboxForm } from "@/components/create-sandbox-form";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

export default function SandboxPage() {
  return (
    <div className="flex min-h-screen flex-col">
      <SimulationBanner />
      <header className="border-b border-border">
        <div className="mx-auto flex max-w-6xl items-center gap-4 px-6 py-4">
          <Link href="/" className="text-muted hover:text-white" aria-label="Back home">
            <ArrowLeft className="h-5 w-5" />
          </Link>
          <span className="text-xl font-bold tracking-tight">
            Fix<span className="text-accent">Lab</span>
          </span>
        </div>
      </header>

      <main className="mx-auto flex w-full max-w-6xl flex-1 flex-col items-center gap-8 px-6 py-12 lg:flex-row lg:items-start lg:justify-center">
        <CreateSandboxForm />
        <Card className="w-full max-w-xl">
          <CardHeader>
            <CardTitle>What you get</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3 text-sm text-muted">
            <p>
              <strong className="text-white">Acceptor:</strong> a dedicated FIX 4.4
              acceptor on its own TCP port, reachable at the host and port shown
              after creation. Your engine logs on as the SenderCompID you choose;
              FixLab answers as <code className="font-mono text-white">FIXLAB</code>.
            </p>
            <p>
              <strong className="text-white">Initiator:</strong> FixLab dials{" "}
              <em>your</em> FIX acceptor and you inject NewOrderSingle / Cancel /
              Replace from the browser — with SSRF protection on every dial.
            </p>
            <p>
              Every message — logon, heartbeat, TestRequest, resend — streams
              to the live workspace over WebSocket within milliseconds, with
              full tag-level parsing from the embedded FIX dictionary.
            </p>
            <ul className="list-disc pl-5">
              <li>4-hour TTL, destroyed automatically</li>
              <li>250 application messages / 500 message history per session</li>
              <li>Sequence numbers, heartbeats and resends handled by QuickFIX/Go</li>
              <li>Deterministic rules, kill switch, order simulation included</li>
            </ul>
            <p className="font-mono text-xs">
              Equivalent API call:{" "}
              <code className="text-white">
                POST /api/v1/sessions {"{"}&quot;targetCompId&quot;: &quot;MYCLIENT&quot;{"}"}
              </code>
            </p>
          </CardContent>
        </Card>
      </main>
    </div>
  );
}
