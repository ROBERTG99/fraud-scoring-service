// The one TanStack Query layer: every screen reads and writes through these hooks.
import { keepPreviousData, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, summarizerApi, unwrap } from "./client";
import type { components } from "./schema";

export type Schemas = components["schemas"];
export type Case = Schemas["v1Case"];
export type Decision = Schemas["v1Decision"];
export type Action = Schemas["v1Action"];
export type LiveMetrics = Schemas["v1LiveMetrics"];
export type RuleSet = Schemas["v1RuleSet"];
export type ShadowSummary = Schemas["v1ShadowSummary"];
export type Timeline = Schemas["v1Timeline"];
export type TestRuleResponse = Schemas["v1TestRuleResponse"];
export type LabelValue = "LABEL_FRAUD" | "LABEL_LEGIT";

export interface CaseFilters {
  status?: "CASE_STATUS_OPEN" | "CASE_STATUS_LABELLED";
  reason_code?: string;
  summary_status?: "" | "pending" | "ok" | "fallback" | "unfaithful";
  page_token?: string;
  page_size?: number;
}

export const keys = {
  cases: (f: CaseFilters) => ["cases", f] as const,
  case: (id: string) => ["case", id] as const,
  timeline: (account: string) => ["timeline", account] as const,
  shadow: (hours: number) => ["shadow", hours] as const,
  rules: ["rules", "active"] as const,
};

function clean<T extends object>(o: T): T {
  return Object.fromEntries(Object.entries(o).filter(([, v]) => v !== undefined && v !== "")) as T;
}

export function useCases(f: CaseFilters) {
  return useQuery({
    queryKey: keys.cases(f),
    queryFn: async ({ signal }) =>
      unwrap(await api.GET("/v1/cases", { params: { query: clean(f) }, signal })),
    placeholderData: keepPreviousData,
    refetchInterval: 15_000,
  });
}

/** One case. `watch` keeps polling (e.g. while a requested regeneration runs in the background). */
export function useCase(caseId: string, watch = false) {
  return useQuery({
    queryKey: keys.case(caseId),
    queryFn: async ({ signal }) =>
      unwrap(await api.GET("/v1/cases/{case_id}", { params: { path: { case_id: caseId } }, signal })),
    // Poll while the AI summary is still pending.
    refetchInterval: (q) => (watch || q.state.data?.summary_status === "pending" ? 3_000 : false),
  });
}

export interface LabelInput {
  caseId: string;
  label: LabelValue;
  note: string;
  idempotencyKey: string;
}

export function useLabelCase() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: LabelInput) =>
      unwrap(
        await api.POST("/v1/cases/{case_id}:label", {
          params: { path: { case_id: v.caseId } },
          body: { label: v.label, note: v.note, idempotency_key: v.idempotencyKey },
        }),
      ),
    onSuccess: (c, v) => {
      qc.setQueryData(keys.case(v.caseId), c);
      void qc.invalidateQueries({ queryKey: ["cases"] });
    },
  });
}

export function useTimeline(accountId: string) {
  return useQuery({
    queryKey: keys.timeline(accountId),
    queryFn: async ({ signal }) =>
      unwrap(
        await api.GET("/v1/accounts/{account_id}/timeline", {
          params: { path: { account_id: accountId }, query: { limit: 200 } },
          signal,
        }),
      ),
    enabled: accountId !== "",
  });
}

export function useShadowSummary(windowHours: number, enabled = true) {
  return useQuery({
    queryKey: keys.shadow(windowHours),
    queryFn: async ({ signal }) =>
      unwrap(
        await api.GET("/v1/shadow/summary", {
          params: { query: { window_hours: windowHours, disagreement_limit: 50 } },
          signal,
        }),
      ),
    enabled,
  });
}

export function useActiveRules() {
  return useQuery({
    queryKey: keys.rules,
    queryFn: async ({ signal }) => unwrap(await api.GET("/v1/rules/active", { signal })),
  });
}

export function useTestRule() {
  return useMutation({
    mutationFn: async (v: { when: string; sampleSize?: number }) =>
      unwrap(await api.POST("/v1/rules:test", { body: { when: v.when, sample_size: v.sampleSize } })),
  });
}

/** Asks the summarizer for a fresh AI summary (analyst or risk_engineer); the case refetches while pending. */
export function useRegenerateSummary() {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (v: { caseId: string; idempotencyKey: string }) =>
      unwrap(
        await summarizerApi.POST("/v1/summaries/{case_id}:regenerate", {
          params: { path: { case_id: v.caseId }, header: { "Idempotency-Key": v.idempotencyKey } },
        }),
      ),
    onSuccess: (_r, v) => void qc.invalidateQueries({ queryKey: keys.case(v.caseId) }),
  });
}

export function newIdempotencyKey(): string {
  return crypto.randomUUID();
}
