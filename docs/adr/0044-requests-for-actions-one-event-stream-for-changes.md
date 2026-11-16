---
status: accepted
---

# The UI sends requests; the core tells it about changes over one one-way event stream

The UI reads and acts through plain requests over HTTP, and learns about changes through one one-way event stream from the core. The developer wants the UI to react to a change at once, for example to animate a card moving, which asking again every few seconds cannot give. The stream delivers the history entries that are written for every change anyway (ADR 0042), in order and with a running number, so no second kind of event exists and nothing has to be kept in step.

1. **Events tell, requests give the truth.** An event says which card changed, what changed and who caused it, which is enough to animate the specific change. The UI then fetches the card by request; it never builds its state from events alone.
2. **The stream resumes.** The core restarts after every merge to Kiln's own repo (ADR 0019). On reconnecting, the UI asks for everything after the last entry it saw and misses nothing.
3. **One description of the API, owned by the core.** The UI's types are generated from it, and a check fails when they are out of date (ADR 0023).
4. **The UI is built into the core's program,** so one version is one file and update and undo (ADR 0036) cover the UI.
5. **The API is the only way to change the store.** Any later tool calls it and never opens the store itself (ADR 0032).
6. **The API listens only on the developer's machine and accepts actions only from its own page.** Otherwise any web page open in the developer's browser could send an approval, which is the main thing standing between a card and an automatic merge (ADR 0008).

## Considered options

- **Requests only, with the UI asking again every few seconds**: the smallest, and a restart of the core needs no handling. Rejected because changes would show with a delay of seconds.
- **A two-way connection (WebSocket) with state pushed to the UI**: the fastest UI. Rejected because nothing on the POC list sends a stream from the UI to the core, state held in the browser would have to be correct for every kind of entry forever, and browsers do not protect such connections by origin.
- **A typed remote-call framework**: agreement between the sides built in, at the price of a framework for three screens.

## Consequences

- There is one way to learn about changes; the UI does not also poll.
- Watching a run's output live, if it is wanted, can use the same kind of stream. Talking to a run would need the two-way connection and is its own decision (ADR 0020).
