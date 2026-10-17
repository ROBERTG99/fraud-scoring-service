import { Link, useParams } from "react-router-dom";

import { useTimeline } from "../api/queries";
import { StateView } from "../components/StateView";

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
                  <span className="num text-xs text-muted">
                    {Object.entries(it.details ?? {})
                      .map(([k, v]) => `${k}=${v}`)
                      .join(" ")}
                  </span>
                  {it.details?.case_id && (
                    <Link to={`/cases/${it.details.case_id}`} className="text-sm text-accent underline">
                      case
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
