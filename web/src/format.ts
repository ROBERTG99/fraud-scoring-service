// Display helpers. int64 fields arrive as strings (proto3 JSON).

export function toNumber(v: string | number | undefined): number {
  if (v === undefined || v === "") return 0;
  const n = typeof v === "number" ? v : Number(v);
  return Number.isFinite(n) ? n : 0;
}

const ZERO_DECIMAL = new Set(["JPY"]);

export function formatAmount(minor: string | number | undefined, currency = "USD"): string {
  const n = toNumber(minor);
  const digits = ZERO_DECIMAL.has(currency) ? 0 : 2;
  const major = n / 10 ** digits;
  return `${currency} ${major.toLocaleString("en-US", { minimumFractionDigits: digits, maximumFractionDigits: digits })}`;
}

export function formatScore(score: number | undefined): string {
  if (score === undefined || score < 0) return "n/a";
  return score.toFixed(2);
}

export function formatPercent(rate: number | undefined, digits = 1): string {
  return `${((rate ?? 0) * 100).toFixed(digits)}%`;
}

export function age(iso: string | undefined, now = Date.now()): string {
  if (!iso) return "";
  const s = Math.max(0, Math.round((now - Date.parse(iso)) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}

export function actionLabel(a: string | undefined): "APPROVE" | "REVIEW" | "DECLINE" | "UNKNOWN" {
  switch (a) {
    case "ACTION_APPROVE":
      return "APPROVE";
    case "ACTION_REVIEW":
      return "REVIEW";
    case "ACTION_DECLINE":
      return "DECLINE";
  }
  return "UNKNOWN";
}

export function shortId(id: string | undefined): string {
  return id ? id.slice(0, 8) : "";
}
