---
name: demo-capture
description: Run the demo and capture screenshots of every console screen into docs/images/.
---
# Demo capture

1. `make up && make seed`.
2. `make demo` — all checkpoints must print OK.
3. `pnpm -C web exec playwright test --config ../tests/e2e/playwright.config.ts screenshots.spec.ts`.
4. Check `docs/images/` and reference the images from README. Never fabricate screenshots.
