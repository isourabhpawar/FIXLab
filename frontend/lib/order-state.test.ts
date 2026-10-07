// Assertion script for the order-state transition map (spec §30).
// Runs under plain node via type stripping: `node lib/order-state.test.ts`.
import assert from "node:assert";
import {
  STATES,
  TERMINAL_STATES,
  outgoing,
  isValidTransition,
  describeTransition,
  type OrderState,
} from "./order-state.ts";

// Every state is in the map.
assert.deepStrictEqual([...STATES].sort(), [
  "CANCELED",
  "FILLED",
  "NEW",
  "PARTIALLY_FILLED",
  "PENDING_CANCEL",
  "REJECTED",
  "REPLACED",
]);
for (const s of STATES) {
  assert.ok(Array.isArray(outgoing(s)), `outgoing(${s}) must be an array`);
}

// Acceptance: NEW→PARTIALLY_FILLED is valid…
assert.strictEqual(isValidTransition("NEW", "PARTIALLY_FILLED"), true);
// …and FILLED→PENDING_CANCEL is invalid (terminal states have no exits).
assert.strictEqual(isValidTransition("FILLED", "PENDING_CANCEL"), false);

// Terminal states have zero outgoing transitions.
for (const s of TERMINAL_STATES) {
  assert.deepStrictEqual(outgoing(s), [], `${s} must be terminal`);
}

// REPLACED behaves like NEW: every NEW flow is a REPLACED flow too.
for (const to of outgoing("NEW")) {
  assert.strictEqual(
    isValidTransition("REPLACED", to),
    true,
    `REPLACED->${to} must be valid`
  );
}

// The cancel-reject path restores the working order.
assert.strictEqual(isValidTransition("PENDING_CANCEL", "NEW"), true);
assert.strictEqual(isValidTransition("PENDING_CANCEL", "CANCELED"), true);

// No self-loops except genuine repeats.
assert.strictEqual(isValidTransition("PARTIALLY_FILLED", "PARTIALLY_FILLED"), true);
assert.strictEqual(isValidTransition("REPLACED", "REPLACED"), true);
assert.strictEqual(isValidTransition("NEW", "NEW"), false);
assert.strictEqual(isValidTransition("FILLED", "FILLED"), false);

// Backward jumps are invalid everywhere.
const backwards: [OrderState, OrderState][] = [
  ["FILLED", "NEW"],
  ["FILLED", "PARTIALLY_FILLED"],
  ["REJECTED", "NEW"],
  ["CANCELED", "PENDING_CANCEL"],
  ["PARTIALLY_FILLED", "NEW"],
];
for (const [from, to] of backwards) {
  assert.strictEqual(isValidTransition(from, to), false, `${from}->${to} must be invalid`);
}

// describeTransition always carries the FIX driver + explanation for
// valid flows, and names the terminal state for invalid ones.
const ok = describeTransition("NEW", "PARTIALLY_FILLED");
assert.strictEqual(ok.valid, true);
assert.ok(ok.fix.includes("150=1"), `fix must name ExecType: ${ok.fix}`);
assert.ok(ok.why.length > 10);

const bad = describeTransition("FILLED", "PENDING_CANCEL");
assert.strictEqual(bad.valid, false);
assert.ok(bad.why.includes("terminal"), `why must name terminality: ${bad.why}`);

const replaced = describeTransition("REPLACED", "FILLED");
assert.strictEqual(replaced.valid, true);
assert.ok(replaced.why.includes("stays working"));

// Every valid transition has a description with a non-empty FIX driver.
for (const from of STATES) {
  for (const to of outgoing(from)) {
    const info = describeTransition(from, to);
    assert.strictEqual(info.valid, true, `${from}->${to}`);
    assert.ok(info.fix.length > 3, `${from}->${to} needs a FIX driver`);
    assert.ok(info.why.length > 10, `${from}->${to} needs an explanation`);
  }
}

console.log("order-state: all assertions passed");
