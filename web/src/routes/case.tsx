import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useCase } from "../api/queries";
import { getSession, can } from "../auth/session";
import { ActionBadge } from "../components/ActionBadge";
import { AiSummary } from "../components/AiSummary";
import { LabelDialog } from "../components/LabelDialog";
import { ReasonChips } from "../components/ReasonChips";
import { StateView } from "../components/StateView";
import { formatAmount, formatScore, shortId } from "../format";

export default function CaseDetail() {
  const { caseId = "" } = useParams();
  // After a regenerate request, poll until the summary changes (or for at most a minute).
  const [watching, setWatching] = useState<{ from: string | undefined; until: number } | null>(null);
  const query = useCase(caseId, watching !== null);
  const summaryAt = query.data?.summary?.created_at;
  useEffect(() => {
    // A fixed deadline: failed refetches during an outage must not keep the 3 s polling alive.
    if (watching && (summaryAt !== watching.from || Date.now() > watching.until)) setWatching(null);
  }, [watching, summaryAt, query.dataUpdatedAt, query.errorUpdatedAt]);
  const session = getSession();
  const [highlighted, setHighlighted] = useState<string | null>(null);
  const [labelling, setLabelling] = useState(false);
  const [returnFocus, setReturnFocus] = useState(false);
  const labelButton = useRef<HTMLButtonElement>(null);
  const labelText = useRef<HTMLSpanElement>(null);
  const factPanel = useRef<HTMLElement>(null);

  // After the dialog closes, focus goes back to "Label case", or to the label text once saved
  // (the mutation stores the labelled case before it closes the dialog).
  useEffect(() => {
    const target = labelText.current ?? labelButton.current;
    if (!returnFocus || !target) return;
    target.focus();
    setReturnFocus(false);
  }, [returnFocus, query.data?.status]);

  const closeDialog = () => {
    setLabelling(false);
    setReturnFocus(true);
  };

  // An evidence item cites a fact that may be off-screen (the panels stack on narrow screens).
  const showEvidence = (key: string) => {
    setHighlighted(key);
    const rows = factPanel.current?.querySelectorAll<HTMLElement>("[data-fact]") ?? [];
    const row = [...rows].find((r) => r.dataset.fact === key);
    row?.scrollIntoView({ block: "nearest" });
    row?.focus({ preventScroll: true });
  };
  return (
    <section className="p-4">
      <p className="text-sm">
        <Link to="/queue" className="text-accent underline">
          ← Queue
        </Link>
      </p>
      <StateView query={query} label="case">
        {(c) => {
          const facts = c.facts ?? [];
          const features = facts.filter((f) => f.key?.startsWith("feature."));
          const other = facts.filter((f) => !f.key?.startsWith("feature."));
          const labelled = c.status === "CASE_STATUS_LABELLED";
          return (
            <>
              <header className="mt-2 flex flex-wrap items-center gap-3">
                <h1 className="text-xl font-semibold">
                  Case <span className="num">{shortId(c.case_id)}</span>
                </h1>
                <ActionBadge action={c.decision?.action} />
                <span className="num">score {formatScore(c.decision?.score)}</span>
                <span className="num">{formatAmount(c.amount_minor, c.currency)}</span>
                <Link to={`/accounts/${c.account_id}`} className="num text-accent underline">
                  {c.account_id}
                </Link>
                {c.decision?.degraded && (
                  <span className="rounded border border-review px-1.5 text-xs text-review-text">
                    degraded decision
                  </span>
                )}
                <div className="ml-auto">
                  {labelled ? (
                    <span ref={labelText} tabIndex={-1} className="text-sm">
                      Labelled <strong>{c.label?.label === "LABEL_FRAUD" ? "fraud" : "legitimate"}</strong> by{" "}
                      <span className="num">{c.label?.analyst_sub}</span>
                    </span>
                  ) : (
                    can("label", session) &&
                    session && (
                      <button
                        ref={labelButton}
                        type="button"
                        onClick={() => setLabelling(true)}
                        className="rounded bg-accent px-3 py-1 font-semibold text-on-accent"
                      >
                        Label case
                      </button>
                    )
                  )}
                </div>
              </header>
              <div className="mt-2">
                <ReasonChips codes={c.decision?.reason_codes} />
              </div>
              <p className="num mt-1 text-xs text-muted">
                model {c.decision?.model_version} · rules {c.decision?.rules_version} · decided{" "}
                {c.decision?.decided_at}
              </p>
              <div className="mt-4 grid gap-4 lg:grid-cols-2">
                <AiSummary
                  c={c}
                  session={session}
                  highlighted={highlighted}
                  onEvidence={showEvidence}
                  onRegenerated={() => setWatching({ from: summaryAt, until: Date.now() + 60_000 })}
                />
                <section
                  ref={factPanel}
                  aria-labelledby="facts"
                  className="min-w-0 rounded border border-border bg-panel p-4"
                >
                  <h2 id="facts" className="font-semibold">
                    Feature snapshot and facts
                  </h2>
                  {[
                    ["Features", features],
                    ["Decision and events", other],
                  ].map(([title, list]) => (
                    <div key={title as string}>
                      <h3 className="mt-3 text-sm text-muted">{title as string}</h3>
                      <table className="mt-1 w-full table-fixed text-sm">
                        <tbody>
                          {(list as typeof facts).map((f) => (
                            <tr
                              key={f.key}
                              data-fact={f.key}
                              tabIndex={-1}
                              aria-current={highlighted === f.key ? "true" : undefined}
                              className={`row border-t border-border ${highlighted === f.key ? "bg-panel-raised outline outline-1 outline-accent" : ""}`}
                            >
                              <th
                                scope="row"
                                className="num w-1/2 break-all px-2 text-left font-normal text-muted"
                              >
                                {f.key}
                              </th>
                              <td className="num break-all px-2 text-right">{f.value}</td>
                            </tr>
                          ))}
                        </tbody>
                      </table>
                    </div>
                  ))}
                </section>
              </div>
              {labelling && session && (
                <LabelDialog caseId={c.case_id ?? ""} session={session} onClose={closeDialog} />
              )}
            </>
          );
        }}
      </StateView>
    </section>
  );
}
