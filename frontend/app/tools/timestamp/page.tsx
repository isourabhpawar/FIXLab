"use client";

import { useMemo, useState } from "react";
import { Check, Copy } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { formatAll, parseTimestamp } from "@/lib/fix-timestamp";

const SAMPLE = "20260107-05:30:00.000";

export default function TimestampPage() {
  const [input, setInput] = useState("");
  const [copiedKey, setCopiedKey] = useState<string | null>(null);

  const forms = useMemo(
    () => (input.trim() ? parseTimestamp(input) : null),
    [input]
  );

  const copy = async (key: string, value: string) => {
    try {
      await navigator.clipboard.writeText(value);
      setCopiedKey(key);
      setTimeout(() => setCopiedKey(null), 1500);
    } catch {
      /* ignore */
    }
  };

  const rows: [string, string][] = forms
    ? [
        ["UTCTimestamp", forms.utcTimestamp],
        ["UTCDateOnly", forms.utcDateOnly],
        ["UTCTimeOnly", forms.utcTimeOnly],
        ["ISO-8601", forms.iso8601],
        ["Epoch millis", String(forms.epochMs)],
        ["Epoch seconds", String(forms.epochSec)],
      ]
    : [];

  return (
    <div className="flex flex-col gap-6">
      <div className="flex max-w-2xl flex-col gap-3">
        <h1 className="text-3xl font-bold tracking-tight">Timestamp Converter</h1>
        <p className="text-muted">
          Paste any one timestamp format — FIX{" "}
          <span className="font-mono">UTCTimestamp</span> (tag 52),{" "}
          <span className="font-mono">UTCDateOnly</span>,{" "}
          <span className="font-mono">UTCTimeOnly</span>, ISO-8601, or epoch —
          and get all the others. Everything is UTC.
        </p>
      </div>

      <Card>
        <CardContent className="flex flex-col gap-3 pt-6">
          <div className="flex flex-wrap gap-3">
            <input
              value={input}
              onChange={(e) => setInput(e.target.value)}
              spellCheck={false}
              placeholder="20260107-05:30:00.000  ·  2026-01-07T05:30:00.000Z  ·  1767762600000"
              className="min-w-0 flex-1 rounded-md border border-border bg-background px-3 py-2 font-mono text-sm text-white placeholder:text-muted/50 focus:border-accent focus:outline-none"
            />
            <Button
              variant="secondary"
              onClick={() => {
                const now = formatAll(Date.now(), "now");
                setInput(now.utcTimestamp);
              }}
            >
              Now
            </Button>
            <Button variant="secondary" onClick={() => setInput(SAMPLE)}>
              Sample
            </Button>
          </div>
          {input.trim() !== "" && !forms && (
            <p className="text-sm text-danger">
              Not a recognized timestamp. Try{" "}
              <span className="font-mono">20260107-05:30:00.000</span>,{" "}
              <span className="font-mono">20260107</span>,{" "}
              <span className="font-mono">05:30:00.000</span>, an ISO-8601
              string, or epoch millis/seconds.
            </p>
          )}
        </CardContent>
      </Card>

      {forms && (
        <Card>
          <CardHeader>
            <CardTitle className="flex items-center gap-2">
              Converted
              <Badge variant="in">{forms.detected}</Badge>
            </CardTitle>
          </CardHeader>
          <CardContent>
            <dl className="flex flex-col gap-1">
              {rows.map(([k, v]) => (
                <div
                  key={k}
                  className="flex items-center justify-between gap-4 border-b border-border/50 py-2"
                >
                  <dt className="text-xs uppercase tracking-wider text-muted">{k}</dt>
                  <dd className="flex items-center gap-2">
                    <code className="font-mono text-sm text-white">{v}</code>
                    <button
                      onClick={() => copy(k, v)}
                      className="text-muted hover:text-white"
                      title={`Copy ${k}`}
                      aria-label={`Copy ${k}`}
                    >
                      {copiedKey === k ? (
                        <Check className="h-3.5 w-3.5 text-accent" />
                      ) : (
                        <Copy className="h-3.5 w-3.5" />
                      )}
                    </button>
                  </dd>
                </div>
              ))}
            </dl>
          </CardContent>
        </Card>
      )}
    </div>
  );
}
