---
status: accepted
---

# Cards record what they settle in one file, and only the developer changes a decision

A card often settles a detail that a decision left open: which file format the store uses, which flags a run's container gets, where a function lives. Until now a card wrote such a line into the decision itself, under "Settled later". The decisions are a protected place (ADR 0047), so every card waited for the developer's approval on the Git host, even when all its code was on free paths. From the second card on, every merged card changed between three and six decisions for such lines, and that approval became the most common intervention (ADR 0007).

Cards therefore write what they settle into one file, `docs/settled.md`, which is not protected. It has a section per decision, by number. A file in `docs/adr/` is changed only by the developer, in the developer's own pull request.

1. **The file is the permanent home of these lines.** Nothing is copied back into a decision later, so there is no second copy that can be out of date. The lines that decisions carried under "Settled later" were moved there on 2026-11-18.
2. **A decision says what was decided, and the file says what cards settled under it.** Whoever reads a decision reads its section of the file with it. The instructions for the spec, implement and review stages name both.
3. **The lines are judged like the rest of a card's change.** The spec lists them, the developer approves the spec, and review sends a card back when a line says more or less than the spec (ADR 0015).
4. **A line that would change what a decision says is not such a line.** It is a question for the developer, and the answer is a change to the decision or a new one.
5. **A decision is changed in one way.** When a later decision reverses or revises an earlier one:
   - the new decision states what is decided now and lists what it changes in earlier ones;
   - the earlier decision keeps its text, and gets the status "superseded by" or "superseded in part by" with the new one's number, and one line at its top saying what no longer holds;
   - its section in `docs/settled.md` gets a note saying which of its lines no longer hold;
   - every place that cites the earlier decision, in the decisions, `CONTEXT.md`, `docs/open-points.md` and `docs/settled.md`, is looked up and checked.

## Considered options

- **Cards keep writing into the decisions**: the developer sees every such line before it lands, at the price of an approval on every card, also on those whose code needs none.
- **Cards write into a file, and the developer copies the lines into the decisions from time to time**: the decisions stay the one place to read, but they are out of date between two rounds of copying, and the copying is work that nothing enforces.
- **The decisions no longer protected**: an agent could then change a decision by itself, against ADR 0015, rule 5.

## Consequences

- A card that touches no protected place merges without the developer once its check has passed (ADR 0006), which the POC's first outcome asks for.
- There are two places to read for one decision. A decision does not show what was settled under it; the file does, under the decision's number.
- An agent can write a wrong or misleading line into the file without the developer's approval. Review against the spec is what catches it, as it did on 2026-11-17, when a card's line said that a run "counts as failed" and its spec had only said that the command exits non-zero.
