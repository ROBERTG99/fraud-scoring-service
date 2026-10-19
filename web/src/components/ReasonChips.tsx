const DEGRADED = new Set(["MODEL_UNAVAILABLE", "FEATURES_UNAVAILABLE"]);

/** Reason codes as chips; degraded-mode codes are marked in words, not only colour. */
export function ReasonChips({ codes }: { codes: string[] | undefined }) {
  if (!codes?.length) return <span className="text-muted">none</span>;
  return (
    <ul className="flex flex-wrap gap-1" aria-label="Reason codes">
      {codes.map((c) => (
        <li
          key={c}
          className={`num rounded border px-1.5 text-xs ${DEGRADED.has(c) ? "border-review text-review-text" : "border-border text-text"}`}
        >
          {DEGRADED.has(c) ? `degraded: ${c}` : c}
        </li>
      ))}
    </ul>
  );
}
