// StreamLiveMetrics over the gateway: newline-delimited {"result": LiveMetrics} or {"error": …} messages.
import { useEffect, useRef, useState } from "react";

import { api } from "./client";
import type { LiveMetrics } from "./queries";

export type LiveStatus = "connecting" | "live" | "reconnecting";

export interface LiveState {
  status: LiveStatus;
  latest: LiveMetrics | null;
  history: LiveMetrics[]; // oldest first, at most HISTORY entries
  error: string | null;
  attempts: number; // consecutive failed connections
}

export const HISTORY = 60;

/** Parses complete lines from a chunk buffer; returns [messages, rest]. */
export function parseLines(buf: string): [{ result?: LiveMetrics; error?: { message?: string } }[], string] {
  const parts = buf.split("\n");
  const rest = parts.pop() ?? "";
  const out = parts
    .filter((l) => l.trim() !== "")
    .map((l) => JSON.parse(l) as { result?: LiveMetrics; error?: { message?: string } });
  return [out, rest];
}

export function backoff(attempt: number, baseMs: number): number {
  return Math.min(10_000, baseMs * 2 ** Math.max(0, attempt - 1));
}

/** Subscribes to live metrics, reconnecting with exponential backoff (base, 2×, 4×… capped at 10 s). */
export function useLiveMetrics(retryBaseMs = 1_000): LiveState {
  const [state, setState] = useState<LiveState>({
    status: "connecting",
    latest: null,
    history: [],
    error: null,
    attempts: 0,
  });
  const attemptsRef = useRef(0);
  useEffect(() => {
    const abort = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    const fail = (msg: string) => {
      if (abort.signal.aborted) return;
      attemptsRef.current += 1;
      const n = attemptsRef.current;
      setState((s) => ({ ...s, status: "reconnecting", error: msg, attempts: n }));
      timer = setTimeout(connect, backoff(n, retryBaseMs));
    };
    const connect = async () => {
      try {
        const r = await api.GET("/v1/metrics/live", { parseAs: "stream", signal: abort.signal });
        if (!r.response.ok || !r.data) return fail(`stream failed (${r.response.status})`);
        const reader = r.data.pipeThrough(new TextDecoderStream()).getReader();
        let buf = "";
        for (;;) {
          const { value, done } = await reader.read();
          if (done) return fail("stream closed");
          const [msgs, rest] = parseLines(buf + value);
          buf = rest;
          for (const m of msgs) {
            if (m.error) return fail(m.error.message ?? "stream error");
            const metrics = m.result;
            if (!metrics) continue;
            attemptsRef.current = 0;
            setState((s) => ({
              status: "live",
              latest: metrics,
              history: [...s.history, metrics].slice(-HISTORY),
              error: null,
              attempts: 0,
            }));
          }
        }
      } catch (e) {
        fail(e instanceof Error ? e.message : "stream error");
      }
    };
    void connect();
    return () => {
      abort.abort();
      clearTimeout(timer);
    };
  }, [retryBaseMs]);
  return state;
}
