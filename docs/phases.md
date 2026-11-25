# Phases

Status: closed on 2026-11-13, at the end of the design session. The POC list has eight items, one more than the "five to seven" of ADR 0004, accepted as it stands.

The rules for this list are in [ADR 0004](adr/0004-kiln-first-then-the-work-repo-by-phase-list.md): items are outcomes that can be checked by watching; the list is closed at the end of the design session; adding an item later means removing one or recording the reason as a decision; when the POC list is checked off, the next work is the work repo and nothing new is built for Kiln itself before it.

## POC: the flow works on Kiln's own repo

1. **A card goes from idea to merged with one approval.** The developer writes a card for Kiln's repo and approves its spec. The pull request is published, passes its checks and merges, and the card is done, without the developer doing anything else.
2. **Kiln updates itself.** After that merge the running instance is the new version, with no action from the developer, and a run that was in progress during the restart finishes normally.
3. **The review loop ends by itself or stops cleanly.** When review finds a real problem, the card returns to implement and comes through without the developer. After two send-backs it stops and shows the findings under "needs you".
4. **A broken run is contained.** A run that is killed is retried once; a second failure stops the card under "needs you". With the Claude credential unavailable, the instance pauses and says so once instead of failing every card.
5. **The guardrails hold.** A failing check keeps the Git host from merging. A change to a protected path waits for the developer's approval on the Git host. A run cannot read the developer's home directory, reach a host that is not on its list, or obtain a credential.
6. **Limits and order are enforced.** With four cards started, three are in progress and the fourth waits and then starts by itself. A card with a predecessor starts only when the predecessor is done.
7. **Kiln reports its own measures.** From its own records it shows interventions per merged card, the cards that needed a follow-up fix, and the cards dropped.
8. **Proposals reach the developer.** A problem review noticed outside a card's change appears on the first screen, and one action turns it into a backlog card.

Every POC item was seen working on 2026-11-25; the MVP list is next (ADR 0004).

## MVP: it works on the work repo, for the developer and at least one colleague

1. **A card for the work repo is published and tracked.** The developer approves the spec and gives the card a work item number. Kiln opens the pull request on Azure DevOps with the work item linked, and the card is done once colleagues have approved and it has merged.
2. **Human review comes to the developer first.** When a colleague comments or asks for changes, the card appears under "needs you" with the comments, and one action sends it to implement and publishes the update.
3. **A pull request that cannot merge goes back by itself.** A conflict or a failed pipeline of the work repo returns the card to implement without the developer.
4. **The work repo's own configuration applies and nothing of Kiln's is added.** Runs there follow its instructions, skills and hooks, and no file of Kiln's is committed to it.
5. **A sealed run can build and test the work repo.** Inside its container an agent can build the work repo's code and run its tests.
6. **A colleague runs their own instance.** The whole path of a card passes an automated test on k3s; a colleague installs Kiln from the instructions without help and gets a card through on the work repo.
7. **Two repos share one instance.** With cards in Kiln's repo and in the work repo, the limit per repo and the limit per instance both hold.

## Later, each as its own decision

Not on either list, and not built until decided with what the earlier phases have shown:

- Kiln creating cards by itself, beyond proposals the developer accepts (ADR 0005, ADR 0033).
- A supervising agent (ADR 0016).
- Skipping the approval for kinds of card that never needed it (ADR 0008).
- Acting on some human review comments without the developer (ADR 0028).
- A limit per stage (ADR 0014).
- A budget on the CPU time of checks (ADR 0031).
- One notification channel (ADR 0022).
- Model calls outside a run (ADR 0020).
- A richer UI and a new visual design, in its own design session (ADR 0022).
- Going public on GitHub, including how users of a public Kiln sign in to Claude (ADR 0020, ADR 0026).
- Removing Azure DevOps support once the work repo has moved (ADR 0027).
