"use client";

import { useState } from "react";
import { Check, Copy, Play } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { FieldsTable } from "@/components/fields-table";
import { decodeFix } from "@/lib/api";
import { cn } from "@/lib/utils";
import type { DecodeResult } from "@/types/fixlab";

// Known-valid FIX 4.4 NewOrderSingle (9=103, 10=247) for the sample button.
const SAMPLE =
  "8=FIX.4.4|9=103|35=D|49=TESTBUY1|56=FIXLAB|34=12|52=20260107-05:30:00.000|11=ORD123|55=MSFT|54=1|38=100|40=2|44=310.50|10=247|";

type View = "SUMMARY" | "FIELDS" | "RAW" | "JSON";
const VIEWS: View[] = ["SUMMARY", "FIELDS", "RAW", "JSON"];

export default function DecoderPage() {
  const [input, setInput] = useState("");
  const [beginString, setBeginString] = useState("");
  const [result, setResult] = useState<DecodeResult | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [view, setView] = useState<View>("SUMMARY");
  const [copied, setCopied] = useState(false);

  const decode = async (raw: string) => {
    setLoading(true);
    setError(null);
    setResult(null);
    try {
      const res = await decodeFix(raw, beginString || undefined);
      setResult(res);
      setView("SUMMARY");
    } catch (e) {
      setError(e instanceof Error ? e.message : "Decode failed");
    } finally {
      setLoading(false);
    }
  };

  const copyRaw = async () => {
    if (!result) return;
    try {
      await navigator.clipboard.writeText(
        result.fields.map((f) => `${f.tag}=${f.value}`).join("\x01") + "\x01"
      );
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* ignore */
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <div className="flex max-w-2xl flex-col gap-3">
        <h1 className="text-3xl font-bold tracking-tight">FIX Decoder</h1>
        <p className="text-muted">
          Paste a raw FIX message — <span className="font-mono">SOH</span>,{" "}
          <span className="font-mono">|</span> or{" "}
          <span className="font-mono">^A</span> delimited — and get parsed
          fields with dictionary names, enum descriptions, and Tag 9
          (BodyLength) / Tag 10 (CheckSum) validation.
        </p>
      </div>

      <Card>
        <CardContent className="flex flex-col gap-3 pt-6">
          <textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            rows={4}
            spellCheck={false}
            placeholder="8=FIX.4.4|9=103|35=D|49=…|10=247|"
            className="w-full rounded-md border border-border bg-background p-3 font-mono text-xs leading-relaxed text-white placeholder:text-muted/50 focus:border-accent focus:outline-none"
          />
          <div className="flex flex-wrap items-center gap-3">
            <Button onClick={() => decode(input)} disabled={loading || !input.trim()}>
              <Play className="h-4 w-4" />
              {loading ? "Decoding…" : "Decode"}
            </Button>
            <Button variant="secondary" onClick={() => { setInput(SAMPLE); decode(SAMPLE); }}>
              Load sample
            </Button>
            <label className="ml-auto flex items-center gap-2 text-xs text-muted">
              Dictionary
              <select
                value={beginString}
                onChange={(e) => setBeginString(e.target.value)}
                className="rounded-md border border-border bg-background px-2 py-1.5 font-mono text-xs text-white"
              >
                <option value="">Auto (from 8=)</option>
                <option value="FIX.4.0">FIX.4.0</option>
                <option value="FIX.4.2">FIX.4.2</option>
                <option value="FIX.4.4">FIX.4.4</option>
                <option value="FIXT.1.1">FIXT.1.1</option>
                <option value="FIX.5.0SP2">FIX.5.0SP2</option>
              </select>
            </label>
          </div>
        </CardContent>
      </Card>

      {error && (
        <Card className="border-danger/40">
          <CardContent className="pt-6">
            <p className="text-sm text-danger">
              <span className="font-semibold">Couldn&apos;t decode: </span>
              {error}
            </p>
          </CardContent>
        </Card>
      )}

      {result && (
        <Card>
          <CardHeader className="flex flex-row items-center justify-between py-3">
            <CardTitle>Decoded message</CardTitle>
            <div className="flex gap-1">
              {VIEWS.map((v) => (
                <button
                  key={v}
                  onClick={() => setView(v)}
                  className={cn(
                    "rounded px-2 py-1 font-mono text-xs",
                    view === v ? "bg-accent/15 text-accent" : "text-muted hover:text-white"
                  )}
                >
                  {v}
                </button>
              ))}
            </div>
          </CardHeader>
          <CardContent className="flex flex-col gap-4">
            <div className="flex flex-wrap gap-2">
              <Badge variant={result.valid ? "in" : "danger"}>
                {result.valid ? "VALID" : "INVALID"}
              </Badge>
              <Badge variant="default">
                35={result.msgType} ({result.msgName || "unknown"})
              </Badge>
              <Badge variant={result.checksumOk ? "in" : "danger"}>
                Checksum {result.checksumOk ? "OK" : `MISMATCH (want ${result.expectedChecksum}, got ${result.actualChecksum})`}
              </Badge>
              <Badge variant={result.bodyLengthOk ? "in" : "danger"}>
                BodyLength {result.bodyLengthOk ? "OK" : `MISMATCH (want ${result.expectedBodyLength}, got ${result.actualBodyLength})`}
              </Badge>
            </div>

            {view === "SUMMARY" && (
              <dl className="grid grid-cols-1 gap-x-6 gap-y-2 sm:grid-cols-2">
                {[
                  ["MsgType", `35=${result.msgType} (${result.msgName || "unknown"})`],
                  ["BeginString", result.beginString],
                  ["Fields", String(result.fields.length)],
                  ["Checksum", `${result.actualChecksum} (expected ${result.expectedChecksum})`],
                  ["BodyLength", `${result.actualBodyLength} (expected ${result.expectedBodyLength})`],
                ].map(([k, v]) => (
                  <div key={k} className="flex items-baseline justify-between gap-4 border-b border-border/50 py-1.5">
                    <dt className="text-xs uppercase tracking-wider text-muted">{k}</dt>
                    <dd className="font-mono text-xs text-white">{v}</dd>
                  </div>
                ))}
              </dl>
            )}
            {view === "FIELDS" && <FieldsTable fields={result.fields} />}
            {view === "RAW" && (
              <div>
                <div className="mb-2 flex justify-end">
                  <Button size="sm" variant="secondary" onClick={copyRaw}>
                    {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                    {copied ? "Copied" : "Copy RAW (SOH)"}
                  </Button>
                </div>
                <pre className="whitespace-pre-wrap break-all rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
                  {result.fields.map((f) => `${f.tag}=${f.value}`).join("|") + "|"}
                </pre>
              </div>
            )}
            {view === "JSON" && (
              <pre className="max-h-96 overflow-auto rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
                {JSON.stringify(result, null, 2)}
              </pre>
            )}
          </CardContent>
        </Card>
      )}
    </div>
  );
}
