# Domain rules define each content kind's visible catalog

Each domain owns a policy per content kind. Requirements and optional filters
belong in the same kind policy, so a gallery restriction cannot accidentally
filter blog posts or another content type. YAML uses content kind names directly
beside each domain's `name` and `hosts`; `gallery` and `blog` need no `kinds` wrapper.
Omitted kinds remain unfiltered.

Locked hidden entities and required entities are server-enforced boundaries
for matching content, including staff public reads and direct media access.
Optional reader filters affect discovery. Internal entity metadata stays
private across all content kinds; marking an entity internal alone does not
hide its associated content everywhere.

Reader choices are scoped by content kind and entity. The current domain
selects applicable controls, and saving them preserves choices for other kinds
and domains. Old unscoped choices remain a fallback until a scoped choice or
reset supersedes them.
