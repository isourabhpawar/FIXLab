"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { BookOpenText, RotateCcw, Search, Upload } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import {
  clearDictionary,
  decodeFix,
  getDictionary,
  uploadDictionary,
} from "@/lib/api";
import type { DictionaryInfo } from "@/types/fixlab";

// Sample custom dictionary (phase 2.4): a minimal QuickFIX data
// dictionary defining one custom field, so the "try it" path works
// without a file at hand.
const SAMPLE_XML = `<?xml version="1.0" encoding="UTF-8"?>
<fix major="4" minor="4" type="FIX">
  <fields>
    <field number="9001" name="MyCustomField" type="STRING">
      <value enum="A" description="Alpha"/>
      <value enum="B" description="Beta"/>
    </field>
  </fields>
  <messages>
    <message name="NewOrderSingle" msgtype="D" msgcat="app"/>
  </messages>
</fix>`;

// Custom FIX dictionary panel (phase 2.4, spec §48): shows the active
// dictionary (standard or uploaded), uploads a broker's QuickFIX XML
// (file picker or paste), tests tag lookups against the active
// dictionary, and reverts to standard.
export function DictionaryPanel({ token }: { token: string }) {
  const [info, setInfo] = useState<DictionaryInfo | null>(null);
  const [xml, setXml] = useState("");
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [lookupTag, setLookupTag] = useState("");
  const [lookupResult, setLookupResult] = useState<string | null>(null);
  const [lookingUp, setLookingUp] = useState(false);
  const fileRef = useRef<HTMLInputElement>(null);

  const refresh = useCallback(async () => {
    try {
      setInfo(await getDictionary(token));
    } catch {
      // The session may be gone; leave the last known state.
    }
  }, [token]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const doUpload = async (text: string, dictName: string) => {
    setBusy(true);
    setError(null);
    try {
      const updated = await uploadDictionary(token, text, dictName);
      setInfo(updated);
      setXml("");
    } catch (e) {
      // The backend names the exact validation problem; show it verbatim.
      setError(e instanceof Error ? e.message : "Upload failed");
    } finally {
      setBusy(false);
    }
  };

  const onFile = async (f: File | undefined) => {
    if (!f) return;
    setError(null);
    if (f.size > 512 * 1024) {
      setError("Dictionary is too large: limit is 512 KiB");
      return;
    }
    const text = await f.text();
    setName((n) => n || f.name.replace(/\.xml$/i, ""));
    await doUpload(text, name || f.name.replace(/\.xml$/i, ""));
  };

  const onRevert = async () => {
    setBusy(true);
    setError(null);
    try {
      await clearDictionary(token);
      await refresh();
    } catch (e) {
      setError(e instanceof Error ? e.message : "Revert failed");
    } finally {
      setBusy(false);
    }
  };

  // Tag lookup: decode a synthetic 35=0 heartbeat carrying the tag, so
  // the lookup runs against the session's ACTIVE dictionary
  // (custom override included).
  const onLookup = async () => {
    const tag = lookupTag.trim();
    if (!/^\d+$/.test(tag)) {
      setLookupResult("Enter a numeric tag.");
      return;
    }
    setLookingUp(true);
    setLookupResult(null);
    try {
      const res = await decodeFix(
        `8=FIX.4.4|9=24|35=0|49=A|56=B|34=1|52=20260107-05:30:00|${tag}=A|10=000|`,
        undefined,
        token
      );
      const field = res.fields.find((f) => f.tag === Number(tag));
      if (!field) {
        setLookupResult(`Tag ${tag} not present in the decoded probe.`);
      } else {
        setLookupResult(
          `${field.tag} — ${field.name}${field.enumDescription ? ` (${field.enumDescription} for "A")` : ""}`
        );
      }
    } catch (e) {
      setLookupResult(e instanceof Error ? e.message : "Lookup failed");
    } finally {
      setLookingUp(false);
    }
  };

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <BookOpenText className="h-4 w-4" />
            Active dictionary
            {info && (
              <Badge variant={info.custom ? "default" : "live"} className="ml-1">
                {info.custom ? `CUSTOM${info.name ? `: ${info.name}` : ""}` : "STANDARD"}
              </Badge>
            )}
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3 text-sm">
          {info ? (
            <div className="grid grid-cols-3 gap-2 text-xs">
              <div className="rounded bg-panel p-2">
                <div className="text-muted">BeginString</div>
                <div className="font-mono">{info.beginString}</div>
              </div>
              <div className="rounded bg-panel p-2">
                <div className="text-muted">Fields</div>
                <div className="font-mono">{info.fields}</div>
              </div>
              <div className="rounded bg-panel p-2">
                <div className="text-muted">Messages</div>
                <div className="font-mono">{info.messages}</div>
              </div>
            </div>
          ) : (
            <div className="text-muted">Loading…</div>
          )}
          <p className="text-xs text-muted">
            A custom dictionary resolves your broker&apos;s tags to names and
            enum descriptions in the live feed, the message inspector, and the
            decoder. It is ephemeral — it dies with this session — and does not
            change wire validation: the FIX engine still validates against the
            standard dictionary, and user-defined fields (tags ≥ 5000) pass
            through to inspection instead of being session-rejected.
          </p>
          {info?.custom && (
            <Button variant="secondary" size="sm" onClick={onRevert} disabled={busy} className="w-fit">
              <RotateCcw className="mr-1 h-3.5 w-3.5" />
              Revert to standard dictionary
            </Button>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <Upload className="h-4 w-4" />
            Upload custom dictionary
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-3">
          <div className="flex flex-wrap items-center gap-2">
            <input
              ref={fileRef}
              type="file"
              accept=".xml,text/xml"
              className="hidden"
              onChange={(e) => onFile(e.target.files?.[0])}
            />
            <Button variant="secondary" size="sm" onClick={() => fileRef.current?.click()} disabled={busy}>
              Choose XML file…
            </Button>
            <Button
              variant="ghost"
              size="sm"
              disabled={busy}
              onClick={() => {
                setXml(SAMPLE_XML);
                setName((n) => n || "sample");
              }}
            >
              Load sample
            </Button>
            <Input
              placeholder="Dictionary name (optional)"
              value={name}
              onChange={(e) => setName(e.target.value)}
              className="max-w-56"
            />
          </div>
          <textarea
            value={xml}
            onChange={(e) => setXml(e.target.value)}
            placeholder="…or paste QuickFIX data-dictionary XML here"
            rows={8}
            spellCheck={false}
            className={cn(
              "w-full rounded border border-border bg-panel p-2 font-mono text-xs",
              "placeholder:text-muted/60 focus:outline-none focus:ring-1 focus:ring-ring"
            )}
          />
          {error && (
            <div className="rounded border border-red-500/40 bg-red-500/10 p-2 text-xs text-red-300">
              {error}
            </div>
          )}
          <Button
            size="sm"
            className="w-fit"
            disabled={busy || xml.trim() === ""}
            onClick={() => doUpload(xml, name)}
          >
            {busy ? "Validating…" : "Validate & install"}
          </Button>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2 text-sm">
            <Search className="h-4 w-4" />
            Test a tag lookup
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-2">
          <div className="flex gap-2">
            <Input
              placeholder="e.g. 9001"
              value={lookupTag}
              onChange={(e) => setLookupTag(e.target.value)}
              onKeyDown={(e) => e.key === "Enter" && onLookup()}
              className="max-w-40 font-mono"
            />
            <Button size="sm" variant="secondary" onClick={onLookup} disabled={lookingUp}>
              Look up
            </Button>
          </div>
          {lookupResult && (
            <div className="rounded bg-panel p-2 font-mono text-xs">{lookupResult}</div>
          )}
          <p className="text-xs text-muted">
            Resolves against this session&apos;s active dictionary — upload a
            custom dictionary, then look up one of its tags (e.g. 9001 in the
            sample).
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
