import path from "node:path";

import { type Page, expect, test } from "@playwright/test";

import { firstOpenCase, signIn } from "./helpers";

// `make docs` (E2E_SCREENSHOTS=1): real screens of the seeded stack, written to docs/images/.
const OUT = path.resolve(__dirname, "../../docs/images");

async function shot(page: Page, name: string): Promise<void> {
  await expect(page.getByText(/^Loading/)).toHaveCount(0);
  await page.screenshot({
    path: path.join(OUT, `${name}.png`),
    fullPage: false,
  });
}

test.use({ viewport: { width: 1280, height: 800 } });

test("console screens", async ({ page }) => {
  await page.goto("/sign-in");
  await expect(page.getByRole("button", { name: /Analyst One/ })).toBeVisible();
  await shot(page, "sign-in");

  await signIn(page, "analyst");
  const caseId = await firstOpenCase(page);
  await shot(page, "queue");

  await page.goto(`/cases/${caseId}`);
  await expect(
    page.getByRole("heading", { name: "Feature snapshot and facts" }),
  ).toBeVisible();
  const evidence = page
    .getByRole("button", { pressed: false })
    .filter({ hasText: /verified/ })
    .first();
  if (await evidence.count()) await evidence.click(); // show the evidence → fact highlight
  await shot(page, "case-detail");

  await page.getByRole("button", { name: "Label case" }).click();
  await page.getByRole("dialog").getByRole("radio", { name: "Fraud" }).check();
  await shot(page, "label-dialog");
  await page
    .getByRole("dialog")
    .getByRole("button", { name: "Cancel" })
    .click();

  const account =
    (await page.locator('a[href^="/accounts/"]').first().textContent()) ?? "";
  await page.goto(`/accounts/${account}`);
  await shot(page, "timeline");

  await signIn(page, "risk", "/live");
  // A few samples of the live stream: poll the sparkline's data table instead of sleeping.
  await expect
    .poll(
      () =>
        page
          .getByRole("table", { name: "Requests per second" })
          .locator("tbody tr")
          .count(),
      { timeout: 15_000 },
    )
    .toBeGreaterThanOrEqual(3);
  await shot(page, "live");
  await page.goto("/rules");
  await shot(page, "rules");
  await page.goto("/shadow");
  await shot(page, "shadow");
});

test("mobile and light theme", async ({ page }) => {
  await page.addInitScript(() => localStorage.setItem("fraud.theme", "light"));
  await signIn(page, "analyst");
  const caseId = await firstOpenCase(page);
  await page.goto(`/cases/${caseId}`);
  await expect(
    page.getByRole("heading", { name: "Feature snapshot and facts" }),
  ).toBeVisible();
  await shot(page, "case-detail-light");
  await page.setViewportSize({ width: 375, height: 800 });
  await page.goto("/queue");
  await shot(page, "queue-375-light");
});
