---
status: accepted
---

# A small launcher judges every update, and the store is copied before it

ADR 0019 requires that a failed update undoes itself, and a new version that crashes or hangs on start cannot be the one that notices. A small launcher therefore stays running as the core's parent. It starts the core; after an update it waits for the new core to report healthy, and if that report does not come in time it starts the previous version and tells it that the update failed. The previous version then turns automatic merging off and needs the developer, as ADR 0019 says.

1. **The launcher does nothing else.** It does not build, touch the store, talk to the container engine or serve anything. The core prepares the new version and asks to be replaced.
2. **Healthy is the core's own report** after one full comparison of the store, the container engine and the Git host (ADR 0035), with the UI being served.
3. **The launcher is outside automatic update.** Its code is a protected path in Kiln's repo (ADR 0006), and a changed launcher takes effect only when the developer restarts Kiln by hand.

## The store during an update

A new version may change the shape of the store, and the previous version does not know the new shape. ADR 0019 did not say what happens to the store when an update is undone.

1. **Migrations go forward only.** The new core applies them when it first opens the store.
2. **The old core copies the store** just before it asks to be replaced, and puts the copy back itself when it is started again after a failed update. The launcher never touches the store.
3. **Until it has reported healthy, a new core changes nothing outside the store.** It converts the store, reads the container engine and the Git host, and serves the UI without accepting actions; it starts no run, removes no container and publishes nothing. Putting the copy back therefore loses nothing, because everything outside the store is still there to be compared again (ADR 0035).
4. **A core refuses to open a store newer than it knows,** so a copy that was not put back correctly stops the previous version with a clear message.
5. **The copy of the last update is kept,** beside the version of the core that made it, and the one before it is deleted at the next update. Kiln itself only ever goes back one step.

A failed update is therefore what ADR 0019 says it is: a new version that does not start properly. A fault that shows once the new version is acting is an ordinary bug and reaches the developer by the usual rules.

## Considered options

- **Migrations with a reverse step, run on undo**: no copy. Rejected because only the new version contains the reverse step and it is the one that just failed, each reverse step would run for the first time during a failed update, and a dropped column cannot be reversed.
- **Only changes the previous version can still read**: nothing is ever lost, but it is a rule on every card that touches the store, enforced by nobody, whose breach shows only during a failed update.
- **The old core judges the new one**, starting it beside itself and exiting once it is healthy: no extra part, and the judge is the last known-good version. Rejected because two live cores would hand over the store and the port, against the single writer of ADR 0032, and nothing would restart the core after an ordinary crash.
- **The operating system restarts the core and a start counter decides**: no supervisor of Kiln's own, and it survives a reboot. Rejected because it cannot see a hang, and the decision would sit in a wrapper script or in the possibly broken new version.

## Consequences

- The launcher also restarts the core after a crash that has nothing to do with an update.
- The launcher can fall behind the core, since it changes only by hand.
- The launcher belongs to the local runtime. On Kubernetes its job falls to the rollout, which is designed when Kubernetes arrives (ADR 0019); the launcher is not shaped for that now.
