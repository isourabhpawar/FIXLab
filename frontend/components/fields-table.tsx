import type { FixField } from "@/types/fixlab";

// Shared FIX field table (spec §22): tag / name / value / enum, used by
// the session message inspector and the /tools/decoder page.
export function FieldsTable({ fields }: { fields: FixField[] }) {
  return (
    <div className="max-h-96 overflow-auto">
      <table className="w-full font-mono text-xs">
        <thead className="sticky top-0 bg-surface">
          <tr className="text-left text-muted">
            <th className="px-2 py-1.5 font-medium">Tag</th>
            <th className="px-2 py-1.5 font-medium">Name</th>
            <th className="px-2 py-1.5 font-medium">Value</th>
            <th className="px-2 py-1.5 font-medium">Enum</th>
          </tr>
        </thead>
        <tbody>
          {fields.map((f, i) => (
            <tr key={`${f.tag}-${i}`} className="border-t border-border/50 hover:bg-panel/50">
              <td className="px-2 py-1 text-info">{f.tag}</td>
              <td className="px-2 py-1 text-white">{f.name || "—"}</td>
              <td className="px-2 py-1 break-all text-muted">{f.value}</td>
              <td className="px-2 py-1 text-accent">{f.enumDescription || ""}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
