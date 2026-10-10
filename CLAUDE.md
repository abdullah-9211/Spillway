# Spillway

Spillway is an open-source durable agent runtime and multi-provider LLM gateway. A Go service does the work and a separate Next.js app in `web/` is the dashboard. All twelve feature phases are built and verified; Phase 13 was done as evidence and documentation only (README, benchmarks, demo script, screenshots), with deployment packaging deferred.

## Read first

1. `docs/SPEC.md`: what is being built and the decisions already locked. Do not re-open them.
2. `docs/TECHNICAL.md`: how it is built. Section 10 is the phase plan. Work on one phase at a time.
3. `docs/design/`: the two design system files (`DESIGN-linear.app.md` for the dark theme, `DESIGN-claude.md` for the light theme), plus `tokens.json` and `tokens.css`, the exported tokens.
4. The approved design lives in a Claude artifact, "Spillway Dashboard Design": https://claude.ai/artifact/DnhuCtbyD973N1Yn1zKXQW. It is private to the owner and is the visual reference for every screen, in dark and light.

## How to work

- **One phase at a time**, in the order of `docs/TECHNICAL.md` section 10. Do not start the next phase until the user says "go".
- **Backend first, then frontend** within a phase. Write the tests with the code. Build the frontend against the real backend, never mock data.
- **Self-check, kept cheap.** Before asking the user to verify, run the fast checks: Go unit tests with `-race`, the phase's integration tests, Vitest with `vitest-axe`, type-check and lint. For a phase with a screen, do at most one basic smoke check per screen.
- **No extensive Playwright or end-to-end suites.** The user wants to save tokens.
- **Skip slow tests.** Anything over about 60 seconds is skipped locally and reported as skipped, with how long it ran. The full chaos test and the load tests run in CI. Run the chaos test locally with `CHAOS_RUNS=10`.
- **Report honestly.** State failures and skipped checks. Never say something works unless it was run.
- **End every phase** with a "Verify phase N" message: what was built (backend and frontend), what the self-check ran, how to run it, backend and frontend checks with the result to expect, and known gaps. Then wait.

## Rules that are easy to get wrong

- Use only colors and fonts from the two design files. The dark theme follows Linear's file and the light theme follows Claude's file. Do not mix them or invent colors.
- Mono type is for IDs, keys, code and tool or model identifiers only. Numbers use Inter with tabular figures.
- Status is always a shape plus a word, never color alone.
- Money is `numeric(14,8)` in Postgres and a micro-dollar `int64` in Go, never `float64`.
- Exactly-once side effects rely on the receiver honouring the idempotency key. Say so wherever it is claimed.
- **Commit and push at the end of every phase** (and after any fix round the user asks for), to `origin` (`https://github.com/abdullah-9211/Spillway.git`, branch `main`). Never commit `.env` files or secrets. Say in the Verify message what was pushed.

## Open points

- The Go module path is `github.com/abdullah-9211/spillway` (lower case). The GitHub repo is named `Spillway`; GitHub treats the two as the same repository.
- Check that the name "Spillway" is free on GitHub and pkg.go.dev before creating the remote repository.
- The final design has no screen for the tool registry. Phase 12 manages tools through the API and CLI.
- Model prices in `config/models.yaml` are placeholders until filled with current provider prices.
