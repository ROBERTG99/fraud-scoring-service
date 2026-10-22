import { readFileSync } from "node:fs";

import { expect, test } from "@playwright/test";

import { signIn } from "./helpers";

// The console half of `make demo`. scripts/demo.sh first runs the scripted scenario
// (`replay -scenario demo`, cmd/replay/scenario.go), which writes its checkpoints and the demo case to
// DEMO_CHECKPOINTS; this spec asserts each of them, then walks the analyst journey on that case.

type Checkpoint = {
  id: string;
  phase: string;
  ok: boolean;
  detail: string;
};

type DemoState = {
  account_id: string;
  case_id: string;
  decision_id: string;
  checkpoints: Checkpoint[];
};

// Every checkpoint the scenario must report, in order (cmd/replay/scenario.go, demoCheckpoints).
const SCENARIO_CHECKPOINTS = [
  "ato_review",
  "idempotent_replay",
  "card_testing_burst",
  "invalid_event_dlq",
  "case_persisted",
  "summary_verified",
  "shadow_compared",
  "model_unavailable_rules_only",
  "live_metrics_degraded",
  "model_recovered",
];

function demoState(): DemoState {
  const path = process.env.DEMO_CHECKPOINTS;
  expect(path, "DEMO_CHECKPOINTS names the scenario's output").toBeTruthy();
  return JSON.parse(readFileSync(path!, "utf8")) as DemoState;
}

test.describe.configure({ mode: "serial" });

test("every scenario checkpoint passed", () => {
  const state = demoState();
  expect(state.checkpoints.map((c) => c.id)).toEqual(SCENARIO_CHECKPOINTS);
  for (const c of state.checkpoints) {
    expect(c.ok, `${c.id}: ${c.detail}`).toBe(true);
  }
  expect(state.case_id).toMatch(/^[0-9a-f-]{36}$/);
});

test("console_case: the analyst opens the demo case with its reason codes and AI summary", async ({
  page,
}) => {
  const { case_id: caseId } = demoState();
  await signIn(page, "analyst");
  await page.goto(`/cases/${caseId}`);
  await expect(
    page.getByRole("heading", {
      name: new RegExp(`Case ${caseId.slice(-8)}`),
    }),
  ).toBeVisible();
  await expect(
    page.getByText("NEW_DEVICE_AFTER_CREDENTIAL_CHANGE").first(),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", {
      name: /AI summary · does not affect decisions/,
    }),
  ).toBeVisible();
  await expect(
    page.getByText("account_takeover", { exact: true }),
  ).toBeVisible();
});

test("evidence_highlights_fact: selecting an evidence item highlights its fact", async ({
  page,
}) => {
  const { case_id: caseId } = demoState();
  await signIn(page, "analyst");
  await page.goto(`/cases/${caseId}`);
  const evidence = page.locator("button[aria-pressed]", {
    hasText: "verified",
  });
  await expect(evidence).toHaveCount(3);
  await expect(page.getByText(/· unverified/)).toHaveCount(0);
  await evidence.first().click();
  await expect(evidence.first()).toHaveAttribute("aria-pressed", "true");
  await expect(page.locator('[aria-current="true"]')).toHaveCount(1);
});

test("label_leaves_queue: the analyst labels the case fraud after the row preview", async ({
  page,
}) => {
  const { case_id: caseId } = demoState();
  await signIn(page, "analyst");
  await page.goto(`/cases/${caseId}`);
  await page.getByRole("button", { name: "Label case" }).click();
  const dialog = page.getByRole("dialog", { name: /Label case/ });
  await dialog.getByRole("radio", { name: "Fraud" }).check();
  await dialog
    .getByRole("textbox")
    .fill("demo: new device two minutes after an email change");
  const preview = JSON.parse(
    (await dialog.getByTestId("label-preview").textContent()) ?? "{}",
  );
  expect(preview).toMatchObject({ case_id: caseId, label: "fraud" });
  await dialog.getByRole("button", { name: "Confirm label" }).click();
  await expect(dialog).toBeHidden();
  await expect(
    page.getByText(/Labelled\s+fraud\s+by\s+analyst-1/),
  ).toBeVisible();

  // The case left the open queue: the server lists it as labelled, not open.
  const token = await page.evaluate(
    () => JSON.parse(sessionStorage.getItem("fraud.session") ?? "{}").token,
  );
  const stored = await page.request.get(`/api/v1/cases/${caseId}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(await stored.json()).toMatchObject({
    status: "CASE_STATUS_LABELLED",
    label: { label: "LABEL_FRAUD" },
  });
});
