// Console session: a dev-issuer token for one persona, kept per browser tab (sessionStorage).

export type Role = "analyst" | "risk_engineer" | "ml_engineer";

export interface Persona {
  id: string;
  name: string;
  role: Role;
}

export interface Session {
  token: string;
  persona: Persona;
  expiresAt: number; // epoch ms
}

const KEY = "fraud.session";

function storage(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

let memory: Session | null = null;

export function getSession(now = Date.now()): Session | null {
  let s = memory;
  try {
    const raw = storage()?.getItem(KEY);
    if (raw) s = JSON.parse(raw) as Session;
  } catch {
    // storage unavailable or corrupt: fall back to the in-memory copy
  }
  if (s && s.expiresAt <= now) {
    clearSession();
    return null;
  }
  return s;
}

export function setSession(s: Session): void {
  memory = s;
  try {
    storage()?.setItem(KEY, JSON.stringify(s));
  } catch {
    // keep the in-memory copy only
  }
}

export function clearSession(): void {
  memory = null;
  try {
    storage()?.removeItem(KEY);
  } catch {
    // nothing to clear
  }
}

// Mirrors internal/auth/rbac.go; the server enforces it, the console only hides what would be refused.
const CAN: Record<string, readonly Role[]> = {
  label: ["analyst"],
  testRule: ["risk_engineer"],
  shadow: ["risk_engineer", "ml_engineer"],
  regenerate: ["analyst", "risk_engineer"],
};

export function can(action: keyof typeof CAN, session: Session | null): boolean {
  return !!session && (CAN[action] ?? []).includes(session.persona.role);
}

export async function fetchPersonas(fetchImpl: typeof fetch = fetch): Promise<Persona[]> {
  const resp = await fetchImpl("/auth/personas");
  if (!resp.ok) throw new Error(`dev-issuer: ${resp.status}`);
  const body = (await resp.json()) as { personas: Persona[] };
  return body.personas;
}

export async function signIn(
  persona: string,
  fetchImpl: typeof fetch = fetch,
  now = Date.now(),
): Promise<Session> {
  const resp = await fetchImpl("/auth/token", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ persona }),
  });
  if (!resp.ok) throw new Error(`sign-in failed: ${resp.status}`);
  const body = (await resp.json()) as { access_token: string; expires_in: number; persona: Persona };
  const s: Session = {
    token: body.access_token,
    persona: body.persona,
    expiresAt: now + body.expires_in * 1000,
  };
  setSession(s);
  return s;
}
