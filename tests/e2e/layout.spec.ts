import { expect, test } from "@playwright/test";

import { firstOpenCase, signIn } from "./helpers";

for (const width of [1280, 768, 375]) {
  test(`case facts stay inside their panel at ${width}px`, async ({ page }) => {
    await page.setViewportSize({ width, height: 800 });
    await signIn(page, "analyst");
    await page.goto(`/cases/${await firstOpenCase(page)}`);
    const panel = page.getByRole("region", {
      name: "Feature snapshot and facts",
    });
    await expect(panel.getByRole("rowheader").first()).toBeVisible();
    const overflow = await panel.evaluate(
      (el) => el.scrollWidth - el.clientWidth,
    );
    expect(
      overflow,
      "long fact values must wrap, not run past the panel",
    ).toBeLessThanOrEqual(0);
    const pageOverflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(pageOverflow, "no horizontal page scroll").toBeLessThanOrEqual(0);
  });
}

test("no screen scrolls the page sideways at 375px", async ({ page }) => {
  await page.setViewportSize({ width: 375, height: 800 });
  await signIn(page, "risk");
  const caseId = await firstOpenCase(page);
  await page.goto(`/cases/${caseId}`);
  const account =
    (await page.locator('a[href^="/accounts/"]').first().textContent()) ?? "";
  for (const path of [
    "/queue",
    `/cases/${caseId}`,
    `/accounts/${account}`,
    "/live",
    "/rules",
    "/shadow",
  ]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByText(/^Loading/)).toHaveCount(0);
    const overflow = await page.evaluate(
      () => document.documentElement.scrollWidth - window.innerWidth,
    );
    expect(overflow, `${path} scrolls sideways`).toBeLessThanOrEqual(0);
  }
});
