import { useState } from "react";
import { Link, useParams } from "react-router-dom";

import { useCase } from "../api/queries";
import { getSession, can } from "../auth/session";
import { ActionBadge } from "../components/ActionBadge";
import { AiSummary } from "../components/AiSummary";
import { LabelDialog } from "../components/LabelDialog";
import { ReasonChips } from "../components/ReasonChips";
import { StateView } from "../components/StateView";
import { formatAmount, formatScore } from "../format";

export default function CaseDetail() {
  const { caseId = "" } = useParams();
  const query = useCase(caseId);
  const session = getSession();
  const [highlighted, setHighlighted] = useState<string | null>(null);
  const [labelling, setLabelling] = useState(false);
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
                  Case <span className="num">{c.case_id?.slice(0, 8)}</span>
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
                    <span className="text-sm">
                      Labelled <strong>{c.label?.label === "LABEL_FRAUD" ? "fraud" : "legitimate"}</strong> by{" "}
                      <span className="num">{c.label?.analyst_sub}</span>
                    </span>
                  ) : (
                    can("label", session) &&
                    session && (
                      <button
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
                <AiSummary c={c} session={session} highlighted={highlighted} onEvidence={setHighlighted} />
                <section
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
                <LabelDialog caseId={c.case_id ?? ""} session={session} onClose={() => setLabelling(false)} />
              )}
            </>
          );
        }}
      </StateView>
    </section>
  );
}
