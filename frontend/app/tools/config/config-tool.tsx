"use client";

import { Suspense, useEffect, useState } from "react";
import { useSearchParams } from "next/navigation";
import { Check, Copy, Download } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { cn } from "@/lib/utils";
import { getSession } from "@/lib/api";
import {
  buildQuickFixJ,
  buildQuickFixN,
  defaultDictionary,
  type QuickFixConfigInput,
} from "@/lib/quickfix-config";

type Engine = "j" | "n";

const BEGIN_STRINGS = ["FIX.4.0", "FIX.4.2", "FIX.4.4", "FIX.5.0SP2"];

// The form, wrapped in Suspense by the page because it reads ?session=.
export function ConfigTool() {
  const params = useSearchParams();
  const [role, setRole] = useState<"initiator" | "acceptor">("initiator");
  const [beginString, setBeginString] = useState("FIX.4.4");
  const [senderCompId, setSenderCompId] = useState("MYCLIENT");
  const [targetCompId, setTargetCompId] = useState("FIXLAB");
  const [host, setHost] = useState("127.0.0.1");
  const [port, setPort] = useState("10000");
  const [heartBtInt, setHeartBtInt] = useState("30");
  const [dataDictionary, setDataDictionary] = useState("FIX44.xml");
  const [engine, setEngine] = useState<Engine>("j");
  const [copied, setCopied] = useState(false);
  const [prefilled, setPrefilled] = useState<string | null>(null);

  // Keep the dictionary in step with the FIX version.
  useEffect(() => {
    setDataDictionary(defaultDictionary(beginString));
  }, [beginString]);

  // Prefill from a live FixLab session (?session=<token>): the session
  // page links here with the token.
  useEffect(() => {
    const token = params.get("session");
    if (!token) return;
    getSession(token)
      .then((info) => {
        const bs = info.identifiers.beginString;
        setBeginString(bs);
        setDataDictionary(defaultDictionary(bs));
        setHeartBtInt("30");
        if (info.role === "ACCEPTOR") {
          // The user's engine initiates into FixLab's listener.
          setRole("initiator");
          setSenderCompId(info.identifiers.targetCompId);
          setTargetCompId(info.identifiers.senderCompId);
          setHost(info.endpoint.host);
          setPort(String(info.endpoint.port));
        } else {
          // Initiator session: FixLab dials the user's acceptor.
          setRole("acceptor");
          setSenderCompId(info.identifiers.targetCompId);
          setTargetCompId(info.identifiers.senderCompId);
          setHost(info.endpoint.host);
          setPort(String(info.endpoint.port));
        }
        setPrefilled(info.sessionToken.slice(0, 14) + "…");
      })
      .catch(() => {
        /* unknown/expired token: leave the manual form */
      });
  }, [params]);

  const input: QuickFixConfigInput = {
    role,
    beginString,
    senderCompId: senderCompId || "MYCLIENT",
    targetCompId: targetCompId || "FIXLAB",
    host: host || "127.0.0.1",
    port: parseInt(port, 10) || 0,
    heartBtInt: parseInt(heartBtInt, 10) || 30,
    dataDictionary: dataDictionary || defaultDictionary(beginString),
  };
  const text = engine === "j" ? buildQuickFixJ(input) : buildQuickFixN(input);

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(text);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* ignore */
    }
  };

  const download = () => {
    const blob = new Blob([text], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = engine === "j" ? "quickfixj.cfg" : "quickfixn.cfg";
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div className="flex flex-col gap-6">
      <div className="flex max-w-2xl flex-col gap-3">
        <h1 className="text-3xl font-bold tracking-tight">
          QuickFIX Config Generator
        </h1>
        <p className="text-muted">
          Generate a ready-to-paste{" "}
          <span className="font-mono">[SESSION]</span> config for QuickFIX/J
          or QuickFIX/n. Open this page from a live session and the
          connection details prefill automatically.
        </p>
        {prefilled && (
          <p className="text-xs text-accent">
            Prefilled from session <span className="font-mono">{prefilled}</span>
          </p>
        )}
      </div>

      <div className="grid gap-6 lg:grid-cols-5">
        <Card className="lg:col-span-2">
          <CardHeader>
            <CardTitle className="text-base">Session details</CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <Field label="Your engine role">
              <select
                value={role}
                onChange={(e) => setRole(e.target.value as "initiator" | "acceptor")}
                className={selectCls}
              >
                <option value="initiator">Initiator (connect to FixLab)</option>
                <option value="acceptor">Acceptor (FixLab connects to you)</option>
              </select>
            </Field>
            <Field label="BeginString">
              <select
                value={beginString}
                onChange={(e) => setBeginString(e.target.value)}
                className={selectCls}
              >
                {BEGIN_STRINGS.map((b) => (
                  <option key={b} value={b}>
                    {b}
                  </option>
                ))}
              </select>
            </Field>
            <div className="grid grid-cols-2 gap-4">
              <Field label="SenderCompID (you)">
                <input value={senderCompId} onChange={(e) => setSenderCompId(e.target.value)} className={inputCls} spellCheck={false} />
              </Field>
              <Field label="TargetCompID">
                <input value={targetCompId} onChange={(e) => setTargetCompId(e.target.value)} className={inputCls} spellCheck={false} />
              </Field>
            </div>
            {role === "initiator" && (
              <Field label="SocketConnectHost">
                <input value={host} onChange={(e) => setHost(e.target.value)} className={inputCls} spellCheck={false} />
              </Field>
            )}
            <div className="grid grid-cols-2 gap-4">
              <Field label={role === "initiator" ? "SocketConnectPort" : "SocketAcceptPort"}>
                <input value={port} onChange={(e) => setPort(e.target.value)} className={inputCls} spellCheck={false} inputMode="numeric" />
              </Field>
              <Field label="HeartBtInt">
                <input value={heartBtInt} onChange={(e) => setHeartBtInt(e.target.value)} className={inputCls} spellCheck={false} inputMode="numeric" />
              </Field>
            </div>
            <Field label="DataDictionary">
              <input value={dataDictionary} onChange={(e) => setDataDictionary(e.target.value)} className={inputCls} spellCheck={false} />
            </Field>
          </CardContent>
        </Card>

        <Card className="lg:col-span-3">
          <CardHeader className="flex flex-row items-center justify-between py-3">
            <div className="flex gap-1">
              {(["j", "n"] as Engine[]).map((e) => (
                <button
                  key={e}
                  onClick={() => setEngine(e)}
                  className={cn(
                    "rounded px-3 py-1 font-mono text-xs",
                    engine === e ? "bg-accent/15 text-accent" : "text-muted hover:text-white"
                  )}
                >
                  {e === "j" ? "QuickFIX/J" : "QuickFIX/n"}
                </button>
              ))}
            </div>
            <div className="flex gap-2">
              <Button size="sm" variant="secondary" onClick={copy}>
                {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                {copied ? "Copied" : "Copy"}
              </Button>
              <Button size="sm" variant="secondary" onClick={download}>
                <Download className="h-3.5 w-3.5" />
                {engine === "j" ? "quickfixj.cfg" : "quickfixn.cfg"}
              </Button>
            </div>
          </CardHeader>
          <CardContent>
            <pre className="overflow-x-auto rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
              {text}
            </pre>
            <p className="mt-3 text-xs text-muted">
              <Badge variant="warn" className="mr-2">
                SIMULATION
              </Badge>
              Point this at a FixLab sandbox only — never at a production
              venue.
            </p>
          </CardContent>
        </Card>
      </div>
    </div>
  );
}

const inputCls =
  "w-full rounded-md border border-border bg-background px-3 py-2 font-mono text-sm text-white placeholder:text-muted/50 focus:border-accent focus:outline-none";
const selectCls =
  "w-full rounded-md border border-border bg-background px-3 py-2 text-sm text-white focus:border-accent focus:outline-none";

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label className="flex flex-col gap-1.5">
      <span className="text-xs font-medium uppercase tracking-wider text-muted">{label}</span>
      {children}
    </label>
  );
}

// Suspense boundary required by Next.js for useSearchParams().
export function ConfigToolSuspense() {
  return (
    <Suspense>
      <ConfigTool />
    </Suspense>
  );
}
