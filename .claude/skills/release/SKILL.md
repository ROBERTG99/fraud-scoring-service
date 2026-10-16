---
name: release
description: Cut a release (tag v*) after make verify passes.
---
# Release

1. `make verify` green; `reviewer` verdict "ship".
2. Update `CHANGELOG.md` (Keep a Changelog) and the README results from generated reports.
3. `git tag -a vX.Y.Z -m "vX.Y.Z"`; pushing the tag triggers `release.yml` (images to GHCR, SBOM).
4. Do not create or change repository visibility; do not force-push.
