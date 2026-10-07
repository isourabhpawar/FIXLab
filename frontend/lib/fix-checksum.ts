// Client-side FIX checksum helpers (spec §36, phase 6).
// Tag 10 = (sum of every byte before the 10= field, SOH delimiters
// included) mod 256, rendered as 3 digits.

export const SOH = "\x01";

// normalizeFix accepts SOH, "|" and "^A" as field delimiters.
export function normalizeFix(raw: string): string {
  return raw.replaceAll("^A", SOH).replaceAll("|", SOH);
}

export function splitFields(soh: string): string[] {
  return soh.split(SOH).filter((p) => p.length > 0);
}

// computeChecksum sums every byte of bodySoh — the message WITHOUT the
// 10= field, SOH-delimited, trailing SOH after the last body field
// included (exactly as it appears on the wire).
export function computeChecksum(bodySoh: string): string {
  let sum = 0;
  for (let i = 0; i < bodySoh.length; i++) sum += bodySoh.charCodeAt(i);
  return String(sum % 256).padStart(3, "0");
}

export interface ChecksumAnalysis {
  fields: string[];
  hasChecksum: boolean;
  expected: string;
  actual?: string;
  ok?: boolean;
  // The full message with the correct 10= appended (SOH-delimited).
  fullSoh: string;
  // Same, with SOH rendered as "|" for readability.
  fullPipe: string;
}

// analyzeChecksum verifies an existing 10= or computes the correct one
// for a message pasted without it.
export function analyzeChecksum(raw: string): ChecksumAnalysis {
  const fields = splitFields(normalizeFix(raw.trim()));
  const last = fields[fields.length - 1] ?? "";
  const hasChecksum = last.startsWith("10=");
  const bodyFields = hasChecksum ? fields.slice(0, -1) : fields;
  const bodySoh = bodyFields.length > 0 ? bodyFields.join(SOH) + SOH : "";
  const expected = computeChecksum(bodySoh);
  const fullSoh = `${bodySoh}10=${expected}${SOH}`;
  const fullPipe = fullSoh.replaceAll(SOH, "|");
  if (!hasChecksum) {
    return { fields, hasChecksum, expected, fullSoh, fullPipe };
  }
  const actual = last.slice(3);
  const actualNum = parseInt(actual, 10);
  return {
    fields,
    hasChecksum,
    expected,
    actual,
    ok: !Number.isNaN(actualNum) && actualNum === parseInt(expected, 10),
    fullSoh,
    fullPipe,
  };
}
