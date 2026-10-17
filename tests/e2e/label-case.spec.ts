import { expect, test } from "@playwright/test";

import { firstOpenCase, signIn } from "./helpers";

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
      name: new RegExp(`Case ${caseId.slice(0, 8)}`),
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

  // The labelled case is listed under the Labelled filter and no longer under Open.
  await page.goto("/queue?status=labelled");
  await expect(page.locator(`a[href="/cases/${caseId}"]`)).toBeVisible();
  await page.goto("/queue");
  await expect(page.getByRole("table").getByRole("link").first()).toBeVisible();
  await expect(page.locator(`a[href="/cases/${caseId}"]`)).toHaveCount(0);
});

test("the label flow works from the keyboard alone", async ({ page }) => {
  await signIn(page, "analyst");
  const caseId = await firstOpenCase(page);
  // Queue: focus the rows, Enter opens the selected (first) case.
  await page.getByRole("rowgroup", { name: "Cases" }).focus();
  await page.keyboard.press("Enter");
  await expect(page).toHaveURL(`/cases/${caseId}`);

  await page.getByRole("button", { name: "Label case" }).focus();
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
  await expect(
    page.getByText(/Labelled\s+legitimate\s+by\s+analyst-1/),
  ).toBeVisible();
});

test("an ML engineer can read a case but not label it", async ({ page }) => {
  await signIn(page, "ml");
  const caseId = await firstOpenCase(page);
  await page.goto(`/cases/${caseId}`);
  await expect(
    page.getByRole("heading", {
      name: new RegExp(`Case ${caseId.slice(0, 8)}`),
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
