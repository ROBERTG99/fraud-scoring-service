// The only HTTP client for the scorer API: generated types (schema.d.ts) + bearer token + 401 handling.
import createClient, { type Middleware } from "openapi-fetch";

import { clearSession, getSession } from "../auth/session";
import type { paths } from "./schema";

export class ApiError extends Error {
  readonly status: number;
  readonly fields: { field: string; description: string }[];

  constructor(status: number, message: string, fields: { field: string; description: string }[] = []) {
    super(message);
    this.status = status;
    this.fields = fields;
  }
}

interface RpcStatus {
  message?: string;
  details?: { field_violations?: { field?: string; description?: string }[] }[];
}

export function toApiError(status: number, body: unknown): ApiError {
  const s = (body ?? {}) as RpcStatus;
  const fields = (s.details ?? []).flatMap((d) =>
    (d.field_violations ?? []).map((v) => ({ field: v.field ?? "", description: v.description ?? "" })),
  );
  return new ApiError(status, s.message || `request failed (${status})`, fields);
}

export function signInPath(location: Pick<Location, "pathname" | "search">): string {
  const next = location.pathname + location.search;
  return next.startsWith("/sign-in") ? "/sign-in" : `/sign-in?next=${encodeURIComponent(next)}`;
}

export interface ClientOptions {
  baseUrl?: string;
  fetch?: typeof fetch;
  onUnauthorized?: () => void;
}

export function createApi(opts: ClientOptions = {}) {
  const client = createClient<paths>({ baseUrl: opts.baseUrl ?? "/api", fetch: opts.fetch });
  const onUnauthorized = opts.onUnauthorized ?? (() => window.location.assign(signInPath(window.location)));
  const auth: Middleware = {
    onRequest({ request }) {
      const s = getSession();
      if (s) request.headers.set("Authorization", `Bearer ${s.token}`);
      return request;
    },
    onResponse({ response }) {
      if (response.status === 401) {
        clearSession();
        onUnauthorized();
      }
      return response;
    },
  };
  client.use(auth);
  return client;
}

export type Api = ReturnType<typeof createApi>;

/** Unwraps an openapi-fetch result or throws ApiError. */
export function unwrap<T>(r: { data?: T; error?: unknown; response: Response }): T {
  if (r.error !== undefined || !r.response.ok) throw toApiError(r.response.status, r.error);
  return r.data as T;
}

export const api = createApi();
