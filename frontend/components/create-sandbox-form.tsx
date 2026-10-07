"use client";

import { useState } from "react";
import { useRouter } from "next/navigation";
import { Loader2 } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { createSession } from "@/lib/api";

// Sandbox creation form shared by / and /sandbox (spec §5).
// Phase 7 adds the "Use TLS" checkbox (ACCEPTOR + INITIATOR); TLS now
// terminates at the FixLab edge. FIX 4.2 / 5.0SP2 remain disabled pending
// later phases.
export function CreateSandboxForm() {
  const router = useRouter();
  const [role, setRole] = useState("ACCEPTOR");
  const [targetCompId, setTargetCompId] = useState("MYCLIENT");
  const [useTls, setUseTls] = useState(false);
  const [remoteHost, setRemoteHost] = useState("");
  const [remotePort, setRemotePort] = useState("");
  const [remoteCompId, setRemoteCompId] = useState("");
  const [localSenderCompId, setLocalSenderCompId] = useState("FIXLAB");
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const isInitiator = role === "INITIATOR";

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setPending(true);
    setError(null);
    try {
      const info = await createSession({
        fixVersion: "FIX.4.4",
        role,
        transport: "TCP",
        tls: useTls,
        auth: "NONE",
        targetCompId: targetCompId.trim() || "MYCLIENT",
        ...(isInitiator
          ? {
              remoteHost: remoteHost.trim(),
              remotePort: remotePort.trim(),
              remoteCompId: remoteCompId.trim().toUpperCase(),
              localSenderCompId: localSenderCompId.trim().toUpperCase() || "FIXLAB",
            }
          : {}),
      });
      router.push(`/session/${info.sessionToken}`);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to create sandbox");
      setPending(false);
    }
  };

  return (
    <Card className="w-full max-w-xl">
      <CardHeader>
        <CardTitle className="text-lg">Create Free FIX Sandbox</CardTitle>
        <p className="text-sm text-muted">
          Anonymous, ephemeral, destroyed automatically after 4 hours. No
          account, no install, no broker approval.
        </p>
      </CardHeader>
      <CardContent>
        <form onSubmit={submit} className="flex flex-col gap-4">
          <Select
            id="fix-version"
            label="FIX Version"
            defaultValue="FIX.4.4"
            options={[
              { value: "FIX.4.2", label: "FIX 4.2", disabled: true, note: "later phase" },
              { value: "FIX.4.4", label: "FIX 4.4" },
              { value: "FIX.5.0SP2", label: "FIX 5.0 SP2", disabled: true, note: "later phase" },
            ]}
          />
          <Select
            id="role"
            label="FixLab Role"
            value={role}
            onChange={(e: React.ChangeEvent<HTMLSelectElement>) => setRole(e.target.value)}
            options={[
              { value: "ACCEPTOR", label: "ACCEPTOR — FixLab listens, your engine connects" },
              { value: "INITIATOR", label: "INITIATOR — FixLab dials your acceptor" },
            ]}
          />
          <Select
            id="transport"
            label="Transport"
            defaultValue="TCP"
            options={[{ value: "TCP", label: "TCP" }]}
          />
          <div className="flex flex-col gap-1.5">
            <label
              htmlFor="use-tls"
              className="flex cursor-pointer items-center gap-2 text-sm text-white"
            >
              <input
                id="use-tls"
                type="checkbox"
                checked={useTls}
                onChange={(e) => setUseTls(e.target.checked)}
                className="h-4 w-4 accent-white"
              />
              Use TLS
            </label>
            <p className="text-xs text-muted">
              TLS 1.2+; terminates at the FixLab edge. Dev deployments use a
              self-signed cert — verify the SHA-256 fingerprint shown on the
              session page.
            </p>
          </div>

          {!isInitiator ? (
            <div className="flex flex-col gap-1.5">
              <label
                htmlFor="target-comp-id"
                className="text-xs font-medium uppercase tracking-wider text-muted"
              >
                Your SenderCompID
              </label>
              <Input
                id="target-comp-id"
                value={targetCompId}
                onChange={(e) => setTargetCompId(e.target.value.toUpperCase())}
                placeholder="MYCLIENT"
                maxLength={32}
              />
              <p className="text-xs text-muted">
                FixLab connects as <span className="font-mono text-white">FIXLAB</span> and
                expects this as the target CompID.
              </p>
            </div>
          ) : (
            <>
              <div className="grid grid-cols-2 gap-4">
                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor="remote-host"
                    className="text-xs font-medium uppercase tracking-wider text-muted"
                  >
                    Remote host
                  </label>
                  <Input
                    id="remote-host"
                    value={remoteHost}
                    onChange={(e) => setRemoteHost(e.target.value)}
                    placeholder="fix.acme.com"
                    maxLength={253}
                    required
                  />
                </div>
                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor="remote-port"
                    className="text-xs font-medium uppercase tracking-wider text-muted"
                  >
                    Remote port
                  </label>
                  <Input
                    id="remote-port"
                    value={remotePort}
                    onChange={(e) => setRemotePort(e.target.value)}
                    placeholder="9878"
                    maxLength={5}
                    inputMode="numeric"
                    required
                  />
                </div>
              </div>
              <div className="grid grid-cols-2 gap-4">
                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor="remote-comp-id"
                    className="text-xs font-medium uppercase tracking-wider text-muted"
                  >
                    Remote TargetCompID
                  </label>
                  <Input
                    id="remote-comp-id"
                    value={remoteCompId}
                    onChange={(e) => setRemoteCompId(e.target.value.toUpperCase())}
                    placeholder="THEIRFIX"
                    maxLength={32}
                    required
                  />
                </div>
                <div className="flex flex-col gap-1.5">
                  <label
                    htmlFor="local-sender-comp-id"
                    className="text-xs font-medium uppercase tracking-wider text-muted"
                  >
                    Our SenderCompID
                  </label>
                  <Input
                    id="local-sender-comp-id"
                    value={localSenderCompId}
                    onChange={(e) => setLocalSenderCompId(e.target.value.toUpperCase())}
                    placeholder="FIXLAB"
                    maxLength={32}
                  />
                </div>
              </div>
              <p className="text-xs text-muted">
                FixLab dials your acceptor with SSRF protection: private IPs,
                localhost, and cloud metadata endpoints are rejected, and the
                resolved IP is pinned for the session.
              </p>
            </>
          )}

          {error && (
            <p className="rounded-md border border-danger/30 bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          )}
          <Button type="submit" size="lg" disabled={pending}>
            {pending && <Loader2 className="h-4 w-4 animate-spin" />}
            {pending ? "Creating sandbox…" : "Create Free FIX Sandbox"}
          </Button>
        </form>
      </CardContent>
    </Card>
  );
}
