import { render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { backoff, parseLines } from "../api/live";
import { signedIn } from "../test/fixtures";
import { json, stubApi } from "../test/render";
import Live from "./live";

afterEach(() => vi.unstubAllGlobals());

function ndjson(lines: unknown[], keepOpen = false): Response {
  const enc = new TextEncoder();
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      for (const l of lines) c.enqueue(enc.encode(JSON.stringify(l) + "\n"));
      if (!keepOpen) c.close();
    },
  });
  return new Response(body, { status: 200, headers: { "Content-Type": "application/json" } });
}

const sample = (rps: number, ts: string) => ({
  result: {
    ts,
    rps,
    p95_ms: 6.5,
    approve: "90",
    review: "8",
    decline: "2",
    degraded_rate: 0.02,
    model_unavailable: "1",
    features_unavailable: "0",
    updater_lag_seconds: 0.4,
  },
});

describe("live metrics", () => {
  it("stream reconnects after error", async () => {
    signedIn("analyst");
    let n = 0;
    const api = stubApi({
      "GET /api/v1/metrics/live": () => {
        n += 1;
        if (n === 1) return json({ code: 14, message: "upstream unavailable" }, 502); // connection fails
        if (n === 2)
          return ndjson([sample(120, "2026-10-17T10:00:00Z"), { error: { message: "stream reset" } }]); // data, then error
        return ndjson([sample(130, "2026-10-17T10:00:02Z"), sample(140, "2026-10-17T10:00:04Z")], true);
      },
    });
    vi.stubGlobal("fetch", api.fetchMock);
    render(<Live retryBaseMs={5} />);
    expect(await screen.findByText(/Reconnecting/)).toBeInTheDocument();
    expect((await screen.findAllByText("140.0")).length).toBeGreaterThan(0); // latest RPS (tile and table view)
    expect(screen.getByRole("status")).toHaveTextContent("Live");
    expect(n).toBe(3);
    expect(screen.getByText("6.5 ms", { selector: "p" })).toBeInTheDocument(); // tile value (the table view repeats it)
    expect(screen.getByText("2.0%", { selector: "p" })).toBeInTheDocument(); // degraded rate tile
    expect(screen.getByText("REVIEW")).toBeInTheDocument();
    // Both samples since the reconnect plus the one before it are kept for the sparkline.
    expect(screen.getByRole("img", { name: /Requests per second: latest 140.0/ })).toBeInTheDocument();
    expect(api.calls.every((c) => c.headers.get("Authorization") === "Bearer tok-analyst-1")).toBe(true);
  });

  it("tells loading (connecting) apart from an idle scorer (connected, no decisions)", async () => {
    signedIn("analyst");
    let open: (r: Response) => void = () => {};
    vi.stubGlobal(
      "fetch",
      stubApi({ "GET /api/v1/metrics/live": () => new Promise<Response>((r) => (open = r)) }).fetchMock,
    );
    render(<Live retryBaseMs={5} />);
    expect(await screen.findByText("Connecting to the live stream…")).toBeInTheDocument();
    const idle = sample(0, "2026-10-17T10:00:00Z");
    idle.result = { ...idle.result, approve: "0", review: "0", decline: "0", p95_ms: 0 };
    open(ndjson([idle], true));
    expect(await screen.findByText("No decisions in the last 10 s window.")).toBeInTheDocument();
    expect(screen.queryByText(/Connecting/)).toBeNull();
  });

  it("parses partial lines and backs off exponentially up to 10 s", () => {
    const [msgs, rest] = parseLines('{"result":{"rps":1}}\n{"resu');
    expect(msgs).toHaveLength(1);
    expect(rest).toBe('{"resu');
    expect([1, 2, 3, 4, 10].map((a) => backoff(a, 1000))).toEqual([1000, 2000, 4000, 8000, 10000]);
  });
});
