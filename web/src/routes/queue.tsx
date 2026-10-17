import { type KeyboardEvent, useState } from "react";
import { Link, useNavigate, useSearchParams } from "react-router-dom";

import { type CaseFilters, useCases } from "../api/queries";
import { StateView } from "../components/StateView";
import { age, formatAmount, formatScore, shortId } from "../format";

const SUMMARY_STATES = ["", "pending", "ok", "fallback", "unfaithful"] as const;

export default function Queue() {
  const [params, setParams] = useSearchParams();
  const [tokens, setTokens] = useState<string[]>([]); // page tokens of the pages before the current one
  const [cursor, setCursor] = useState(0);
  const navigate = useNavigate();
  const filters: CaseFilters = {
    status: params.get("status") === "labelled" ? "CASE_STATUS_LABELLED" : "CASE_STATUS_OPEN",
    reason_code: params.get("reason") ?? "",
    summary_status: (params.get("summary") ?? "") as CaseFilters["summary_status"],
    page_token: tokens[tokens.length - 1],
    page_size: 50,
  };
  const cases = useCases(filters);

  const setFilter = (k: string, v: string) => {
    const next = new URLSearchParams(params);
    if (v) next.set(k, v);
    else next.delete(k);
    setParams(next, { replace: true });
    setTokens([]);
    setCursor(0);
  };

  const onKey = (e: KeyboardEvent<HTMLTableSectionElement>, n: number, ids: string[]) => {
    if (e.key === "ArrowDown" || e.key === "j") setCursor((c) => Math.min(n - 1, c + 1));
    else if (e.key === "ArrowUp" || e.key === "k") setCursor((c) => Math.max(0, c - 1));
    else if (e.key === "Enter" && ids[cursor]) navigate(`/cases/${ids[cursor]}`);
    else return;
    e.preventDefault();
  };

  return (
    <section className="p-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <h1 className="text-xl font-semibold">
          Review queue{" "}
          {cases.data && (
            <span className="num text-base text-muted">({cases.data.total_open ?? 0} open)</span>
          )}
        </h1>
        <form
          className="flex flex-wrap gap-3 text-sm"
          onSubmit={(e) => e.preventDefault()}
          aria-label="Filters"
        >
          <label className="flex flex-col">
            Status
            <select
              value={params.get("status") ?? "open"}
              onChange={(e) => setFilter("status", e.target.value === "open" ? "" : e.target.value)}
              className="rounded border border-border bg-panel px-2 py-1"
            >
              <option value="open">Open</option>
              <option value="labelled">Labelled</option>
            </select>
          </label>
          <label className="flex flex-col">
            Reason code
            <input
              value={params.get("reason") ?? ""}
              onChange={(e) => setFilter("reason", e.target.value.toUpperCase().replace(/[^A-Z0-9_]/g, ""))}
              placeholder="e.g. MODEL_SCORE_ELEVATED"
              className="num rounded border border-border bg-panel px-2 py-1"
            />
          </label>
          <label className="flex flex-col">
            Summary
            <select
              value={params.get("summary") ?? ""}
              onChange={(e) => setFilter("summary", e.target.value)}
              className="rounded border border-border bg-panel px-2 py-1"
            >
              {SUMMARY_STATES.map((s) => (
                <option key={s} value={s}>
                  {s || "any"}
                </option>
              ))}
            </select>
          </label>
        </form>
      </div>
      <div className="mt-3 overflow-x-auto rounded border border-border bg-panel">
        <StateView
          query={cases}
          label="cases"
          isEmpty={(d) => !d.cases?.length}
          empty="No cases match these filters."
        >
          {(d) => {
            const list = d.cases ?? [];
            const ids = list.map((c) => c.case_id ?? "");
            return (
              <table className="w-full text-sm">
                <caption className="sr-only">Cases by priority. Arrow keys or j/k move, Enter opens.</caption>
                <thead className="text-left text-muted">
                  <tr>
                    <th className="px-3 py-2">Case</th>
                    <th className="px-3 py-2">Score</th>
                    <th className="px-3 py-2 text-right">Amount</th>
                    <th className="px-3 py-2">Reasons</th>
                    <th className="px-3 py-2">Age</th>
                    <th className="px-3 py-2">Summary</th>
                  </tr>
                </thead>
                <tbody
                  tabIndex={0}
                  onKeyDown={(e) => onKey(e, list.length, ids)}
                  aria-label="Cases"
                  className="focus-visible:outline-2"
                >
                  {list.map((c, i) => (
                    <tr
                      key={c.case_id}
                      aria-selected={i === cursor}
                      className={`row border-t border-border ${i === cursor ? "bg-panel-raised" : ""}`}
                    >
                      <td className="px-3 py-1">
                        <Link to={`/cases/${c.case_id}`} className="num text-accent underline">
                          {shortId(c.case_id)}
                        </Link>
                      </td>
                      <td className="num px-3 py-1">{formatScore(c.decision?.score)}</td>
                      <td className="num px-3 py-1 text-right">{formatAmount(c.amount_minor, c.currency)}</td>
                      <td className="num px-3 py-1 text-xs">{(c.decision?.reason_codes ?? []).join(", ")}</td>
                      <td className="num px-3 py-1">{age(c.created_at)}</td>
                      <td className="px-3 py-1">{c.summary_status}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            );
          }}
        </StateView>
      </div>
      <div className="mt-3 flex gap-2 text-sm">
        <button
          type="button"
          disabled={tokens.length === 0}
          onClick={() => setTokens((t) => t.slice(0, -1))}
          className="rounded border border-border px-3 py-1 disabled:opacity-50"
        >
          Previous page
        </button>
        <button
          type="button"
          disabled={!cases.data?.next_page_token}
          onClick={() =>
            cases.data?.next_page_token && setTokens((t) => [...t, cases.data!.next_page_token!])
          }
          className="rounded border border-border px-3 py-1 disabled:opacity-50"
        >
          Next page
        </button>
      </div>
    </section>
  );
}
