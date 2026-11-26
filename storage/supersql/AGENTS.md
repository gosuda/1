# AGENTS.md

## Release blockers

- Read `docs/RELEASE_BLOCKERS.md` before starting work; it is the source of truth for what blocks a release.
- Keep it current in the same change:
  - Fixed a listed item → check it off only after its **Verify** step passes.
  - Found a new correctness, durability, concurrency, security, or test-suite blocker → add it with severity, location, evidence, and a verification command.
  - Found a listed item is invalid → move it to "Verified non-blockers" with the evidence.
