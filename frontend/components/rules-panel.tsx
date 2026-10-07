"use client";

import { useCallback, useEffect, useState } from "react";
import { Pencil, Plus, RefreshCw, Trash2, X } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { Input } from "@/components/ui/input";
import { Select } from "@/components/ui/select";
import { cn } from "@/lib/utils";
import {
  createRule,
  deleteRule,
  getRules,
  updateRule,
} from "@/lib/api";
import type {
  PredicateOp,
  Rule,
  RuleAction,
  RuleActionType,
  RuleInput,
  RulePredicate,
} from "@/types/fixlab";

// Supported predicate fields (spec §32): tags 38/40/44 compare
// numerically, the rest as strings.
const FIELDS: { tag: number; name: string; numeric: boolean }[] = [
  { tag: 55, name: "Symbol", numeric: false },
  { tag: 38, name: "OrderQty", numeric: true },
  { tag: 54, name: "Side", numeric: false },
  { tag: 11, name: "ClOrdID", numeric: false },
  { tag: 1, name: "Account", numeric: false },
  { tag: 40, name: "OrdType", numeric: true },
  { tag: 44, name: "Price", numeric: true },
];

const OPS: { value: PredicateOp; label: string; numericOnly: boolean }[] = [
  { value: "eq", label: "=  equals", numericOnly: false },
  { value: "ne", label: "≠  not equals", numericOnly: false },
  { value: "gt", label: ">  greater than", numericOnly: true },
  { value: "lt", label: "<  less than", numericOnly: true },
  { value: "contains", label: "∋  contains", numericOnly: false },
];

const ACTION_TYPES: { value: RuleActionType; label: string }[] = [
  { value: "ACK_NEW", label: "ACK_NEW — acknowledge (150=0/39=0)" },
  { value: "FULL_FILL", label: "FULL_FILL — fill remainder" },
  { value: "PARTIAL_FILL", label: "PARTIAL_FILL — partial fill" },
  { value: "REJECT", label: "REJECT — reject order" },
  { value: "DELAY", label: "DELAY — sleep, then continue chain" },
];

const MAX_DELAY_MS = 60000;

function fieldName(tag: number): string {
  return FIELDS.find((f) => f.tag === tag)?.name ?? `tag ${tag}`;
}

function summarizePredicates(preds: RulePredicate[]): string {
  if (preds.length === 0) return "always";
  return preds.map((p) => `${fieldName(p.tag)} ${p.op} "${p.value}"`).join(" AND ");
}

function summarizeActions(actions: RuleAction[]): string {
  return actions
    .map((a) => {
      switch (a.type) {
        case "DELAY":
          return `DELAY ${a.delayMs ?? 0}ms`;
        case "FULL_FILL":
          return a.price ? `FULL_FILL @ ${a.price}` : "FULL_FILL";
        case "PARTIAL_FILL":
          return `PARTIAL_FILL ${a.qty ?? "?"} @ ${a.price ?? "?"}`;
        case "REJECT":
          return `REJECT${a.ordRejReason ? ` (${a.ordRejReason})` : ""}${a.text ? ` "${a.text}"` : ""}`;
        case "ACK_NEW":
          return "ACK_NEW";
      }
    })
    .join(" → ");
}

