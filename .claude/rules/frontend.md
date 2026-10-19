---
paths:
  - "web/**"
---
# Frontend rules

- Data only through generated types (`web/src/api/schema.d.ts`) and the TanStack Query layer in `web/src/api/`.
- Every screen has loading, empty, error-with-retry and success states.
- Colours from `web/src/tokens.css`; colour is always paired with text. WCAG 2.2 AA, keyboard reachable.
- IDs and amounts use JetBrains Mono with tabular numerals.
- Vitest for components; Playwright + axe in `tests/e2e/`.
