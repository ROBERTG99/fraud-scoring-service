import { type Locator, type Page, expect } from "@playwright/test";

export const PERSONAS = {
  analyst: "Analyst One",
  risk: "Risk Engineer One",
  ml: "ML Engineer One",
} as const;

/** Signs in through the dev-issuer persona picker, the way an analyst does. */
export async function signIn(
  page: Page,
  persona: keyof typeof PERSONAS,
  next = "/queue",
): Promise<void> {
  await page.goto(`/sign-in?next=${encodeURIComponent(next)}`);
  await page
    .getByRole("button", { name: new RegExp(PERSONAS[persona]) })
    .click();
  await expect(page).toHaveURL(new RegExp(`${next.replace(/[/?]/g, "\\$&")}$`));
}

/** The first open case in the queue (seeded by `make seed`). */
export async function firstOpenCase(page: Page): Promise<string> {
  await page.goto("/queue");
  const link = page.getByRole("table").getByRole("link").first();
  await expect(link).toBeVisible();
  const href = await link.getAttribute("href");
  expect(href).toMatch(/^\/cases\/[0-9a-f-]{36}$/);
  return href!.slice("/cases/".length);
}

/** Presses Tab until the target has focus, the way a keyboard user reaches it (at most `limit` presses). */
export async function tabUntilFocused(
  page: Page,
  target: Locator,
  limit = 30,
): Promise<void> {
  for (let i = 0; i < limit; i++) {
    if (await target.evaluate((el) => el === document.activeElement)) return;
    await page.keyboard.press("Tab");
  }
  await expect(target).toBeFocused();
}
