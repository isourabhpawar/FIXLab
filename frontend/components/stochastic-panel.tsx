"use client";

import { useCallback, useEffect, useState } from "react";
import { Dices, RefreshCw } from "lucide-react";
import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { cn } from "@/lib/utils";
import { getStochastic, updateStochastic } from "@/lib/api";
import type { StochasticConfig } from "@/types/fixlab";

const DEFAULTS: StochasticConfig = {
  enabled: false,
  acceptPct: 85,
  rejectPct: 15,
  partialFillPct: 0,
  avgLatencyMs: 45,
  stdDevMs: 10,
  maxDelayMs: 5000,
  seed: 0,
};

function NumField({
  label,
  value,
  onChange,
  min,
  step,
  hint,
}: {
  label: string;
  value: number;
  onChange: (v: number) => void;
  min?: number;
  step?: string;
  hint?: string;
}) {
  return (
    <label className="flex flex-col gap-1">
      <span className="text-xs text-muted">{label}</span>
      <Input
        type="number"
        value={Number.isFinite(value) ? value : ""}
        min={min}
        step={step ?? "1"}
        onChange={(e) => onChange(e.target.value === "" ? NaN : Number(e.target.value))}
        className="w-full"
      />
      {hint && <span className="text-[11px] text-muted/70">{hint}</span>}
    </label>
  );
}

// Stochastic simulation policy editor (phase 2.1, spec §48): outcome
// percentages, latency distribution, and an optional seed for
// reproducible tests.
export function StochasticPanel({ token }: { token: string }) {
  const [cfg, setCfg] = useState<StochasticConfig>(DEFAULTS);
  const [outcomes, setOutcomes] = useState({ fill: 0, reject: 0, partial: 0 });
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const st = await getStochastic(token);
      setCfg({ ...DEFAULTS, ...st.config });
      setOutcomes(st.outcomes);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to load policy");
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    void load();
  }, [load]);

  const sum = cfg.acceptPct + cfg.rejectPct + cfg.partialFillPct;
  const sumOk = Number.isFinite(sum) && Math.abs(sum - 100) < 1e-9;
  const valid =
    sumOk &&
    cfg.avgLatencyMs >= 0 &&
    cfg.stdDevMs >= 0 &&
    cfg.maxDelayMs >= 0;

  const save = async () => {
    setSaving(true);
    setError(null);
    try {
      const st = await updateStochastic(token, cfg);
      setCfg({ ...DEFAULTS, ...st.config });
      setOutcomes(st.outcomes);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Failed to save policy");
    } finally {
      setSaving(false);
    }
  };

  if (loading) return <p className="text-sm text-muted">Loading policy…</p>;

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <CardTitle className="flex items-center gap-2">
            <Dices className="h-4 w-4" />
            Stochastic simulator
            <Badge variant={cfg.enabled ? "live" : "warn"}>
              {cfg.enabled ? "ON" : "OFF"}
            </Badge>
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          <p className="text-xs text-muted">
            Draws a random outcome for every inbound NewOrderSingle after a
            random latency. Precedence:{" "}
            <span className="text-white">kill switch → deterministic rules → stochastic → manual ack</span>.
            A rule that fires (or the kill switch) always wins — stochastic
            never overrides them.
          </p>

          <label className="flex items-center gap-2 text-sm">
            <input
              type="checkbox"
              checked={cfg.enabled}
              onChange={(e) => setCfg({ ...cfg, enabled: e.target.checked })}
              className="h-4 w-4 accent-white"
            />
            Enable stochastic simulation
          </label>

          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <NumField label="Accept %" value={cfg.acceptPct} min={0} step="0.1"
              onChange={(v) => setCfg({ ...cfg, acceptPct: v })} hint="full fill at order price" />
            <NumField label="Reject %" value={cfg.rejectPct} min={0} step="0.1"
              onChange={(v) => setCfg({ ...cfg, rejectPct: v })} hint="150=8/39=8" />
            <NumField label="Partial fill %" value={cfg.partialFillPct} min={0} step="0.1"
              onChange={(v) => setCfg({ ...cfg, partialFillPct: v })} hint="10–90% of qty; rest stays working" />
          </div>
          <p className={cn("text-xs", sumOk ? "text-muted" : "text-danger")}>
            Sum: {Number.isFinite(sum) ? sum.toFixed(1) : "—"}%{" "}
            {sumOk ? "✓" : "— must total 100%"}
          </p>

          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <NumField label="Avg latency (ms)" value={cfg.avgLatencyMs} min={0}
              onChange={(v) => setCfg({ ...cfg, avgLatencyMs: v })} hint="mean of the delay distribution" />
            <NumField label="Std dev (ms)" value={cfg.stdDevMs} min={0}
              onChange={(v) => setCfg({ ...cfg, stdDevMs: v })} hint="0 = fixed delay" />
            <NumField label="Max delay (ms)" value={cfg.maxDelayMs} min={0}
              onChange={(v) => setCfg({ ...cfg, maxDelayMs: v })} hint="caps the sampled delay (0 = 5000)" />
          </div>

          <NumField label="Seed (optional)" value={cfg.seed} min={0} step="1"
            onChange={(v) => setCfg({ ...cfg, seed: v })}
            hint="0 = random. A fixed seed reproduces the exact outcome sequence — useful for tests." />

          {error && <p className="text-sm text-danger">{error}</p>}

          <div className="flex items-center gap-2">
            <Button onClick={save} disabled={saving || !valid}>
              {saving ? "Saving…" : "Save policy"}
            </Button>
            <Button variant="secondary" onClick={() => void load()} disabled={loading}>
              <RefreshCw className="h-4 w-4" />
            </Button>
            {!valid && (
              <span className="text-xs text-danger">
                Fix the highlighted values before saving.
              </span>
            )}
          </div>
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Outcomes drawn this session</CardTitle>
        </CardHeader>
        <CardContent>
          <div className="flex gap-4 text-sm">
            <span>
              <Badge variant="in">{outcomes.fill}</Badge>{" "}
              <span className="text-muted">fills</span>
            </span>
            <span>
              <Badge variant="danger">{outcomes.reject}</Badge>{" "}
              <span className="text-muted">rejects</span>
            </span>
            <span>
              <Badge variant="out">{outcomes.partial}</Badge>{" "}
              <span className="text-muted">partials</span>
            </span>
          </div>
          <p className="mt-2 text-[11px] text-muted/70">
            Counts reset with the session. Refresh to update.
          </p>
        </CardContent>
      </Card>
    </div>
  );
}
