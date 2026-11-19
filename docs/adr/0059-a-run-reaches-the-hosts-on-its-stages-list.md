---
status: accepted
---

# A run reaches the hosts on its stage's list, through a way out that holds no credential

A spec carries the research (ADR 0009), and research that rests on what a model was trained on is not research: it has to read what is written now, in documentation, in issue trackers and in other people's code. Until now a sealed run reached the proxy that carries its requests to Claude, and nothing else (ADR 0017, ADR 0041). So a spec run could not look anything up, and the specs written by hand for Kiln's own cards all had.

A run is therefore created with a list of hosts.

1. **An empty list is the seal as it was.** The run reaches the proxy and nothing else.
2. **A host on the list can be reached over HTTPS, and `*` stands for every public host.**
3. **There is one list per stage, and it is kept in the instance** (ADR 0025), never in the repo. A list in the repo could be widened by the card whose runs it applies to.
4. **Only the spec stage has a list for now.** Implement and review runs stay as they were. Kiln's checks run with the network off, so implement has no use for one yet. A list for review would be a guardrail, since a page that steers a review run can pass a bad change (ADR 0006).
5. **Whatever the list says, a run reaches nothing that is not public:** not the developer's machine, not the virtual machine the container engine runs in or anything else on it, not another run. This is decided on the address a connection is made to, after the name is resolved, and not on the name.
6. **A run's way out is not the proxy.** The proxy holds the Claude credential and carries nothing but requests to Claude. The way out holds no credential, belongs to one run, and is made and removed with it.
7. **Nothing is read or changed inside a connection.** A run that can reach a host can also send to it. A list narrows what can leak and does not stop it.

## What this changes in earlier decisions

- **ADR 0041, point 2:** "Every run gets a network of its own, shared only with the proxy." A run with a list shares its network with its way out as well.
- **ADR 0038:** "a run has no network but the proxy". A run with a list also reaches the hosts on it.
- **ADR 0034:** "Kiln has three kinds of process." A way out is a fourth: one container per run that has a list, for as long as that run exists.
- **ADR 0047:** the repo is "divided by the parts of ADR 0034: the core, the proxy and the launcher". The way out is a part as well, and its program is a place of the seal.
- **`docs/phases.md`, POC outcome 5:** "A run cannot … reach a host other than the proxy" becomes "reach a host that is not on its list". The list is closed (ADR 0004), and this decision is the recorded reason.

ADR 0017 holds as written: "no network except a proxy outside the container, which allows only listed hosts".

## Considered options

- **No web for spec runs, with what could not be checked marked as unclear:** the seal untouched. Rejected by the developer on 2026-11-18: a spec without the web is not research.
- **Claude Code's web search alone:** it runs on Anthropic's side and needs no way out, but it returns titles and addresses, not pages.
- **A list of named hosts only, without `*`:** a smaller surface. Research stops at the list's edge, and the answer is rarely on a host that was listed in advance.
- **The way out inside the proxy:** one container fewer, but every web connection would pass through the process that holds the credential.
- **A tool outside the seal that fetches a page for a run, read only:** a run could read and not send. It is the most to build, a run would use Kiln's tool in place of Claude Code's own, and an address can still carry data out.

## Consequences

- A page can steer a run that reads it. What a steered spec run can do is write a bad spec, which the developer reads before approving (ADR 0008).
- For Kiln's own repo a run holds nothing secret: the code is public and no credential is inside. For a repo whose code is private, `*` lets a run send that code anywhere, so the list for such a repo is asked before its first card.
- A host with a public address that trusts the developer's machine by where it sits, such as a service behind a VPN, is reached by a run as the developer would reach it, without the developer's sign-ins.
- Which hosts a run reached is kept in the run's record, so that a narrower list can be decided on what runs really read.

## Revisit when

- The first card for a repo whose code is private is written.
- A stage other than spec needs a list.
- The records show that spec runs read a small, stable set of hosts.
