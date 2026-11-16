---
status: accepted
---

# One instance per developer, no shared team instance

Kiln is for developers, but each developer runs their own instance; an instance never serves more than one person. Agent runs use that developer's own Claude login and billing, and a shared instance would need a service or technical account to hold a token and carry the cost, which most developers cannot get. A team that wants Kiln runs one instance per person.

## Consequences

- No accounts, roles or per-user permissions inside an instance.
- Nothing in an instance is designed to be seen or edited by a second person at the same time.
