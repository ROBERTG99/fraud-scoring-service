import { useEffect, useId, useRef, useState } from "react";

import { ApiError } from "../api/client";
import { type LabelValue, newIdempotencyKey, useLabelCase } from "../api/queries";
import type { Session } from "../auth/session";
import { shortId } from "../format";

const MAX_NOTE = 2000;

/** The dialog's Tab stops in order. A radio group is one stop: its checked radio, else its first. */
function tabStops(root: HTMLElement): HTMLElement[] {
  const all = [...root.querySelectorAll<HTMLElement>('input,button,textarea,[href],[tabindex="0"]')].filter(
    (n) => !n.hasAttribute("disabled"),
  );
  return all.filter((n) => {
    if (!(n instanceof HTMLInputElement) || n.type !== "radio") return true;
    const group = all.filter(
      (m): m is HTMLInputElement => m instanceof HTMLInputElement && m.type === "radio" && m.name === n.name,
    );
    return n === (group.find((m) => m.checked) ?? group[0]);
  });
}

interface Props {
  caseId: string;
  session: Session;
  onClose: () => void;
}

/** Writes an analyst label. The preview shows the exact analyst_labels row the server will write. */
export function LabelDialog({ caseId, session, onClose }: Props) {
  const [label, setLabel] = useState<LabelValue | null>(null);
  const [note, setNote] = useState("");
  // One key per dialog: a retry after a network error is the same request.
  const [idempotencyKey] = useState(newIdempotencyKey);
  const mutation = useLabelCase();
  const titleId = useId();
  const noteId = useId();
  const dialog = useRef<HTMLDivElement>(null);
  // The parent re-renders (and recreates onClose) on every case refetch; read the latest through a ref
  // so the effect below runs once per dialog and never pulls focus back to the first control.
  const close = useRef(onClose);
  useEffect(() => {
    close.current = onClose;
  });

  useEffect(() => {
    const el = dialog.current;
    el?.querySelector<HTMLElement>("input,button,textarea")?.focus();
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") close.current();
      if (e.key !== "Tab" || !el) return;
      const items = tabStops(el);
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last?.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first?.focus();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, []);

  const tooLong = [...note].length > MAX_NOTE;
  const row = {
    case_id: caseId,
    label: label === "LABEL_FRAUD" ? "fraud" : label === "LABEL_LEGIT" ? "legit" : "(choose)",
    note: note === "" ? null : note,
    analyst_sub: session.persona.id,
    idempotency_key: idempotencyKey,
  };
  const conflict = mutation.error instanceof ApiError && mutation.error.status === 409;

  return (
    <div className="fixed inset-0 z-10 flex items-center justify-center bg-black/60 p-4">
      <div
        ref={dialog}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleId}
        className="w-full max-w-lg rounded border border-border bg-panel p-4"
      >
        <h2 id={titleId} className="text-lg font-semibold">
          Label case <span className="num">{shortId(caseId)}</span>
        </h2>
        <fieldset className="mt-3">
          <legend className="text-sm text-muted">Outcome</legend>
          <div className="mt-1 flex gap-4">
            {(
              [
                ["LABEL_FRAUD", "Fraud"],
                ["LABEL_LEGIT", "Legitimate"],
              ] as const
            ).map(([v, text]) => (
              <label key={v} className="flex items-center gap-2">
                <input
                  type="radio"
                  name="label"
                  value={v}
                  checked={label === v}
                  onChange={() => setLabel(v)}
                />
                {text}
              </label>
            ))}
          </div>
        </fieldset>
        <label htmlFor={noteId} className="mt-3 block text-sm text-muted">
          Note (optional, at most {MAX_NOTE} characters)
        </label>
        <textarea
          id={noteId}
          value={note}
          onChange={(e) => setNote(e.target.value)}
          rows={3}
          aria-invalid={tooLong}
          className="mt-1 w-full rounded border border-field-border bg-bg p-2"
        />
        {tooLong && (
          <p className="text-sm text-decline-text">The note is longer than {MAX_NOTE} characters.</p>
        )}
        <section aria-label="Row preview" className="mt-3">
          <h3 className="text-sm text-muted">Row to be written to analyst_labels</h3>
          <pre
            data-testid="label-preview"
            tabIndex={0}
            className="num mt-1 overflow-x-auto rounded bg-bg p-2 text-xs"
          >
            {JSON.stringify(row, null, 2)}
          </pre>
        </section>
        {mutation.isError && (
          <p role="alert" className="mt-2 text-sm text-decline-text">
            {conflict ? "This case already has a different label." : (mutation.error as Error).message}
          </p>
        )}
        <div className="mt-4 flex justify-end gap-2">
          <button type="button" onClick={onClose} className="rounded border border-border px-3 py-1">
            Cancel
          </button>
          <button
            type="button"
            disabled={!label || tooLong || mutation.isPending}
            onClick={() =>
              label &&
              mutation.mutate({ caseId, label, note, idempotencyKey }, { onSuccess: () => close.current() })
            }
            className="rounded bg-accent px-3 py-1 font-semibold text-on-accent disabled:opacity-50"
          >
            {mutation.isPending ? "Saving…" : "Confirm label"}
          </button>
        </div>
      </div>
    </div>
  );
}
