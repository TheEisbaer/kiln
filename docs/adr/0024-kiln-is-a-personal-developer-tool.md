---
status: accepted
---

# Kiln is the developer's personal project and a developer tool, never a customer product

Kiln belongs to the developer personally. They use it at work and may share it with a few colleagues, each running their own instance (ADR 0001), but it is not their employer's project and will never be offered to that employer's customers, paying or not. "Public later" (ADR 0003) means available to other developers as a tool, not a product.

## Consequences

- Where Kiln's repo lives is the developer's choice, not the employer's.
- Nothing company-specific goes into the repo: no company names, hosts, identifiers or credentials. What a setup needs from a company (for example how a Claude credential is obtained) stays outside Kiln, in that setup.
- Colleagues are users with their own instances, not co-owners.
