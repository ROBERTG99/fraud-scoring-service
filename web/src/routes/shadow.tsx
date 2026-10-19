import { useState } from "react";

import { useShadowSummary } from "../api/queries";
import { can, getSession } from "../auth/session";
import { StateView } from "../components/StateView";
import { formatPercent, formatScore, shortId, toNumber } from "../format";

const BANDS = ["APPROVE", "REVIEW", "DECLINE"];

export default function Shadow() {
  const [hours, setHours] = useState(24);
  const allowed = can("shadow", getSession());
  const q = useShadowSummary(hours, allowed);
  if (!allowed) {
    return (
      <section className="p-4">
        <h1 className="text-xl font-semibold">Model and shadow</h1>
        <p className="mt-3 text-muted">This screen is for risk and ML engineers.</p>
      </section>
    );
  }
  return (
    <section className="p-4">
      <div className="flex flex-wrap items-end justify-between gap-3">
        <h1 className="text-xl font-semibold">Model and shadow</h1>
        <label className="flex flex-col text-sm">
          Window
          <select
            value={hours}
            onChange={(e) => setHours(Number(e.target.value))}
            className="rounded border border-field-border bg-panel px-2 py-1"
          >
            <option value={24}>24 hours</option>
            <option value={168}>7 days</option>
            <option value={720}>30 days</option>
          </select>
        </label>
      </div>
      <StateView
        query={q}
        label="shadow comparisons"
        isEmpty={(d) => toNumber(d.total) === 0}
        empty="No shadow comparisons in this window."
      >
        {(d) => {
          const cell = (p: string, s: string) =>
            toNumber(d.band_matrix?.find((c) => c.primary_band === p && c.shadow_band === s)?.count);
          return (
            <div className="mt-3 grid gap-4 lg:grid-cols-2">
              <div className="rounded border border-border bg-panel p-4">
                <p className="text-sm text-muted">Agreement</p>
                <p className="num text-3xl font-semibold">{formatPercent(d.agreement_rate)}</p>
                <p className="num mt-1 text-xs text-muted">
                  {toNumber(d.total)} compared · primary {d.primary_version} · shadow {d.shadow_version} ·{" "}
                  {toNumber(d.dropped)} dropped (queue full)
                </p>
                <table className="num mt-4 w-full text-sm">
                  <caption className="text-left text-sm text-muted">
                    Band changes: rows primary, columns shadow
                  </caption>
                  <thead>
                    <tr>
                      <th scope="col" className="text-left text-muted">
                        primary \ shadow
                      </th>
                      {BANDS.map((b) => (
                        <th key={b} scope="col" className="text-right">
                          {b}
                        </th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {BANDS.map((p) => (
                      <tr key={p} className="row border-t border-border">
                        <th scope="row" className="text-left">
                          {p}
                        </th>
                        {BANDS.map((s) => (
                          <td
                            key={s}
                            className={`text-right ${p !== s && cell(p, s) > 0 ? "font-semibold text-review-text" : ""}`}
                          >
                            {cell(p, s)}
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
              <div className="rounded border border-border bg-panel p-4">
                <h2 className="font-semibold">Disagreements</h2>
                {d.disagreements?.length ? (
                  <table className="num mt-2 w-full text-sm">
                    <thead className="text-left text-muted">
                      <tr>
                        <th>Decision</th>
                        <th>Primary</th>
                        <th>Shadow</th>
                      </tr>
                    </thead>
                    <tbody>
                      {d.disagreements.map((x) => (
                        <tr key={x.decision_id} className="row border-t border-border">
                          <td>{shortId(x.decision_id)}</td>
                          <td>
                            {x.primary_band} {formatScore(x.primary_score)}
                          </td>
                          <td>
                            {x.shadow_band} {formatScore(x.shadow_score)}
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                ) : (
                  <p className="mt-2 text-sm text-muted">
                    The versions agree on every decision in this window.
                  </p>
                )}
              </div>
            </div>
          );
        }}
      </StateView>
    </section>
  );
}
