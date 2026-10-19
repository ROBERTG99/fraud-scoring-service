import { type KeyboardEvent, type PointerEvent, useId, useState } from "react";

interface Props {
  title: string;
  values: number[];
  labels: string[]; // one per value (e.g. time), shown in the tooltip and the table view
  format: (v: number) => string;
}

const W = 240;
const H = 48;
const PAD = 4;

/** One series: 2 px line, hover/keyboard crosshair with a tooltip, and a table view for screen readers. */
export function Sparkline({ title, values, labels, format }: Props) {
  const [hover, setHover] = useState<number | null>(null);
  const tableId = useId();
  if (values.length < 2) {
    return <p className="text-xs text-muted">Collecting samples for {title.toLowerCase()}…</p>;
  }
  const max = Math.max(...values);
  const min = Math.min(0, ...values);
  const span = max - min || 1;
  const x = (i: number) => PAD + (i * (W - 2 * PAD)) / (values.length - 1);
  const y = (v: number) => H - PAD - ((v - min) * (H - 2 * PAD)) / span;
  const d = values.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`).join("");
  const pick = (e: PointerEvent<SVGSVGElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const rel = ((e.clientX - rect.left) / rect.width) * W;
    setHover(
      Math.max(
        0,
        Math.min(values.length - 1, Math.round(((rel - PAD) * (values.length - 1)) / (W - 2 * PAD))),
      ),
    );
  };
  const onKey = (e: KeyboardEvent<SVGSVGElement>) => {
    const cur = hover ?? values.length - 1;
    if (e.key === "ArrowLeft") setHover(Math.max(0, cur - 1));
    else if (e.key === "ArrowRight") setHover(Math.min(values.length - 1, cur + 1));
    else if (e.key === "Escape") setHover(null);
    else return;
    e.preventDefault();
  };
  const h = hover;
  return (
    <figure className="relative">
      <svg
        viewBox={`0 0 ${W} ${H}`}
        className="h-12 w-full touch-none"
        role="img"
        aria-label={`${title}: latest ${format(values[values.length - 1]!)}`}
        aria-describedby={tableId}
        tabIndex={0}
        onPointerMove={pick}
        onPointerLeave={() => setHover(null)}
        onKeyDown={onKey}
        preserveAspectRatio="none"
      >
        <line x1={PAD} x2={W - PAD} y1={H - PAD} y2={H - PAD} stroke="var(--chart-grid)" strokeWidth={1} />
        <path
          d={d}
          fill="none"
          stroke="var(--chart-series)"
          strokeWidth={2}
          strokeLinejoin="round"
          strokeLinecap="round"
          vectorEffect="non-scaling-stroke"
        />
        {h !== null && (
          <>
            <line
              x1={x(h)}
              x2={x(h)}
              y1={PAD}
              y2={H - PAD}
              stroke="var(--text-muted)"
              strokeWidth={1}
              vectorEffect="non-scaling-stroke"
            />
            <circle
              cx={x(h)}
              cy={y(values[h]!)}
              r={4}
              fill="var(--chart-series)"
              stroke="var(--panel)"
              strokeWidth={2}
              vectorEffect="non-scaling-stroke"
            />
          </>
        )}
      </svg>
      {h !== null && (
        <div
          role="tooltip"
          className="num pointer-events-none absolute -top-7 right-0 rounded border border-border bg-panel-raised px-2 py-0.5 text-xs text-text"
        >
          {labels[h]} · {format(values[h]!)}
        </div>
      )}
      <table id={tableId} className="sr-only">
        <caption>{title}</caption>
        <tbody>
          {values.map((v, i) => (
            <tr key={i}>
              <th scope="row">{labels[i]}</th>
              <td>{format(v)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </figure>
  );
}
