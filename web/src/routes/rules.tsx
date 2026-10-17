import { useState } from "react";

import { ApiError } from "../api/client";
import { useActiveRules, useTestRule } from "../api/queries";
import { can, getSession } from "../auth/session";
import { ActionBadge } from "../components/ActionBadge";
import { StateView } from "../components/StateView";
import { formatPercent, shortId, toNumber } from "../format";

function TestRule() {
  const [when, setWhen] = useState("");
  const t = useTestRule();
  const r = t.data;
  return (
    <section aria-labelledby="test-rule" className="mt-4 rounded border border-border bg-panel p-4">
      <h2 id="test-rule" className="font-semibold">
        Test rule
      </h2>
      <p className="text-sm text-muted">
        Evaluates an expression on recent decisions' features. It never changes the active rule set.
      </p>
      <form
        className="mt-2"
        onSubmit={(e) => {
          e.preventDefault();
          if (when.trim()) t.mutate({ when, sampleSize: 1000 });
        }}
      >
        <label htmlFor="rule-when" className="text-sm text-muted">
          Expression
        </label>
        <textarea
          id="rule-when"
          value={when}
          maxLength={500}
          onChange={(e) => setWhen(e.target.value)}
          rows={2}
          className="num mt-1 w-full rounded border border-border bg-bg p-2 text-sm"
          placeholder="new_device && minutes_since_email_change < 60"
        />
        <button
          type="submit"
          disabled={!when.trim() || t.isPending}
          className="mt-2 rounded bg-accent px-3 py-1 font-semibold text-on-accent disabled:opacity-50"
        >
          {t.isPending ? "Testing…" : "Run on 1,000 recent decisions"}
        </button>
      </form>
      {t.isError && (
        <p role="alert" className="mt-2 text-sm text-decline-text">
          {t.error instanceof ApiError && t.error.fields.length
            ? t.error.fields.map((f) => `${f.field}: ${f.description}`).join("; ")
            : (t.error as Error).message}
        </p>
      )}
      {r && !r.valid && (
        <p role="alert" className="num mt-2 text-sm text-decline-text">
          Invalid expression: {r.error}
        </p>
      )}
      {r?.valid && (
        <p className="num mt-2 text-sm" aria-live="polite">
          Matched {toNumber(r.matched)} of {toNumber(r.evaluated)} ({formatPercent(r.match_rate)})
          {r.sample_decision_ids?.length ? ` · e.g. ${r.sample_decision_ids.map(shortId).join(", ")}` : ""}
        </p>
      )}
    </section>
  );
}

export default function Rules() {
  const q = useActiveRules();
  const session = getSession();
  return (
    <section className="p-4">
      <h1 className="text-xl font-semibold">Rules</h1>
      <StateView
        query={q}
        label="the active rule set"
        isEmpty={(d) => !d.rules?.length}
        empty="The active rule set has no rules."
      >
        {(rs) => (
          <>
            <p className="num mt-1 text-sm text-muted">
              version {rs.version} · sha256 {rs.sha256?.slice(0, 12)} · feature spec {rs.feature_spec} ·
              review ≥ {rs.thresholds?.review} · decline ≥ {rs.thresholds?.decline}
            </p>
            <div
              role="region"
              aria-label="Active rules"
              tabIndex={0}
              className="mt-3 overflow-x-auto rounded border border-border bg-panel"
            >
              <table className="w-full text-sm">
                <thead className="text-left text-muted">
                  <tr>
                    <th className="px-3 py-2">ID</th>
                    <th className="px-3 py-2">Name</th>
                    <th className="px-3 py-2">When</th>
                    <th className="px-3 py-2">Action</th>
                    <th className="px-3 py-2">Reason code</th>
                    <th className="px-3 py-2">Final</th>
                  </tr>
                </thead>
                <tbody>
                  {(rs.rules ?? []).map((r) => (
                    <tr key={r.id} className="row border-t border-border">
                      <td className="num px-3 py-1">{r.id}</td>
                      <td className="px-3 py-1">{r.name}</td>
                      <td className="num px-3 py-1 text-xs">{r.when}</td>
                      <td className="px-3 py-1">
                        <ActionBadge action={r.action} />
                      </td>
                      <td className="num px-3 py-1 text-xs">{r.reason_code}</td>
                      <td className="px-3 py-1">{r.final ? "yes" : "no"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </StateView>
      {can("testRule", session) && <TestRule />}
    </section>
  );
}
