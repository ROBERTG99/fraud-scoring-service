import { useMutation, useQuery } from "@tanstack/react-query";
import { useNavigate, useSearchParams } from "react-router-dom";

import { type Persona, fetchPersonas, signIn } from "../auth/session";
import { StateView } from "../components/StateView";

const ROLE_TEXT: Record<string, string> = {
  analyst: "Reviews cases and writes labels",
  risk_engineer: "Tests rules, reads shadow results",
  ml_engineer: "Reads model and shadow results",
};

export function safeNext(next: string | null): string {
  // Only same-origin paths: never redirect to another site after sign-in. Browsers read "\\" as "/" and
  // strip tabs and newlines from URLs, so either could turn "/x" into "//host"; reject them outright.
  return next &&
    next.startsWith("/") &&
    !next.startsWith("//") &&
    !next.startsWith("/sign-in") &&
    // eslint-disable-next-line no-control-regex
    !/[\\\u0000-\u001f\u007f]/.test(next)
    ? next
    : "/queue";
}

export default function SignIn() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const personas = useQuery({ queryKey: ["personas"], queryFn: () => fetchPersonas() });
  const login = useMutation({
    mutationFn: (p: Persona) => signIn(p.id),
    onSuccess: () => navigate(safeNext(params.get("next")), { replace: true }),
  });
  return (
    <main className="mx-auto mt-16 max-w-md px-4">
      <h1 className="text-2xl font-semibold">Fraud review console</h1>
      <p className="mt-2 text-sm text-muted">
        Development sign-in: pick a synthetic persona. Tokens come from the local dev-issuer.
      </p>
      <section aria-label="Personas" className="mt-6 rounded border border-border bg-panel">
        <StateView
          query={personas}
          label="personas"
          isEmpty={(d) => d.length === 0}
          empty="The dev-issuer has no personas."
        >
          {(list) => (
            <ul>
              {list.map((p) => (
                <li key={p.id} className="border-b border-border last:border-b-0">
                  <button
                    type="button"
                    disabled={login.isPending}
                    onClick={() => login.mutate(p)}
                    className="flex w-full items-center justify-between px-4 py-3 text-left hover:bg-panel-raised disabled:opacity-60"
                  >
                    <span>
                      <span className="block font-semibold">{p.name}</span>
                      <span className="block text-sm text-muted">{ROLE_TEXT[p.role] ?? p.role}</span>
                    </span>
                    <span className="num text-xs text-muted">{p.role}</span>
                  </button>
                </li>
              ))}
            </ul>
          )}
        </StateView>
      </section>
      {login.isError && (
        <p role="alert" className="mt-3 text-sm text-decline-text">
          Sign-in failed: {(login.error as Error).message}
        </p>
      )}
    </main>
  );
}
