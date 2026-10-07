"use client";

import { useMemo, useState } from "react";
import { Check, Copy } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { analyzeChecksum } from "@/lib/fix-checksum";

const SAMPLE_NO_CHECKSUM =
  "8=FIX.4.4|9=103|35=D|49=TESTBUY1|56=FIXLAB|34=12|52=20260107-05:30:00.000|11=ORD123|55=MSFT|54=1|38=100|40=2|44=310.50|";

export default function ChecksumPage() {
  const [input, setInput] = useState("");
  const [copied, setCopied] = useState(false);

  const analysis = useMemo(
    () => (input.trim() ? analyzeChecksum(input) : null),
    [input]
  );

  const copy = async () => {
    if (!analysis) return;
    try {
      await navigator.clipboard.writeText(analysis.fullSoh);
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    } catch {
      /* ignore */
    }
  };

  return (
    <div className="flex flex-col gap-6">
      <div className="flex max-w-2xl flex-col gap-3">
        <h1 className="text-3xl font-bold tracking-tight">Checksum Calculator</h1>
        <p className="text-muted">
          Paste a FIX message <em>without</em> Tag 10 to compute the correct
          checksum, or paste a complete message to verify it. Accepts SOH,{" "}
          <span className="font-mono">|</span> and{" "}
          <span className="font-mono">^A</span> delimiters.
        </p>
        <p className="font-mono text-xs text-muted">
          Tag 10 = (sum of every byte before the 10= field, SOH delimiters
          included) mod 256, as 3 digits.
        </p>
      </div>

      <Card>
        <CardContent className="flex flex-col gap-3 pt-6">
          <textarea
            value={input}
            onChange={(e) => setInput(e.target.value)}
            rows={4}
            spellCheck={false}
            placeholder="8=FIX.4.4|9=103|35=D|…  (with or without 10=)"
            className="w-full rounded-md border border-border bg-background p-3 font-mono text-xs leading-relaxed text-white placeholder:text-muted/50 focus:border-accent focus:outline-none"
          />
          <div>
            <Button variant="secondary" onClick={() => setInput(SAMPLE_NO_CHECKSUM)}>
              Load sample (no 10=)
            </Button>
          </div>
        </CardContent>
      </Card>

      {analysis && (
        <Card>
          <CardHeader>
            <CardTitle className="flex flex-wrap items-center gap-2">
              Result
              {!analysis.hasChecksum && (
                <Badge variant="in">computed 10={analysis.expected}</Badge>
              )}
              {analysis.hasChecksum && analysis.ok && (
                <Badge variant="in">VALID CHECKSUM</Badge>
              )}
              {analysis.hasChecksum && !analysis.ok && (
                <Badge variant="danger">
                  MISMATCH — expected 10={analysis.expected}, got 10={analysis.actual}
                </Badge>
              )}
            </CardTitle>
          </CardHeader>
          <CardContent className="flex flex-col gap-3">
            {analysis.hasChecksum && (
              <dl className="grid grid-cols-1 gap-x-6 gap-y-2 sm:grid-cols-2">
                {[
                  ["Expected 10=", analysis.expected],
                  ["Actual 10=", analysis.actual ?? "—"],
                ].map(([k, v]) => (
                  <div key={k} className="flex items-baseline justify-between gap-4 border-b border-border/50 py-1.5">
                    <dt className="text-xs uppercase tracking-wider text-muted">{k}</dt>
                    <dd className="font-mono text-xs text-white">{v}</dd>
                  </div>
                ))}
              </dl>
            )}
            <div>
              <div className="mb-2 flex items-center justify-between">
                <p className="text-xs uppercase tracking-wider text-muted">
                  Message with correct 10= (SOH shown as |)
                </p>
                <Button size="sm" variant="secondary" onClick={copy}>
                  {copied ? <Check className="h-3.5 w-3.5" /> : <Copy className="h-3.5 w-3.5" />}
                  {copied ? "Copied" : "Copy (SOH)"}
                </Button>
              </div>
              <pre className="whitespace-pre-wrap break-all rounded-md bg-background p-3 font-mono text-xs leading-relaxed text-muted">
                {analysis.fullPipe}
              </pre>
            </div>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
