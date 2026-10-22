import type { ReactNode } from "react";

import { ApiError } from "../api/client";

interface Props<T> {
  query: {
    isPending: boolean;
    isError: boolean;
    error: unknown;
    data: T | undefined;
    refetch: () => unknown;
  };
  isEmpty?: (data: T) => boolean;
  empty?: ReactNode;
  label: string;
  children: (data: T) => ReactNode;
}

/** Loading, empty, error-with-retry and success states for one query. */
export function StateView<T>({ query, isEmpty, empty, label, children }: Props<T>) {
  if (query.isPending) {
    return (
      <div role="status" aria-live="polite" className="p-6 text-muted">
        Loading {label}…
      </div>
    );
  }
  if (query.isError) {
    const status = query.error instanceof ApiError ? ` (${query.error.status})` : "";
    const msg = query.error instanceof Error ? query.error.message : "request failed";
    return (
      <div role="alert" className="m-4 rounded border border-decline bg-panel p-4">
        <p className="font-semibold text-decline-text">
          Could not load {label}
          {status}
        </p>
        <p className="mt-1 text-sm text-muted">{msg}</p>
        <button
          type="button"
          onClick={() => void query.refetch()}
          className="mt-3 rounded bg-accent px-3 py-1 text-sm font-semibold text-on-accent"
        >
          Retry
        </button>
      </div>
    );
  }
  const data = query.data as T;
  if (isEmpty?.(data)) {
    return <div className="p-6 text-muted">{empty ?? `No ${label}.`}</div>;
  }
  return <>{children(data)}</>;
}
