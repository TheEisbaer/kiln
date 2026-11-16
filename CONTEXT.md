# Kiln

Kiln takes a developer's ideas and turns them into changes in the repos that developer works in, with agents doing the work between idea and change.

## Language

### Who and where

**Developer**:
The one person an instance belongs to and works for.
_Avoid_: User, owner, team

**Git host**:
The service that holds a target repo, runs its checks and governs its merges.
_Avoid_: Remote, forge, provider, platform

**Instance**:
One running Kiln, belonging to exactly one developer.
_Avoid_: Board, workspace, tenant

**Target repo**:
A repo the developer works in and has pointed their instance at; changes land there.
_Avoid_: Project, workspace

### Cards

**Card**:
One idea, written by the developer or accepted by them from a proposal, small enough to become exactly one pull request in exactly one target repo.
_Avoid_: Ticket, issue, task, story

**Proposal**:
A concrete problem a reviewing agent noticed outside a card's change, offered to the developer, who can turn it into a card or dismiss it.
_Avoid_: Suggestion, remark, finding, auto-card

**Predecessor**:
A card that another card names as having to be done before it may start.
_Avoid_: Dependency, blocker, parent

**Split**:
Replacing an idea too big for one card by several separate cards.
_Avoid_: Epic, subtasks, breakdown

**Origin**:
The card a split-off card came from; a reference only, with no state of its own.
_Avoid_: Parent, epic

### Stages

**Stage**:
One of the five steps a card passes through in order; each has a result, after which the card may wait for someone.
_Avoid_: Lane, column, phase, status

**Backlog**:
The stage of a card that is written and not yet started.
_Avoid_: Idea, inbox, todo

**Spec**:
The stage in which an agent works out what will be built, including whatever research that takes. Also the document it produces, which the developer approves.
_Avoid_: Research, plan, design, proposal

**Implement**:
The stage in which an agent writes the change.
_Avoid_: Build, code, develop

**Review**:
The stage in which an agent with a fresh context judges the change and either passes it or sends it back to implement.
_Avoid_: QA, check, verify

**Done**:
The stage of a card whose pull request is merged.
_Avoid_: Closed, finished, shipped

### What happens to a card

**Queued**:
The state of a backlog card the developer has released but which is waiting for its predecessors or for room under a limit.
_Avoid_: Pending, blocked, waiting

**In progress**:
A card that has left the backlog and is not yet done or dropped.
_Avoid_: Active, in flight, WIP

**Limit**:
The most cards that may be in progress at once, set per target repo and per instance, and enforced by Kiln.
_Avoid_: Cap, quota, WIP limit

**Approval**:
The one planned intervention on every card: the developer agreeing to the spec, before any code is written.
_Avoid_: Sign-off, confirmation, go

**Intervention**:
An occasion on which a card needs the developer between being started and being merged.
_Avoid_: Interaction, prompt, touch

**Run**:
One attempt by an agent at a card's current stage.
_Avoid_: Job, session, task, execution

**Sealed**:
Said of a run that can reach nothing on the developer's machine, no credentials and no network beyond what Kiln explicitly hands it.
_Avoid_: Sandboxed, isolated, jailed

**Ceiling**:
A bound on a run (elapsed time, time without output, or budget) at which Kiln ends it.
_Avoid_: Timeout, cap, limit, quota

**Finished**:
Said of a run that produced its stage's result, whatever that result says.
_Avoid_: Succeeded, passed, completed

**Failed**:
Said of a run that ended for a reason that is not about the code, such as a lost connection or a vanished container.
_Avoid_: Crashed, errored, broken

**Withdrawn**:
Said of a run Kiln ended because its card no longer needs it.
_Avoid_: Stopped, cancelled, aborted, killed

**Needs you**:
The state of a card that has stopped and waits for the developer outside the planned approval.
_Avoid_: Attention, blocked, stuck, needs human

**Paused**:
The state of an instance in which no runs are started, because something every run depends on is failing.
_Avoid_: Halted, down, stopped

**Standard**:
A written rule of a target repo that its code must meet; given to the implementing agent and enforced by review.
_Avoid_: Guideline, convention, best practice

**Check**:
A requirement on a change that a tool decides, such as tests, lint or types.
_Avoid_: Gate, validation, pipeline

**Finding**:
A reason review gives for sending a card back; it must point to the spec, a concrete failure or a standard.
_Avoid_: Remark, comment, issue, nit

**Send-back**:
Review returning a card to implement because of a finding.
_Avoid_: Rejection, revision, bounce

**Save**:
Putting a card's work on that card's branch in the instance's own copy of the target repo, after every implement round.
_Avoid_: Checkpoint, commit, snapshot

**Publish**:
Putting a card's work on the target repo's Git host as a pull request, once review has passed.
_Avoid_: Push, submit, open

**Dropped**:
The ending of a card that will not produce a change, confirmed by the developer; the other ending is done.
_Avoid_: Archived, cancelled, closed, rejected

**Ready to merge**:
The state of a card whose review has passed: Kiln has nothing left to do, and what remains is the merge, which the target repo's Git host governs.
_Avoid_: Finished, done, shipped

### Merging

**Automatic merging**:
A target repo's opt-in under which a ready-to-merge pull request merges without the developer, provided the guardrails hold.
_Avoid_: Auto-merge, self-merge

**Automatic update**:
An instance replacing itself with the newly merged version after a card in Kiln's own repo merges.
_Avoid_: Self-checkout, self-release, auto-deploy

**Protected path**:
A path in a target repo, configured for that repo, whose changes always need the developer and never merge automatically.
_Avoid_: Sensitive path, owner path
