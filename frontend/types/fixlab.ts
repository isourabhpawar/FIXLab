// Shared FixLab types (backend JSON contracts, spec §18/§19/§38).

export interface SessionInfo {
  sessionToken: string;
  role: string;
  endpoint: { host: string; port: number; tls: boolean };
  identifiers: { beginString: string; senderCompId: string; targetCompId: string };
  status: string;
  createdAt: string;
  expiresAt: string;
  appMessages: number;
  messageCount: number;
  killSwitch: boolean; // phase 4: venue halt state
  stochastic: StochasticState; // phase 2.1: stochastic policy + outcomes
  remoteIp?: string; // phase 5: pinned SSRF-validated dial IP (initiator only)
  // Phase 7: present only on TLS sessions.
  certFingerprint?: string; // SHA-256 hex of the serving cert
  certSelfSigned?: boolean;
  replay?: ReplayStatus; // phase 2.3: scenario replay progress
  dictionary: DictionaryInfo; // phase 2.4: active FIX dictionary
}

// Active FIX dictionary (phase 2.4, spec §48): the standard embedded
// dictionary, or an uploaded custom override.
export interface DictionaryInfo {
  custom: boolean;
  name?: string;
  beginString: string;
  fields: number;
  messages: number;
}

// Scenario replay status (phase 2.3, spec §48).
export interface ReplayStatus {
  inProgress: boolean;
  totalSteps: number;
  doneSteps: number;
  failed: boolean;
  error?: string;
}

// Recorded scenario step (phase 2.3, spec §48).
export interface ScenarioStep {
  seq: number;
  atMs: number;
  kind: "logon" | "inbound" | "execution" | "logout" | "disconnect";
  payload: Record<string, any>;
}

// Exportable scenario document (phase 2.3). Never contains the token.
export interface Scenario {
  version: number;
  role: string;
  beginString?: string;
  targetCompId?: string;
  recordedAt: string;
  dropped?: number;
  steps: ScenarioStep[];
}

// POST /api/v1/sessions/replay response (phase 2.3).
export interface ReplayResult {
  sessionToken: string;
  totalSteps: number;
  speed: number;
}

export interface FixField {
  tag: number;
  name: string;
  value: string;
  enumDescription?: string;
}

export interface FixMessage {
  type: string; // "FIX_MESSAGE"
  direction: "INBOUND" | "OUTBOUND";
  msgSeqNum: number;
  msgType: string;
  msgName: string;
  rawFix: string; // pipe-delimited for display
  fields: FixField[];
  senderCompId: string;
  targetCompId: string;
  timestamp: string;
}

// A message in the UI store carries a client-side id and the measured
// server→browser latency.
export interface FeedMessage extends FixMessage {
  id: string;
  latencyMs: number | null;
}

export interface WsEvent {
  type: string;
  sessionId: string;
  timestamp: string;
  payload: any;
}

export type ConnectionStatus =
  | "WAITING_FOR_CONNECTION"
  | "TCP_CONNECTED"
  | "LOGON_RECEIVED"
  | "CONNECTED"
  | "DISCONNECTED"
  | "EXPIRED"
  | "DESTROYED"
  // Initiator-only states (phase 5, spec §8). LOGON_ACCEPTED is the
  // initiator's CONNECTED.
  | "CONNECTING"
  | "LOGON_SENT"
  | "LOGON_ACCEPTED"
  | "CONNECTION_FAILED";

// Order blotter record (spec §23, phase 3).
export interface PendingReplace {
  clOrdId: string;
  orderQty: number;
  price: number;
}

export interface Order {
  orderId: string;
  clOrdId: string;
  direction?: "INBOUND" | "OUTBOUND"; // phase 5: OUTBOUND = injected into remote
  symbol: string;
  side: string; // "1" = Buy, "2" = Sell
  orderQty: number;
  price: number;
  ordType: string;
  cumQty: number;
  leavesQty: number;
  avgPx: number;
  status:
    | "NEW"
    | "PARTIALLY_FILLED"
    | "FILLED"
    | "REJECTED"
    | "PENDING_CANCEL"
    | "CANCELED"
    | "REPLACED";
  prevStatus?: string;
  cancelReqClOrdId?: string;
  pendingReplace?: PendingReplace;
  createdAt: string;
  updatedAt: string;
}

export interface ExecutionSummary {
  msgType: string; // "8" | "9"
  execType?: string;
  ordStatus?: string;
  execId?: string;
  orderId: string;
  clOrdId: string;
  cumQty: number;
  leavesQty: number;
  avgPx: number;
  lastQty?: number;
  lastPx?: number;
  rawFix: string;
}

export interface ExecuteResult {
  order: Order;
  execution: ExecutionSummary;
}

export type ExecuteAction =
  | "FILL"
  | "PARTIAL_FILL"
  | "REJECT"
  | "CANCEL_ACCEPT"
  | "CANCEL_REJECT"
  | "REPLACE_ACCEPT"
  | "REPLACE_REJECT";

export interface ExecuteBody {
  action: ExecuteAction;
  qty?: number;
  price?: number;
  ordRejReason?: string;
  cxlRejReason?: string;
  text?: string;
}

// Outbound order injection (phase 5, spec §8).
export type InjectMsgType = "D" | "F" | "G";

export interface SendOrderBody {
  msgType: InjectMsgType;
  fields: Record<string, string>; // tag number -> value
}

export interface SendOrderResult {
  order: Order;
}

// Deterministic rule engine (spec §32/§33, phase 4).
export type PredicateOp = "eq" | "ne" | "gt" | "lt" | "contains";

export interface RulePredicate {
  tag: number;
  op: PredicateOp;
  value: string;
}

export type RuleActionType =
  | "ACK_NEW"
  | "FULL_FILL"
  | "PARTIAL_FILL"
  | "REJECT"
  | "DELAY";

export interface RuleAction {
  type: RuleActionType;
  delayMs?: number; // DELAY: 0..60000
  qty?: number; // PARTIAL_FILL: LastQty (32), > 0
  price?: number; // FULL_FILL override / PARTIAL_FILL LastPx (31), >= 0
  ordRejReason?: string; // REJECT: 103
  text?: string; // REJECT: 58
}

export interface Rule {
  id: string;
  name: string;
  priority: number;
  enabled: boolean;
  predicates: RulePredicate[];
  actions: RuleAction[];
  matchedCount: number;
  createdAt: string;
}

export interface RuleInput {
  name: string;
  priority: number;
  enabled: boolean;
  predicates: RulePredicate[];
  actions: RuleAction[];
}

// Stochastic simulation policy (phase 2.1, spec §48). Percentages must
// sum to 100. Seed 0 = random; any other seed reproduces the outcome
// sequence.
export interface StochasticConfig {
  enabled: boolean;
  acceptPct: number;
  rejectPct: number;
  partialFillPct: number;
  avgLatencyMs: number;
  stdDevMs: number;
  maxDelayMs: number;
  seed: number;
}

export interface StochasticState {
  config: StochasticConfig;
  outcomes: { fill: number; reject: number; partial: number };
}

// FIX decoder result (phase 6, POST /api/v1/tools/decode).
export interface DecodeResult {
  valid: boolean;
  msgType: string;
  msgName: string;
  beginString: string;
  fields: FixField[];
  checksumOk: boolean;
  expectedChecksum: string;
  actualChecksum: string;
  bodyLengthOk: boolean;
  expectedBodyLength: number;
  actualBodyLength: number;
}
