import { actionLabel } from "../format";

const STYLE: Record<string, string> = {
  APPROVE: "border-approve text-approve-text",
  REVIEW: "border-review text-review-text",
  DECLINE: "border-decline text-decline-text",
  UNKNOWN: "border-border text-muted",
};

/** Decision action: colour is always paired with the word. */
export function ActionBadge({ action }: { action: string | undefined }) {
  const a = actionLabel(action);
  return <span className={`inline-block rounded border px-1.5 text-xs font-semibold ${STYLE[a]}`}>{a}</span>;
}
