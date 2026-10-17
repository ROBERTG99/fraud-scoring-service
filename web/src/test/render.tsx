import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render } from "@testing-library/react";
import { type RouteObject, RouterProvider, createMemoryRouter } from "react-router-dom";

export function renderRoutes(routes: RouteObject[], path: string) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const router = createMemoryRouter(routes, { initialEntries: [path] });
  const utils = render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return { ...utils, router, qc };
}

export function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });
}

export type Handler = (req: Request, url: URL) => Response | Promise<Response>;

/** Stubs fetch with a router: "GET /api/v1/cases" → handler. Unmatched requests return 404. */
export function stubApi(routes: Record<string, Handler>) {
  const calls: { method: string; path: string; body: string; headers: Headers }[] = [];
  const fetchMock = async (input: RequestInfo | URL, init?: RequestInit) => {
    const req =
      input instanceof Request ? input : new Request(new URL(String(input), "http://localhost"), init);
    const url = new URL(req.url, "http://localhost");
    const body = req.method === "GET" ? "" : await req.clone().text();
    calls.push({ method: req.method, path: url.pathname, body, headers: req.headers });
    for (const [key, h] of Object.entries(routes)) {
      const [method, pattern] = key.split(" ") as [string, string];
      const re = new RegExp("^" + pattern.replace(/[.]/g, "\\.").replace(/\{[^}]+\}/g, "[^/]+") + "$");
      if (method === req.method && re.test(url.pathname)) return h(req, url);
    }
    return json({ code: 5, message: "not found" }, 404);
  };
  return { fetchMock, calls };
}
