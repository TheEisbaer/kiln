---
status: accepted
---

# Opus for spec and review, a model proposed per card for implement, Fable only as an advisor

Which model a run uses depends on its stage. Choosing a tier for every run with a separate judging call would be a model call outside a run. Kiln makes none (ADR 0020), so the choice is made inside the spec instead.

1. **Spec and review run on Opus.** They are the stages where a mistake reaches the developer.
2. **The spec proposes the model for implement** from Opus, Sonnet and Haiku. The proposal is a line in the spec, so the developer sees it at approval and can change it.
3. **A send-back moves implement to Opus.** If review sends the card back, the next implement round does not repeat a cheaper choice.
4. **Fable is not a stage model.** A run that is stuck on a judgement call can consult it as an advisor, at most twice per run.
5. **Models are named by exact identifier,** in the instance, never by alias. An alias can stop resolving, or resolve to another model, without anything in Kiln having changed.
6. **Kiln records, per card, the proposal, any advisor consults and the outcome,** so the rule can be changed on data: how often a cheaper choice passed review first time.

## Effort

Effort is set per stage, starting from Anthropic's defaults for Claude Code (`medium` on all current models) with two deliberate exceptions. Kiln's runs are unattended, and the documentation describes the lower levels as fitting work where a person reviews each result and steers, and the higher ones as verifying more and deciding more alone.

| Stage | Model | Effort | Why |
|---|---|---|---|
| Spec | Opus | `high` | a wrong spec is the most expensive mistake in the flow |
| Implement | proposed per card | `medium` | an approved spec is a clear scope |
| Review | Opus | `high` | verification and edge cases are the job |
| Advisor | Fable | `high` | consulted rarely and only on hard calls |

- Effort is fixed per stage and not proposed per card. The spec already proposes the model; if effort varied per card as well, the recorded data could not show which of the two mattered.
- Kiln always passes the effort explicitly. Claude Code's defaults have changed between model versions, and a changed default must not change how Kiln behaves.
- Nothing above `high` without a test. Higher levels cost more time and budget per run, so they would hit the ceilings of ADR 0029 more often.
- The effort of each run is recorded with its model. The scale is calibrated per model, so a level is only comparable within one model.

## Considered options

- **The most capable model for every stage**: rejected by the developer as too much; it would mean Fable for everything.
- **A fixed model per stage, including implement**: simpler, but gives no data on which cards a cheaper model handles.
- **A router with a judge call per run**: rejected; it needs a call outside a run, and its main benefit is saving money, which is not a goal (ADR 0007).

## Consequences

- The spec agent chooses without data at first and will sometimes guess too low. Rule 3 makes that cost one extra implement round rather than the developer's attention.
- The advisor is something Kiln gives every run, not something read from the target repo.

## Settled later

- That a send-back happened is read from the number of send-backs on the card (ADR 0054). A session that is resumed after a send-back continues on the stronger model (ADR 0053).
- The core compares the model that a run's first event, `system/init`, names with the one it gave. A run on another model is stopped, saved and recorded with the model seen, and counts as failed. Claude Code 2.1.326's output does not show the effort, so the effort recorded is the one given.
