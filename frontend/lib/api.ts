// REST client for the FixLab backend (spec §37).
import type {
  DecodeResult,
  DictionaryInfo,
  ExecuteBody,
  ExecuteResult,
  FixMessage,
  Order,
  ReplayResult,
  Rule,
  RuleInput,
  Scenario,
  SendOrderBody,
  SendOrderResult,
  SessionInfo,
  StochasticConfig,
  StochasticState,
} from "@/types/fixlab";

export const API_URL =
  process.env.NEXT_PUBLIC_FIXLAB_API_URL ?? "http://localhost:8080";

export function wsUrl(token: string): string {
  return `${API_URL.replace(/^http/, "ws")}/ws/session/${token}`;
}

async function req<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_URL}${path}`, {
    ...init,
    headers: { "Content-Type": "application/json", ...(init?.headers ?? {}) },
  });
  if (!res.ok) {
    let detail = res.statusText;
    try {
      const body = await res.json();
      if (body?.error) detail = body.error;
    } catch {
      /* ignore */
    }
    throw new Error(detail);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export interface CreateSessionBody {
  fixVersion?: string;
  role?: string;
  transport?: string;
  tls?: boolean; // phase 7: JSON boolean, default false
  auth?: string;
  targetCompId?: string;
  // Initiator-only (phase 5, spec §8).
  remoteHost?: string;
  remotePort?: string;
  remoteCompId?: string;
  localSenderCompId?: string;
}

export function createSession(body: CreateSessionBody): Promise<SessionInfo> {
  return req<SessionInfo>("/api/v1/sessions", {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function getSession(token: string): Promise<SessionInfo> {
  return req<SessionInfo>(`/api/v1/sessions/${token}`);
}

export function getMessages(
  token: string,
  limit = 100
): Promise<{ messages: FixMessage[]; count: number; total: number }> {
  return req(`/api/v1/sessions/${token}/messages?limit=${limit}`);
}

export function deleteSession(token: string): Promise<void> {
  return req<void>(`/api/v1/sessions/${token}`, { method: "DELETE" });
}

export function getOrders(token: string): Promise<{ orders: Order[]; count: number }> {
  return req(`/api/v1/sessions/${token}/orders`);
}

export function executeOrder(
  token: string,
  clOrdId: string,
  body: ExecuteBody
): Promise<ExecuteResult> {
  return req<ExecuteResult>(
    `/api/v1/sessions/${token}/orders/${encodeURIComponent(clOrdId)}/execute`,
    { method: "POST", body: JSON.stringify(body) }
  );
}

// Deterministic rules (phase 4).
export function getRules(token: string): Promise<{ rules: Rule[]; count: number }> {
  return req(`/api/v1/sessions/${token}/rules`);
}

export function createRule(token: string, body: RuleInput): Promise<Rule> {
  return req<Rule>(`/api/v1/sessions/${token}/rules`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

export function updateRule(
  token: string,
  id: string,
  body: RuleInput
): Promise<Rule> {
  return req<Rule>(`/api/v1/sessions/${token}/rules/${encodeURIComponent(id)}`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

export function deleteRule(token: string, id: string): Promise<void> {
  return req<void>(
    `/api/v1/sessions/${token}/rules/${encodeURIComponent(id)}`,
    { method: "DELETE" }
  );
}

export function setKillSwitch(
  token: string,
  enabled: boolean
): Promise<{ enabled: boolean }> {
  return req<{ enabled: boolean }>(
    `/api/v1/sessions/${token}/killswitch`,
    { method: "POST", body: JSON.stringify({ enabled }) }
  );
}

// Stochastic simulation policy (phase 2.1, spec §48).
export function getStochastic(token: string): Promise<StochasticState> {
  return req<StochasticState>(`/api/v1/sessions/${token}/stochastic`);
}

export function updateStochastic(
  token: string,
  body: StochasticConfig
): Promise<StochasticState> {
  return req<StochasticState>(`/api/v1/sessions/${token}/stochastic`, {
    method: "PUT",
    body: JSON.stringify(body),
  });
}

// Scenario recording + replay (phase 2.3, spec §48): the session's
// application-level story as exportable JSON, and one-call replay into
// a fresh session.
export function getScenario(token: string): Promise<Scenario> {
  return req<Scenario>(`/api/v1/sessions/${token}/scenario`);
}

export function replayScenario(body: {
  sourceToken?: string;
  scenario?: Scenario;
  speed?: number;
}): Promise<ReplayResult> {
  return req<ReplayResult>(`/api/v1/sessions/replay`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}

// Stateless FIX decoder (phase 6, spec §36): no session, no account.
// Throws with the backend's specific message on malformed input (400).
// Pass a session token (phase 2.4) to decode with the session's ACTIVE
// dictionary — including an uploaded custom override.
export function decodeFix(
  rawFix: string,
  beginString?: string,
  token?: string
): Promise<DecodeResult> {
  return req<DecodeResult>("/api/v1/tools/decode", {
    method: "POST",
    body: JSON.stringify({ rawFix, beginString: beginString ?? "", token: token ?? "" }),
  });
}

// Custom FIX dictionary (phase 2.4, spec §48): per-session upload of a
// broker's QuickFIX data-dictionary XML. Ephemeral — dies with the
// session. Affects message inspection (WS feed, /messages, decode with
// token), not engine wire validation.
export function uploadDictionary(
  token: string,
  xml: string,
  name?: string
): Promise<DictionaryInfo> {
  return req<DictionaryInfo>(`/api/v1/sessions/${token}/dictionary`, {
    method: "POST",
    body: JSON.stringify({ xml, name: name ?? "" }),
  });
}

export function getDictionary(token: string): Promise<DictionaryInfo> {
  return req<DictionaryInfo>(`/api/v1/sessions/${token}/dictionary`);
}

export function clearDictionary(token: string): Promise<{ custom: boolean }> {
  return req<{ custom: boolean }>(`/api/v1/sessions/${token}/dictionary`, {
    method: "DELETE",
  });
}

// Outbound order injection (phase 5, spec §8): NewOrderSingle (D),
// OrderCancelRequest (F), OrderCancelReplaceRequest (G) into the remote
// counterparty on an INITIATOR session.
export function sendOrder(
  token: string,
  body: SendOrderBody
): Promise<SendOrderResult> {
  return req<SendOrderResult>(`/api/v1/sessions/${token}/send-order`, {
    method: "POST",
    body: JSON.stringify(body),
  });
}
