---
paths:
  - "wiki/**"
  - "docs/**"
  - "README.md"
---

# Wiki / docs conventions

- Public repo: **no** real domains, IPs, node hostnames, emails, tokens. Use `<internal-domain>`, `<node-name>`, `<vlan-ip>`.
- Keep every heading's `{#anchor}` id; keep relative links valid; blank line before every table.
- New page → add to `nav:` in `properdocs.yml` and link from the section's neighbours.
- Incidents go in `wiki/05-operations/known-issues.md` as Symptom / Root cause / Fix; TODOs in `wiki/06-todo/`.
- Edit minimally in the existing tone; verify facts from the repo before writing them.
