// Client-side FIX timestamp converter (spec §36, phase 6): convert
// between UTCTimestamp, UTCDateOnly, UTCTimeOnly, ISO-8601 and epoch.
// All instants are UTC.

export interface TimestampForms {
  detected: string;
  epochMs: number;
  epochSec: number;
  utcTimestamp: string; // 20260107-05:30:00.000
  utcDateOnly: string; // 20260107
  utcTimeOnly: string; // 05:30:00.000
  iso8601: string; // 2026-01-07T05:30:00.000Z
}

const pad = (n: number, w = 2) => String(n).padStart(w, "0");

function fromParts(
  y: number,
  mo: number,
  d: number,
  h: number,
  mi: number,
  s: number,
  ms: number,
  label: string
): TimestampForms | null {
  const epochMs = Date.UTC(y, mo - 1, d, h, mi, s, ms);
  const check = new Date(epochMs);
  // Reject impossible dates (e.g. month 13, Feb 30) that Date.UTC
  // silently rolls over.
  if (
    check.getUTCFullYear() !== y ||
    check.getUTCMonth() !== mo - 1 ||
    check.getUTCDate() !== d ||
    check.getUTCHours() !== h ||
    check.getUTCMinutes() !== mi ||
    check.getUTCSeconds() !== s
  ) {
    return null;
  }
  return formatAll(epochMs, label);
}

export function formatAll(epochMs: number, detected: string): TimestampForms {
  const d = new Date(epochMs);
  return {
    detected,
    epochMs,
    epochSec: Math.floor(epochMs / 1000),
    utcTimestamp: `${d.getUTCFullYear()}${pad(d.getUTCMonth() + 1)}${pad(d.getUTCDate())}-${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}.${pad(d.getUTCMilliseconds(), 3)}`,
    utcDateOnly: `${d.getUTCFullYear()}${pad(d.getUTCMonth() + 1)}${pad(d.getUTCDate())}`,
    utcTimeOnly: `${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:${pad(d.getUTCSeconds())}.${pad(d.getUTCMilliseconds(), 3)}`,
    iso8601: d.toISOString(),
  };
}

const RE_TS = /^(\d{4})(\d{2})(\d{2})-(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$/;
const RE_DATE = /^(\d{4})(\d{2})(\d{2})$/;
const RE_TIME = /^(\d{2}):(\d{2}):(\d{2})(?:\.(\d{1,3}))?$/;
const RE_EPOCH = /^\d{10}$|^\d{13}$/;
const RE_ISO = /^\d{4}-\d{2}-\d{2}[T ]/;

function msFrac(frac: string | undefined): number {
  if (!frac) return 0;
  return parseInt(frac.padEnd(3, "0"), 10);
}

// parseTimestamp detects the input format and returns every other form,
// or null when the input is not a recognized timestamp.
export function parseTimestamp(input: string): TimestampForms | null {
  const s = input.trim();
  if (s === "") return null;

  let m: RegExpMatchArray | null;
  if ((m = s.match(RE_TS))) {
    return fromParts(+m[1], +m[2], +m[3], +m[4], +m[5], +m[6], msFrac(m[7]), "UTCTimestamp (52)");
  }
  if ((m = s.match(RE_DATE))) {
    return fromParts(+m[1], +m[2], +m[3], 0, 0, 0, 0, "UTCDateOnly");
  }
  if ((m = s.match(RE_TIME))) {
    // UTCTimeOnly carries no date: anchor it to today (UTC).
    const now = new Date();
    return fromParts(
      now.getUTCFullYear(),
      now.getUTCMonth() + 1,
      now.getUTCDate(),
      +m[1],
      +m[2],
      +m[3],
      msFrac(m[4]),
      "UTCTimeOnly (anchored to today UTC)"
    );
  }
  if (RE_EPOCH.test(s)) {
    const epochMs = s.length === 10 ? parseInt(s, 10) * 1000 : parseInt(s, 10);
    if (!Number.isFinite(epochMs)) return null;
    return formatAll(epochMs, s.length === 10 ? "Epoch seconds" : "Epoch milliseconds");
  }
  if (RE_ISO.test(s)) {
    const epochMs = Date.parse(s);
    if (Number.isNaN(epochMs)) return null;
    return formatAll(epochMs, "ISO-8601");
  }
  return null;
}
