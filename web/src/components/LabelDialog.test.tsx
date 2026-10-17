import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { getSession } from "../auth/session";
import { CASE_ID, sampleCase, signedIn } from "../test/fixtures";
import { json, stubApi } from "../test/render";
import { LabelDialog } from "./LabelDialog";

afterEach(() => vi.unstubAllGlobals());

function setup(status = 200) {
  const { calls, onClose } = mount(status);
  return { calls, onClose };
}

function mount(status = 200) {
  signedIn("analyst");
  const { fetchMock, calls } = stubApi({
    [`POST /api/v1/cases/{id}:label`]: () =>
      status === 200
        ? json(sampleCase({ status: "CASE_STATUS_LABELLED" }))
        : json({ code: 6, message: "conflicting label" }, status),
  });
  vi.stubGlobal("fetch", fetchMock);
  const onClose = vi.fn();
  const qc = new QueryClient({ defaultOptions: { mutations: { retry: false } } });
  const ui = (close: () => void) => (
    <QueryClientProvider client={qc}>
      <LabelDialog caseId={CASE_ID} session={getSession()!} onClose={close} />
    </QueryClientProvider>
  );
  const { rerender } = render(ui(onClose));
  return { calls, onClose, rerender: (close: () => void) => rerender(ui(close)) };
}

describe("LabelDialog", () => {
  it("previews the exact row to be written and sends the same values", async () => {
    const { calls, onClose } = setup();
    const preview = () => JSON.parse(screen.getByTestId("label-preview").textContent ?? "{}");
    expect(preview()).toMatchObject({
      case_id: CASE_ID,
      label: "(choose)",
      note: null,
      analyst_sub: "analyst-1",
    });
    expect(screen.getByRole("button", { name: "Confirm label" })).toBeDisabled();

    await userEvent.click(screen.getByLabelText("Fraud"));
    await userEvent.type(screen.getByLabelText(/Note/), "Customer denied the purchase");
    const row = preview();
    expect(row).toMatchObject({
      label: "fraud",
      note: "Customer denied the purchase",
      analyst_sub: "analyst-1",
    });
    expect(row.idempotency_key).toMatch(/^[A-Za-z0-9_-]{8,128}$/);

    await userEvent.click(screen.getByRole("button", { name: "Confirm label" }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    const sent = JSON.parse(calls[0]!.body);
    expect(sent).toEqual({
      label: "LABEL_FRAUD",
      note: "Customer denied the purchase",
      idempotency_key: row.idempotency_key,
    });
    expect(calls[0]!.path).toBe(`/api/v1/cases/${CASE_ID}:label`);
  });

  it("explains a conflicting label and keeps the dialog open", async () => {
    const { onClose } = setup(409);
    await userEvent.click(screen.getByLabelText("Legitimate"));
    await userEvent.click(screen.getByRole("button", { name: "Confirm label" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("already has a different label");
    expect(onClose).not.toHaveBeenCalled();
  });

  it("blocks notes over 2000 characters and closes on Escape", async () => {
    const { onClose } = setup();
    await userEvent.click(screen.getByLabelText("Fraud"));
    const note = screen.getByLabelText(/Note/);
    await userEvent.click(note);
    await userEvent.paste("x".repeat(2001));
    expect(screen.getByText(/longer than 2000/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Confirm label" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    expect(onClose).toHaveBeenCalled();
  });

  it("keeps Tab inside the dialog when the checked radio is not the first control", async () => {
    setup();
    const user = userEvent.setup();
    await user.click(screen.getByRole("radio", { name: "Legitimate" }));
    // Tab order: checked radio (the group is one stop), note, row preview, Cancel, Confirm label.
    await user.tab();
    expect(screen.getByRole("textbox")).toHaveFocus();
    await user.tab();
    expect(screen.getByTestId("label-preview")).toHaveFocus();
    await user.tab();
    await user.tab();
    expect(screen.getByRole("button", { name: "Confirm label" })).toHaveFocus();
    await user.tab();
    expect(screen.getByRole("radio", { name: "Legitimate" })).toHaveFocus();
    await user.tab({ shift: true });
    expect(screen.getByRole("button", { name: "Confirm label" })).toHaveFocus();
  });

  it("keeps focus where the analyst is typing when the parent re-renders with a new onClose", async () => {
    // case.tsx re-renders every 3 s while the summary is pending, with a fresh onClose each time.
    const { rerender } = mount();
    const user = userEvent.setup();
    expect(screen.getByRole("radio", { name: "Fraud" })).toHaveFocus();
    const note = screen.getByLabelText(/Note/);
    await user.click(note);
    await user.keyboard("half a note");
    const latest = vi.fn();
    rerender(() => latest());
    rerender(() => latest());
    expect(note).toHaveFocus();
    await user.keyboard("{Escape}");
    expect(latest).toHaveBeenCalledTimes(1); // the newest onClose, not the one from the first render
  });
});
