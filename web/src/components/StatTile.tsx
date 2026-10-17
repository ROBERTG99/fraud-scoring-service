import type { ReactNode } from "react";

export function StatTile({
  label,
  value,
  sub,
  children,
}: {
  label: string;
  value: string;
  sub?: string;
  children?: ReactNode;
}) {
  return (
    <div className="rounded border border-border bg-panel p-3">
      <p className="text-sm text-muted">{label}</p>
      <p className="num mt-1 text-2xl font-semibold text-text">{value}</p>
      {sub && <p className="mt-0.5 text-xs text-muted">{sub}</p>}
      {children && <div className="mt-2">{children}</div>}
    </div>
  );
}
