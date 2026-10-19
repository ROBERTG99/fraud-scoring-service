## What and why

<!-- One paragraph: the change and the reason. Link the task (PROGRESS.md) or decision (docs/DECISIONS.md). -->

## Evidence

<!-- The commands you ran and their one-line results (make check-fast, make test, make eval, ...). -->

## Checklist

- [ ] Test written first; `make check-fast` is green
- [ ] Numbers in docs cite the command or report that produced them
- [ ] No secrets, no real personal data; synthetic data only
- [ ] Nothing puts a network call other than the model server, Redis or Postgres on the scoring path
