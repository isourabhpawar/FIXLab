"use client";

import { useState } from "react";
import Link from "next/link";
import { Check, Copy } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import type { SessionInfo } from "@/types/fixlab";

// Connection details card (spec §7): what the developer pastes into
// their FIX engine config.
export function ConnectionCard({ info }: { info: SessionInfo }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Connection</CardTitle>
      </CardHeader>
      <CardContent className="grid grid-cols-2 gap-x-6 gap-y-3 sm:grid-cols-3">
        <Field label="Host" value={info.endpoint.host} copyable />
        <Field label="Port" value={String(info.endpoint.port)} copyable />
        <Field label="BeginString" value={info.identifiers.beginString} copyable />
        <Field label="SenderCompID" value={info.identifiers.senderCompId} copyable note="FixLab" />
        <Field label="TargetCompID" value={info.identifiers.targetCompId} copyable note="you" />
        <Field label="Transport" value={info.endpoint.tls ? "TLS" : "TCP"} />
        {info.endpoint.tls && info.certFingerprint && (
          <Field
            label="Cert SHA-256"
            value={info.certFingerprint}
            copyable
          />
        )}
      </CardContent>
      {info.endpoint.tls && info.certSelfSigned && (
        <p className="px-6 pb-4 text-xs text-amber-300">
          Self-signed dev certificate — verify this fingerprint out of band
          before trusting.
        </p>
      )}
    </Card>
  );
}

function Field({
  label,
  value,
  copyable,
  note,
}: {
  label: string;
  value: string;
  copyable?: boolean;
  note?: string;
}) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* clipboard unavailable */
    }
  };
  return (
    <div className="flex flex-col gap-1">
      <span className="text-xs font-medium uppercase tracking-wider text-muted">
        {label}
        {note && <span className="normal-case text-muted/60"> · {note}</span>}
      </span>
      <span className="flex items-center gap-2">
        <code className="font-mono text-sm text-white">{value}</code>
        {copyable && (
          <button
            onClick={copy}
            className="text-muted hover:text-white"
            title={`Copy ${label}`}
            aria-label={`Copy ${label}`}
          >
            {copied ? <Check className="h-3.5 w-3.5 text-accent" /> : <Copy className="h-3.5 w-3.5" />}
          </button>
        )}
      </span>
    </div>
  );
}

// QuickFIX/J + QuickFIX/n session config snippet. The full generator
// lives at /tools/config; this card links there with the session token
// so the connection details prefill (phase 6).
export function QuickFixConfigCard({ info, token }: { info: SessionInfo; token?: string }) {
  const [copied, setCopied] = useState(false);
  const cfg = `[SESSION]
BeginString=${info.identifiers.beginString}
SenderCompID=${info.identifiers.targetCompId}
TargetCompID=${info.identifiers.senderCompId}
ConnectionType=initiator
SocketConnectHost=${info.endpoint.host}
SocketConnectPort=${info.endpoint.port}
HeartBtInt=30
DataDictionary=FIX44.xml${info.endpoint.tls ? "\nSocketUseSSL=Y" : ""}`;
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(cfg);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* ignore */
    }
  };
  return (
    <Card>
      <CardHeader className="flex flex-row items-center justify-between">
        <CardTitle>QuickFIX Session Config</CardTitle>
        <div className="flex gap-2">
          <Button size="sm" variant="secondary" onClick={copy}>
            {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
            {copied ? "Copied" : "Copy"}
          </Button>
          {token && (
            <Link href={`/tools/config?session=${encodeURIComponent(token)}`}>
              <Button size="sm" variant="default">
                Config generator
              </Button>
            </Link>
          )}
        </div>
      </CardHeader>
      <CardContent>
        <pre className="overflow-x-auto rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
          {cfg}
        </pre>
      </CardContent>
    </Card>
  );
}
