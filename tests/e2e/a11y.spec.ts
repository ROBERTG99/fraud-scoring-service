import AxeBuilder from "@axe-core/playwright";
import { type Page, expect, test } from "@playwright/test";

import { PERSONAS, firstOpenCase, signIn } from "./helpers";

// WCAG 2.0/2.1/2.2 A and AA rules. The gate is spec §9: no serious (or critical) violation on any screen,
// in both themes, at the 375 px, 768 px and 1280 px breakpoints. Lesser findings are attached to the report.
const TAGS = ["wcag2a", "wcag2aa", "wcag21a", "wcag21aa", "wcag22aa"];

async function audit(page: Page, name: string): Promise<void> {
  const result = await new AxeBuilder({ page }).withTags(TAGS).analyze();
  const blocking = result.violations.filter(
    (v) => v.impact === "serious" || v.impact === "critical",
  );
  await test.info().attach(`axe-${name}.json`, {
    body: JSON.stringify(result.violations, null, 2),
    contentType: "application/json",
  });
  expect(
    blocking.map(
      (v) =>
        `${v.id} (${v.impact}): ${v.nodes.map((n) => n.target.join(" ")).join(", ")}`,
    ),
    `${name}: serious axe violations`,
  ).toEqual([]);
}

type Screen = {
  name: string;
  persona: keyof typeof PERSONAS;
  path: (caseId: string, account: string) => string;
  ready: RegExp;
};

const SCREENS: Screen[] = [
  {
    name: "queue",
    persona: "analyst",
    path: () => "/queue",
    ready: /Review queue/,
  },
  {
    name: "case",
    persona: "analyst",
    path: (c) => `/cases/${c}`,
    ready: /^Case /,
  },
  {
    name: "timeline",
    persona: "analyst",
    path: (_, a) => `/accounts/${a}`,
    ready: /Account timeline/,
  },
  {
    name: "shadow",
    persona: "ml",
    path: () => "/shadow",
    ready: /Model and shadow/,
  },
  { name: "live", persona: "risk", path: () => "/live", ready: /Live metrics/ },
  { name: "rules", persona: "risk", path: () => "/rules", ready: /Rules/ },
];

for (const width of [1280, 768, 375]) {
  for (const theme of ["dark", "light"] as const) {
    test.describe(`${width}px ${theme}`, () => {
      test.use({ viewport: { width, height: 800 } });
      // The console keeps the theme per viewer in localStorage (App.tsx), not from prefers-color-scheme.
      test.beforeEach(async ({ page }) => {
        await page.addInitScript(
          (t) => localStorage.setItem("fraud.theme", t),
          theme,
        );
      });

      if (theme === "dark") {
        // Sign-in renders before the theme toggle exists: always the dark default.
        test("sign-in", async ({ page }) => {
          await page.goto("/sign-in");
          await expect(
            page.getByRole("button", { name: /Analyst One/ }),
          ).toBeVisible();
          await audit(page, `sign-in-${width}`);
        });
      }

      for (const s of SCREENS) {
        test(s.name, async ({ page }) => {
          await signIn(page, s.persona);
          const caseId = await firstOpenCase(page);
          await page.goto(`/cases/${caseId}`);
          const account =
            (await page
              .locator('a[href^="/accounts/"]')
              .first()
              .textContent()) ?? "";
          await page.goto(s.path(caseId, account));
          await expect(
            page.getByRole("heading", { level: 1, name: s.ready }),
          ).toBeVisible();
          await expect(page.locator("html")).toHaveAttribute(
            "data-theme",
            theme,
          );
          await expect(page.getByText(/^Loading/)).toHaveCount(0);
          await audit(page, `${s.name}-${width}-${theme}`);
        });
      }
    });
  }
}

test("the label dialog passes axe while open", async ({ page }) => {
  await signIn(page, "analyst");
  await page.goto(`/cases/${await firstOpenCase(page)}`);
  await page.getByRole("button", { name: "Label case" }).click();
  await expect(page.getByRole("dialog")).toBeVisible();
  await audit(page, "label-dialog");
});