// Client-side validation mirrors the backend DSL (spec §32/§33).
function validateInput(input: RuleInput): string | null {
  if (input.actions.length === 0) return "At least one action is required.";
  for (const p of input.predicates) {
    const field = FIELDS.find((f) => f.tag === p.tag);
    if (!field) return `Unsupported tag ${p.tag}.`;
    const op = OPS.find((o) => o.value === p.op);
    if (!op) return `Unknown operator ${p.op}.`;
    if (op.numericOnly && !field.numeric)
      return `"${op.label}" needs a numeric field (OrderQty, OrdType, Price).`;
    if (op.numericOnly && !Number.isFinite(Number(p.value)))
      return `"${op.label}" on ${field.name} needs a numeric value.`;
  }
  for (const a of input.actions) {
    switch (a.type) {
      case "PARTIAL_FILL":
        if (!(a.qty! > 0) || !(a.price! > 0))
          return "PARTIAL_FILL needs qty > 0 and price > 0.";
        break;
      case "REJECT":
        if (!a.ordRejReason?.trim() && !a.text?.trim())
          return "REJECT needs an OrdRejReason or text.";
        break;
      case "DELAY":
        if (!(a.delayMs! >= 0) || a.delayMs! > MAX_DELAY_MS)
          return `DELAY needs 0–${MAX_DELAY_MS} ms.`;
        break;
      case "FULL_FILL":
        if (a.price !== undefined && !(a.price >= 0))
          return "FULL_FILL price must be >= 0.";
        break;
    }
  }
  return null;
}

const EMPTY_INPUT: RuleInput = {
  name: "",
  priority: 10,
  enabled: true,
  predicates: [],
  actions: [{ type: "ACK_NEW" }],
};

