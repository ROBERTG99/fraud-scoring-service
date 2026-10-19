import { useLiveMetrics } from "../api/live";
import { Sparkline } from "../components/Sparkline";
import { StatTile } from "../components/StatTile";
import { formatPercent, toNumber } from "../format";

const STATUS_TEXT = { connecting: "Connecting…", live: "Live", reconnecting: "Reconnecting" } as const;

export default function Live({ retryBaseMs = 1_000 }: { retryBaseMs?: number }) {
  const s = useLiveMetrics(retryBaseMs);
  const m = s.latest;
  const times = s.history.map((h) => (h.ts ? new Date(h.ts).toISOString().slice(11, 19) : ""));
  const approve = toNumber(m?.approve);
  const review = toNumber(m?.review);
  const decline = toNumber(m?.decline);
  const total = approve + review + decline;
  return (
    <section className="p-4">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">Live metrics</h1>
        <p
          role="status"
          aria-live="polite"
          className={`text-sm ${s.status === "live" ? "text-approve-text" : "text-review-text"}`}
        >
          {STATUS_TEXT[s.status]}
          {s.status === "reconnecting" && s.error ? ` (attempt ${s.attempts}: ${s.error})` : ""}
        </p>
        <p className="text-xs text-muted">last 10 s window, pushed every 2 s</p>
      </div>
      {!m ? (
        <p className="mt-6 text-muted">
          {s.status === "reconnecting"
            ? "No data yet: the stream is reconnecting."
            : "Connecting to the live stream…"}
        </p>
      ) : (
        <>
          {total === 0 && <p className="mt-4 text-muted">No decisions in the last 10 s window.</p>}
          <div className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
            <StatTile label="Requests per second" value={(m.rps ?? 0).toFixed(1)}>
              <Sparkline
                title="Requests per second"
                values={s.history.map((h) => h.rps ?? 0)}
                labels={times}
                format={(v) => v.toFixed(1)}
              />
            </StatTile>
            <StatTile label="Score latency p95" value={`${(m.p95_ms ?? 0).toFixed(1)} ms`} sub="budget 45 ms">
              <Sparkline
                title="Score latency p95 (ms)"
                values={s.history.map((h) => h.p95_ms ?? 0)}
                labels={times}
                format={(v) => `${v.toFixed(1)} ms`}
              />
            </StatTile>
            <StatTile
              label="Degraded decisions"
              value={formatPercent(m.degraded_rate)}
              sub={`model unavailable ${toNumber(m.model_unavailable)} · features unavailable ${toNumber(m.features_unavailable)}`}
            />
            <StatTile label="Action mix" value={`${total} decisions`}>
              <ul className="num text-sm">
                <li>
                  <span className="text-approve-text">APPROVE</span> {approve} (
                  {formatPercent(total ? approve / total : 0)})
                </li>
                <li>
                  <span className="text-review-text">REVIEW</span> {review} (
                  {formatPercent(total ? review / total : 0)})
                </li>
                <li>
                  <span className="text-decline-text">DECLINE</span> {decline} (
                  {formatPercent(total ? decline / total : 0)})
                </li>
              </ul>
            </StatTile>
            <StatTile label="Feature-updater lag" value={`${(m.updater_lag_seconds ?? 0).toFixed(1)} s`} />
          </div>
        </>
      )}
    </section>
  );
}
