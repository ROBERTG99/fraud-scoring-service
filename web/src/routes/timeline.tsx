import { Link, useParams } from "react-router-dom";

import { useTimeline } from "../api/queries";
import { StateView } from "../components/StateView";
import { formatAmount, shortId } from "../format";

const KIND_TEXT: Record<string, string> = {
  "event.login": "Login",
  "event.device": "Device",
  "event.transaction": "Transaction",
  decision: "Decision",
  label: "Label",
};

export default function Timeline() {
  const { accountId = "" } = useParams();
  const q = useTimeline(accountId);
  return (
    <section className="p-4">
      <h1 className="text-xl font-semibold">
        Account timeline <span className="num">{accountId}</span>
      </h1>
      <div className="mt-3 rounded border border-border bg-panel">
        <StateView
          query={q}
          label="timeline"
          isEmpty={(d) => !d.items?.length}
          empty="No events, decisions or labels for this account."
        >
          {(d) => (
            <ol aria-label="Timeline, newest first">
              {(d.items ?? []).map((it) => (
                <li
                  key={`${it.kind}-${it.id}`}
                  className="row flex flex-wrap items-baseline gap-3 border-t border-border px-3 py-1 first:border-t-0"
                >
                  <time className="num w-44 text-xs text-muted" dateTime={it.ts}>
                    {it.ts?.replace("T", " ").slice(0, 19)}
                  </time>
                  <span className="w-24 text-sm font-semibold">{KIND_TEXT[it.kind ?? ""] ?? it.kind}</span>
                  <span className="text-sm">{it.title}</span>
                  {/* amount_minor is in minor units: format it, never show it as major units. */}
                  {it.details?.amount_minor !== undefined && (
                    <span className="num whitespace-nowrap text-sm">
                      {formatAmount(it.details.amount_minor, it.details.currency)}
                    </span>
                  )}
                  <span className="num min-w-0 break-all text-xs text-muted">
                    {Object.entries(it.details ?? {})
                      .filter(([k]) => k !== "amount_minor" && k !== "currency")
                      .map(([k, v]) => `${k}=${v}`)
                      .join(" ")}
                  </span>
                  {it.details?.case_id && (
                    <Link to={`/cases/${it.details.case_id}`} className="text-sm text-accent underline">
                      Open case <span className="num">{shortId(it.details.case_id)}</span>
                    </Link>
                  )}
                </li>
              ))}
            </ol>
          )}
        </StateView>
      </div>
    </section>
  );
}
