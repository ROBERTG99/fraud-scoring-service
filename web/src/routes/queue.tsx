import { type KeyboardEvent, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import { type CaseFilters, useCases } from "../api/queries";
import { StateView } from "../components/StateView";
import { age, formatAmount, formatScore, shortId } from "../format";

const SUMMARY_STATES = ["", "pending", "ok", "fallback", "unfaithful"] as const;

export default function Queue() {
  const [params, setParams] = useSearchParams();
  const [tokens, setTokens] = useState<string[]>([]); // page tokens of the pages before the current one
  // The cursor is a case ID, not a row number: the list refetches every 15 s and can reorder.
  const [cursor, setCursor] = useState<string | null>(null);
  const links = useRef(new Map<string, HTMLAnchorElement>());
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
    setCursor(null);
  };

  // Roving tabindex: the list is one Tab stop (the current row's case link); arrows or j/k move it and
  // focus follows, so screen readers announce the row. Enter is the link's own activation.
  const onKey = (e: KeyboardEvent<HTMLTableSectionElement>, ids: string[], current: number) => {
    let next: number;
    if (e.key === "ArrowDown" || e.key === "j") next = Math.min(ids.length - 1, current + 1);
    else if (e.key === "ArrowUp" || e.key === "k") next = Math.max(0, current - 1);
    else return;
    e.preventDefault();
    const id = ids[next]!;
    setCursor(id);
    links.current.get(id)?.focus();
  };
  const page = (t: string[]) => {
    setTokens(t);
    setCursor(null);
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
              className="rounded border border-field-border bg-panel px-2 py-1"
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
              className="num rounded border border-field-border bg-panel px-2 py-1"
            />
          </label>
          <label className="flex flex-col">
            Summary
            <select
              value={params.get("summary") ?? ""}
              onChange={(e) => setFilter("summary", e.target.value)}
              className="rounded border border-field-border bg-panel px-2 py-1"
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
            // A case that left the list (labelled elsewhere) falls back to the first row.
            const current = Math.max(0, ids.indexOf(cursor ?? ""));
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
                <tbody onKeyDown={(e) => onKey(e, ids, current)} aria-label="Cases">
                  {list.map((c, i) => (
                    <tr
                      key={c.case_id}
                      className={`row border-t border-border ${i === current ? "bg-panel-raised outline-2 -outline-offset-2 outline-accent" : ""}`}
                    >
                      <td className="px-3 py-1">
                        <Link
                          ref={(el) => {
                            if (el) links.current.set(ids[i]!, el);
                            else links.current.delete(ids[i]!);
                          }}
                          to={`/cases/${c.case_id}`}
                          tabIndex={i === current ? 0 : -1}
                          onFocus={() => setCursor(ids[i]!)}
                          aria-label={`Open case ${shortId(c.case_id)}`}
                          className="num text-accent underline"
                        >
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
          onClick={() => page(tokens.slice(0, -1))}
          className="rounded border border-border px-3 py-1 disabled:opacity-50"
        >
          Previous page
        </button>
        <button
          type="button"
          disabled={!cases.data?.next_page_token}
          onClick={() => cases.data?.next_page_token && page([...tokens, cases.data.next_page_token])}
          className="rounded border border-border px-3 py-1 disabled:opacity-50"
        >
          Next page
        </button>
      </div>
    </section>
  );
}
