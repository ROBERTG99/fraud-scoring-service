import { expect, test } from "@playwright/test";

import { firstOpenCase, signIn, tabUntilFocused } from "./helpers";

test("an analyst labels an open case and it leaves the open queue", async ({
  page,
}) => {
  await signIn(page, "analyst");
  await expect(
    page.getByRole("heading", { name: /Review queue/ }),
  ).toBeVisible();
  const caseId = await firstOpenCase(page);

  await page.getByRole("table").getByRole("link").first().click();
  await expect(page).toHaveURL(`/cases/${caseId}`);
  await expect(
    page.getByRole("heading", {
      name: new RegExp(`Case ${caseId.slice(-8)}`),
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", {
      name: /AI summary · does not affect decisions/,
    }),
  ).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Feature snapshot and facts" }),
  ).toBeVisible();

  await page.getByRole("button", { name: "Label case" }).click();
  const dialog = page.getByRole("dialog", { name: /Label case/ });
  await expect(dialog).toBeVisible();
  await expect(
    dialog.getByRole("button", { name: "Confirm label" }),
  ).toBeDisabled();
  await dialog.getByRole("radio", { name: "Fraud" }).check();
  await dialog.getByRole("textbox").fill("e2e: confirmed with the cardholder");
  const preview = JSON.parse(
    (await dialog.getByTestId("label-preview").textContent()) ?? "{}",
  );
  expect(preview).toMatchObject({
    case_id: caseId,
    label: "fraud",
    analyst_sub: "analyst-1",
  });
  await dialog.getByRole("button", { name: "Confirm label" }).click();

  await expect(dialog).toBeHidden();
  await expect(
    page.getByText(/Labelled\s+fraud\s+by\s+analyst-1/),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Label case" })).toHaveCount(0);

  // The server stored the label (the Labelled list can page it off page 1, so ask for the case itself).
  const token = await page.evaluate(
    () => JSON.parse(sessionStorage.getItem("fraud.session") ?? "{}").token,
  );
  const stored = await page.request.get(`/api/v1/cases/${caseId}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(stored.status()).toBe(200);
  expect(await stored.json()).toMatchObject({
    status: "CASE_STATUS_LABELLED",
    label: { label: "LABEL_FRAUD", analyst_sub: "analyst-1" },
  });
  // It was the first open case, so if it were still open it would be on page 1 of the Open queue.
  await page.goto("/queue");
  await expect(page.getByRole("table").getByRole("link").first()).toBeVisible();
  await expect(page.locator(`a[href="/cases/${caseId}"]`)).toHaveCount(0);
});

test("the label flow works from the keyboard alone", async ({ page }) => {
  await signIn(page, "analyst");
  const caseId = await firstOpenCase(page);
  // Queue: the first Tab stop is the skip link, then Tab reaches the case list (one stop, roving tabindex).
  await page.keyboard.press("Tab");
  await expect(
    page.getByRole("link", { name: "Skip to content" }),
  ).toBeFocused();
  const first = page.locator(`a[href="/cases/${caseId}"]`);
  await tabUntilFocused(page, first);
  await page.keyboard.press("j");
  await expect(first).not.toBeFocused();
  await page.keyboard.press("k");
  await expect(first).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(`/cases/${caseId}`);

  const labelButton = page.getByRole("button", { name: "Label case" });
  await tabUntilFocused(page, labelButton);
  await page.keyboard.press("Enter");
  const dialog = page.getByRole("dialog", { name: /Label case/ });
  await expect(dialog.getByRole("radio", { name: "Fraud" })).toBeFocused();
  await page.keyboard.press("ArrowRight"); // radio group: Fraud → Legitimate
  await expect(dialog.getByRole("radio", { name: "Legitimate" })).toBeChecked();
  await page.keyboard.press("Tab"); // note
  await page.keyboard.type("keyboard only");
  await page.keyboard.press("Tab"); // row preview (scrollable, so focusable)
  await page.keyboard.press("Tab"); // Cancel
  await page.keyboard.press("Tab"); // Confirm label
  await expect(
    dialog.getByRole("button", { name: "Confirm label" }),
  ).toBeFocused();
  await page.keyboard.press("Tab"); // focus is trapped: back to the first control
  await expect(dialog.getByRole("radio", { name: "Legitimate" })).toBeFocused();
  await page.keyboard.press("Shift+Tab");
  await expect(
    dialog.getByRole("button", { name: "Confirm label" }),
  ).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(dialog).toBeHidden();
  // Focus returns to the page (the label text that replaced the button), not to <body>.
  const labelled = page.getByText(/Labelled\s+legitimate\s+by\s+analyst-1/);
  await expect(labelled).toBeVisible();
  await expect(labelled).toBeFocused();
});

test("closing the label dialog returns focus to Label case", async ({
  page,
}) => {
  await signIn(page, "analyst");
  await page.goto(`/cases/${await firstOpenCase(page)}`);
  const labelButton = page.getByRole("button", { name: "Label case" });
  await labelButton.click();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).toBeHidden();
  await expect(labelButton).toBeFocused();
});

test("an ML engineer can read a case but not label it", async ({ page }) => {
  await signIn(page, "ml");
  const caseId = await firstOpenCase(page);
  await page.goto(`/cases/${caseId}`);
  await expect(
    page.getByRole("heading", {
      name: new RegExp(`Case ${caseId.slice(-8)}`),
    }),
  ).toBeVisible();
  await expect(page.getByRole("button", { name: "Label case" })).toHaveCount(0);
  // The server refuses it too (RBAC), not just the console.
  const token = await page.evaluate(
    () => JSON.parse(sessionStorage.getItem("fraud.session") ?? "{}").token,
  );
  const resp = await page.request.post(`/api/v1/cases/${caseId}:label`, {
    headers: { Authorization: `Bearer ${token}` },
    data: {
      label: "LABEL_FRAUD",
      note: "",
      idempotency_key: crypto.randomUUID(),
    },
  });
  expect(resp.status()).toBe(403);
});

test("signed-out visitors are sent to sign-in and come back afterwards", async ({
  page,
}) => {
  await page.goto("/live");
  await expect(page).toHaveURL(/\/sign-in\?next=%2Flive$/);
  await page.getByRole("button", { name: /Risk Engineer One/ }).click();
  await expect(page).toHaveURL(/\/live$/);
});
