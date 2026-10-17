import { type Case, newIdempotencyKey, useRegenerateSummary } from "../api/queries";
import { type Session, can } from "../auth/session";
import { formatPercent } from "../format";

interface Props {
  c: Case;
  session: Session | null;
  highlighted: string | null;
  onEvidence: (factKey: string) => void;
}

const STATUS_TEXT: Record<string, string> = {
  pending: "Summary pending: the worker writes it asynchronously.",
  fallback: "AI summary unavailable: showing a templated fact list.",
  unfaithful: "The model's summary failed the faithfulness check and was withheld.",
};

/** The asynchronous LLM summary, clearly separated from the decision (spec §13 screen 3). */
export function AiSummary({ c, session, highlighted, onEvidence }: Props) {
  const regenerate = useRegenerateSummary();
  const s = c.summary;
  const status = c.summary_status ?? "pending";
  return (
    <section aria-labelledby="ai-summary" className="min-w-0 rounded border border-border bg-panel p-4">
      <div className="flex flex-wrap items-baseline justify-between gap-2">
        <h2 id="ai-summary" className="font-semibold">
          AI summary · does not affect decisions
        </h2>
        {can("regenerate", session) && (
          <button
            type="button"
            disabled={regenerate.isPending}
            onClick={() =>
              regenerate.mutate({ caseId: c.case_id ?? "", idempotencyKey: newIdempotencyKey() })
            }
            className="rounded border border-border px-2 py-0.5 text-sm"
          >
            {regenerate.isPending ? "Requesting…" : "Regenerate"}
          </button>
        )}
      </div>
      {regenerate.isError && (
        <p role="alert" className="text-sm text-decline-text">
          Regenerate failed: {(regenerate.error as Error).message}
        </p>
      )}
      {STATUS_TEXT[status] && <p className="mt-2 text-sm text-review-text">{STATUS_TEXT[status]}</p>}
      {s && (
        <>
          <dl className="mt-2 grid grid-cols-[auto_1fr] gap-x-3 text-sm">
            <dt className="text-muted">Pattern</dt>
            <dd>{s.pattern}</dd>
            <dt className="text-muted">Model</dt>
            <dd className="num">{s.model}</dd>
            <dt className="text-muted">Prompt</dt>
            <dd className="num">{s.prompt_version}</dd>
            <dt className="text-muted">Faithfulness</dt>
            <dd className="num">{formatPercent(s.faithfulness, 0)}</dd>
            <dt className="text-muted">Confidence</dt>
            <dd className="num">{formatPercent(s.confidence, 0)}</dd>
          </dl>
          <h3 className="mt-3 text-sm text-muted">Evidence (select to highlight the fact)</h3>
          <ul className="mt-1 space-y-1">
            {(s.evidence ?? []).map((e) => (
              <li key={`${e.fact_key}-${e.statement}`}>
                <button
                  type="button"
                  aria-pressed={highlighted === e.fact_key}
                  onClick={() => onEvidence(e.fact_key ?? "")}
                  className={`w-full rounded border px-2 py-1 text-left text-sm ${highlighted === e.fact_key ? "border-accent" : "border-border"}`}
                >
                  <span className="num block text-xs text-muted">
                    {e.fact_key} · {e.verified ? "verified" : "unverified"}
                  </span>
                  {e.statement}
                </button>
              </li>
            ))}
          </ul>
          {!!s.recommended_checks?.length && (
            <>
              <h3 className="mt-3 text-sm text-muted">Recommended checks</h3>
              <ul className="mt-1 list-disc pl-5 text-sm">
                {s.recommended_checks.map((r) => (
                  <li key={r}>{r}</li>
                ))}
              </ul>
            </>
          )}
        </>
      )}
    </section>
  );
}
