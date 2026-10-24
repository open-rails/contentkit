# Public media and processing events

Public/private storage is a property of the declared media preset. Public
covers and previews remain public before release and after unpublishing.
Private originals and reader pages continue to require scoped tokens.
Publication, billing and access changes do not copy, hide or regenerate media.
Explicit replacement/removal and permanent purge still retire obsolete files.

The processor records meaningful progress and completion in the retained event
log before its processing job completes. Current status remains available
through the existing media HTTP and Go interfaces. Observers own their cursors;
retention expiry requires a fresh current-state read.

Completion also wakes the existing version metadata worker in the same database
transaction. That worker records media facts, refreshes listing facets, builds
immutable reader snapshots and applies the selected cover. It performs real
metadata work, including before unrelated processing finishes. There is no
readiness delivery job or source-managed event consumer.

This change requires a stopped-writer upgrade: stop older hosts and media
workers before applying the new migrations and restart the matching release.
Published migration files must not be rewritten. The upgrade must retire old
waiting exposure/readiness jobs without losing pending metadata work.

Implementation and verification are in progress under tracker ContentKit #247.