export function RulesPanel({ token }: { token: string }) {
  const [rules, setRules] = useState<Rule[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [dialog, setDialog] = useState<{ input: RuleInput; editingId: string | null } | null>(null);
  const [busy, setBusy] = useState(false);

  const refresh = useCallback(async () => {
    try {
      const res = await getRules(token);
      setRules(res.rules);
      setError(null);
    } catch (err) {
      setError(err instanceof Error ? err.message : "Failed to load rules");
    } finally {
      setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    refresh();
  }, [refresh]);

  const save = async () => {
    if (!dialog) return;
    const problem = validateInput(dialog.input);
    if (problem) {
      setError(problem);
      return;
    }
    setBusy(true);
    setError(null);
    try {
      if (dialog.editingId) {
        await updateRule(token, dialog.editingId, dialog.input);
      } else {
        await createRule(token, dialog.input);
      }
      setDialog(null);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Save failed");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (id: string) => {
    if (!confirm("Delete this rule?")) return;
    try {
      await deleteRule(token, id);
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Delete failed");
    }
  };

  const toggle = async (rule: Rule) => {
    try {
      await updateRule(token, rule.id, {
        name: rule.name,
        priority: rule.priority,
        enabled: !rule.enabled,
        predicates: rule.predicates,
        actions: rule.actions,
      });
      await refresh();
    } catch (err) {
      setError(err instanceof Error ? err.message : "Toggle failed");
    }
  };

  return (
    <div className="flex flex-col gap-4">
      <Card>
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle>Deterministic rules</CardTitle>
            <div className="flex gap-2">
              <Button size="sm" variant="secondary" onClick={refresh}>
                <RefreshCw className="h-3.5 w-3.5" />
                Refresh
              </Button>
              <Button
                size="sm"
                onClick={() =>
                  setDialog({ input: { ...EMPTY_INPUT, predicates: [], actions: [{ type: "ACK_NEW" }] }, editingId: null })
                }
              >
                <Plus className="h-3.5 w-3.5" />
                Add rule
              </Button>
            </div>
          </div>
          <p className="mt-2 text-xs text-muted">
            Rules fire on inbound NewOrderSingle in priority order (lowest
            first); the first matching rule wins. The kill switch is
            evaluated before all rules. Rule-fired executions consume the
            session quota and stream the same ORDER_UPDATED / EXECUTION_SENT
            events as manual executions.
          </p>
        </CardHeader>
        <CardContent>
          {loading && <p className="text-sm text-muted">Loading rules…</p>}
          {error && <p className="mb-3 text-sm text-danger">{error}</p>}
          {!loading && rules.length === 0 && (
            <p className="text-sm text-muted">
              No rules yet. Add one to automate fills, rejects, and delays —
              or drive orders manually from the Orders tab.
            </p>
          )}
          {rules.length > 0 && (
            <div className="overflow-x-auto">
              <table className="w-full text-sm">
                <thead>
                  <tr className="border-b border-border text-left text-xs uppercase tracking-wider text-muted">
                    <th className="py-2 pr-4">Priority</th>
                    <th className="py-2 pr-4">Rule</th>
                    <th className="py-2 pr-4">If</th>
                    <th className="py-2 pr-4">Then</th>
                    <th className="py-2 pr-4">Matched</th>
                    <th className="py-2 pr-4">Enabled</th>
                    <th className="py-2" />
                  </tr>
                </thead>
                <tbody>
                  {rules.map((r) => (
                    <tr key={r.id} className="border-b border-border/50 last:border-0">
                      <td className="py-2 pr-4 font-mono text-white">{r.priority}</td>
                      <td className="py-2 pr-4">
                        <div className="font-medium text-white">{r.name || <span className="text-muted">unnamed</span>}</div>
                        <div className="font-mono text-[10px] text-muted">{r.id}</div>
                      </td>
                      <td className="max-w-xs py-2 pr-4 font-mono text-xs text-muted">
                        {summarizePredicates(r.predicates)}
                      </td>
                      <td className="max-w-xs py-2 pr-4 font-mono text-xs text-muted">
                        {summarizeActions(r.actions)}
                      </td>
                      <td className="py-2 pr-4 font-mono text-white">{r.matchedCount}</td>
                      <td className="py-2 pr-4">
                        <button
                          onClick={() => toggle(r)}
                          className={cn(
                            "rounded-full px-2.5 py-0.5 font-mono text-xs",
                            r.enabled
                              ? "bg-accent/15 text-accent"
                              : "bg-panel text-muted"
                          )}
                          title={r.enabled ? "Disable rule" : "Enable rule"}
                        >
                          {r.enabled ? "ON" : "OFF"}
                        </button>
                      </td>
                      <td className="py-2">
                        <div className="flex justify-end gap-1">
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={() =>
                              setDialog({
                                input: {
                                  name: r.name,
                                  priority: r.priority,
                                  enabled: r.enabled,
                                  predicates: r.predicates.map((p) => ({ ...p })),
                                  actions: r.actions.map((a) => ({ ...a })),
                                },
                                editingId: r.id,
                              })
                            }
                          >
                            <Pencil className="h-3.5 w-3.5" />
                          </Button>
                          <Button size="sm" variant="ghost" onClick={() => remove(r.id)}>
                            <Trash2 className="h-3.5 w-3.5 text-danger" />
                          </Button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </CardContent>
      </Card>

      {dialog && (
        <RuleDialog
          input={dialog.input}
          editing={dialog.editingId !== null}
          busy={busy}
          onChange={(input) => setDialog({ ...dialog, input })}
          onSave={save}
          onClose={() => setDialog(null)}
        />
      )}
    </div>
  );
}

function RuleDialog({
  input,
  editing,
  busy,
  onChange,
  onSave,
  onClose,
}: {
  input: RuleInput;
  editing: boolean;
  busy: boolean;
  onChange: (input: RuleInput) => void;
  onSave: () => void;
  onClose: () => void;
}) {
  const set = (patch: Partial<RuleInput>) => onChange({ ...input, ...patch });

  const setPredicate = (i: number, patch: Partial<RulePredicate>) => {
    const predicates = input.predicates.map((p, j) => (j === i ? { ...p, ...patch } : p));
    // If the field changed to a non-numeric one, gt/lt is no longer valid.
    const field = FIELDS.find((f) => f.tag === predicates[i].tag);
    if (field && !field.numeric && (predicates[i].op === "gt" || predicates[i].op === "lt")) {
      predicates[i].op = "eq";
    }
    set({ predicates });
  };

  const setAction = (i: number, patch: Partial<RuleAction>) => {
    const actions = input.actions.map((a, j) => (j === i ? { ...a, ...patch } : a));
    set({ actions });
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
      <Card className="max-h-[90vh] w-full max-w-2xl overflow-y-auto">
        <CardHeader>
          <div className="flex items-center justify-between">
            <CardTitle>{editing ? "Edit rule" : "Add rule"}</CardTitle>
            <Button size="sm" variant="ghost" onClick={onClose}>
              <X className="h-4 w-4" />
            </Button>
          </div>
        </CardHeader>
        <CardContent className="flex flex-col gap-5">
          <div className="grid grid-cols-3 gap-3">
            <div className="col-span-2 flex flex-col gap-1.5">
              <label className="text-xs font-medium uppercase tracking-wider text-muted" htmlFor="rule-name">
                Name
              </label>
              <Input
                id="rule-name"
                value={input.name}
                onChange={(e) => set({ name: e.target.value })}
                placeholder="e.g. msft-autofill"
              />
            </div>
            <div className="flex flex-col gap-1.5">
              <label className="text-xs font-medium uppercase tracking-wider text-muted" htmlFor="rule-priority">
                Priority
              </label>
              <Input
                id="rule-priority"
                type="number"
                value={input.priority}
                onChange={(e) => set({ priority: Number(e.target.value) })}
              />
            </div>
          </div>
          <label className="flex items-center gap-2 text-sm text-white">
            <input
              type="checkbox"
              checked={input.enabled}
              onChange={(e) => set({ enabled: e.target.checked })}
              className="h-4 w-4 accent-emerald-500"
            />
            Enabled
          </label>

          <div>
            <div className="mb-2 flex items-center justify-between">
              <h4 className="text-xs font-medium uppercase tracking-wider text-muted">
                If — predicates (all must match)
              </h4>
              <Button
                size="sm"
                variant="secondary"
                onClick={() =>
                  set({ predicates: [...input.predicates, { tag: 55, op: "eq", value: "" }] })
                }
              >
                <Plus className="h-3.5 w-3.5" /> Predicate
              </Button>
            </div>
            {input.predicates.length === 0 && (
              <p className="text-xs text-muted">No predicates — the rule matches every NewOrderSingle.</p>
            )}
            <div className="flex flex-col gap-2">
              {input.predicates.map((p, i) => {
                const field = FIELDS.find((f) => f.tag === p.tag);
                return (
                  <div key={i} className="flex items-end gap-2">
                    <div className="w-40">
                      <Select
                        label={i === 0 ? "Field" : ""}
                        value={String(p.tag)}
                        onChange={(e) => setPredicate(i, { tag: Number(e.target.value) })}
                        options={FIELDS.map((f) => ({
                          value: String(f.tag),
                          label: `${f.tag} — ${f.name}`,
                        }))}
                      />
                    </div>
                    <div className="w-44">
                      <Select
                        label={i === 0 ? "Operator" : ""}
                        value={p.op}
                        onChange={(e) => setPredicate(i, { op: e.target.value as PredicateOp })}
                        options={OPS.filter((o) => !o.numericOnly || field?.numeric).map((o) => ({
                          value: o.value,
                          label: o.label,
                        }))}
                      />
                    </div>
                    <div className="flex-1">
                      {i === 0 && (
                        <label className="mb-1.5 block text-xs font-medium uppercase tracking-wider text-muted">
                          Value
                        </label>
                      )}
                      <Input
                        value={p.value}
                        onChange={(e) => setPredicate(i, { value: e.target.value })}
                        placeholder={field?.numeric ? "e.g. 10000" : "e.g. MSFT"}
                      />
                    </div>
                    <Button size="sm" variant="ghost" onClick={() =>
                      set({ predicates: input.predicates.filter((_, j) => j !== i) })
                    }>
                      <X className="h-3.5 w-3.5" />
                    </Button>
                  </div>
                );
              })}
            </div>
          </div>

          <div>
            <div className="mb-2 flex items-center justify-between">
              <h4 className="text-xs font-medium uppercase tracking-wider text-muted">
                Then — action chain (runs in order)
              </h4>
              <Button
                size="sm"
                variant="secondary"
                onClick={() => set({ actions: [...input.actions, { type: "ACK_NEW" }] })}
              >
                <Plus className="h-3.5 w-3.5" /> Action
              </Button>
            </div>
            <div className="flex flex-col gap-2">
              {input.actions.map((a, i) => (
                <div key={i} className="flex items-end gap-2 rounded-md border border-border/50 p-2">
                  <div className="w-64 shrink-0">
                    <Select
                      label={i === 0 ? "Action" : ""}
                      value={a.type}
                      onChange={(e) => setAction(i, { type: e.target.value as RuleActionType })}
                      options={ACTION_TYPES}
                    />
                  </div>
                  <ActionParams action={a} first={i === 0} onChange={(patch) => setAction(i, patch)} />
                  <Button size="sm" variant="ghost" onClick={() =>
                    set({ actions: input.actions.filter((_, j) => j !== i) })
                  }>
                    <X className="h-3.5 w-3.5" />
                  </Button>
                </div>
              ))}
            </div>
          </div>

          <div className="flex justify-end gap-2">
            <Button variant="secondary" onClick={onClose} disabled={busy}>
              Cancel
            </Button>
            <Button onClick={onSave} disabled={busy}>
              {busy ? "Saving…" : editing ? "Save changes" : "Create rule"}
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  );
}

function ActionParams({
  action,
  first,
  onChange,
}: {
  action: RuleAction;
  first: boolean;
  onChange: (patch: Partial<RuleAction>) => void;
}) {
  const label = (text: string) =>
    first ? (
      <label className="mb-1.5 block text-xs font-medium uppercase tracking-wider text-muted">
        {text}
      </label>
    ) : null;
  switch (action.type) {
    case "DELAY":
      return (
        <div className="w-32">
          {label("Delay (ms)")}
          <Input
            type="number"
            min={0}
            max={MAX_DELAY_MS}
            value={action.delayMs ?? 0}
            onChange={(e) => onChange({ delayMs: Number(e.target.value) })}
          />
        </div>
      );
    case "FULL_FILL":
      return (
        <div className="w-40">
          {label("Price (blank = order price)")}
          <Input
            type="number"
            min={0}
            step="any"
            value={action.price ?? ""}
            onChange={(e) =>
              onChange({ price: e.target.value === "" ? undefined : Number(e.target.value) })
            }
            placeholder="order price"
          />
        </div>
      );
    case "PARTIAL_FILL":
      return (
        <>
          <div className="w-32">
            {label("Qty")}
            <Input
              type="number"
              min={0}
              step="any"
              value={action.qty ?? ""}
              onChange={(e) => onChange({ qty: Number(e.target.value) })}
            />
          </div>
          <div className="w-32">
            {label("Price")}
            <Input
              type="number"
              min={0}
              step="any"
              value={action.price ?? ""}
              onChange={(e) => onChange({ price: Number(e.target.value) })}
            />
          </div>
        </>
      );
    case "REJECT":
      return (
        <>
          <div className="w-32">
            {label("OrdRejReason (103)")}
            <Input
              value={action.ordRejReason ?? ""}
              onChange={(e) => onChange({ ordRejReason: e.target.value })}
              placeholder="e.g. 99"
            />
          </div>
          <div className="flex-1">
            {label("Text (58)")}
            <Input
              value={action.text ?? ""}
              onChange={(e) => onChange({ text: e.target.value })}
              placeholder="e.g. size limit"
            />
          </div>
        </>
      );
    default:
      return null;
  }
}

export function KillSwitchBanner() {
  return (
    <div className="rounded-md border border-danger/60 bg-danger/10 px-4 py-2.5 text-center">
      <span className="font-mono text-sm font-bold tracking-wide text-danger">
        🔴 VENUE HALTED
      </span>
      <span className="ml-2 text-xs text-muted">
        Kill switch is on — every inbound NewOrderSingle is being rejected.
      </span>
    </div>
  );
}
