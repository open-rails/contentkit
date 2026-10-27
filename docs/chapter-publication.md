# Item access and durable chapter publication

Implementation of [ContentKit tracker #250](https://github.com/open-rails/tracker/blob/master/contentkit/0250-item-access-and-chapter-releases.md).
This document describes the target; the integration remains in progress.

Each gallery edition is an independently identified content item. Its exact
`private` folder is an all-or-nothing token scope. Access to one edition never
grants access to another edition of the gallery. Public files need no token;
reader tokens cannot open `tmp`, `originals`, `internal` or manifest files.

An explicitly authorized read-only auditor can request administrative
inspection of ready reader derivatives before release. These requests return
short-lived exact preview URLs, never a folder cookie, editing controls,
undeclared original uploads or `HostOnly`/download-only outputs. Declared reader
audio and explicit non-ZIP `ServeOriginals` assets may pass through; this does
not grant general source-export authority. Reader-plan inspection segments and
playback carry an explicit flag so media fetches repeat the permission check.
HLS inspection reads only indexes referenced by allowed current tracks and
uses exact preview URLs for their bytes. Inspection does not create publication
records or change release state.

Chapters are start markers in the manifest's ordered source files. A chapter
covers `[its start, next start)`; the last chapter continues to the end. Markers
use existing virtual filenames. Inserting `17c.jpg` before the next chapter's
`20.jpg` marker puts it in the earlier chapter automatically. There is no
separate membership list or new per-file identifier. Generated low-resolution,
thumbnail and other rendition files follow their source file.

Chapter definitions and schedules are in PostgreSQL. The manifest keeps
virtual filenames, original-name provenance and immutable object references;
S3 holds the bytes. Preserve the existing hash-derived storage allocations and
their lifetime protection. Renames update marker references, and removing a
marker requires an explicit replacement rather than guessing another file.

Uploads enter temporary storage, originals are retained, and prepared outputs
wait in `internal`. One durable publisher reconciles actual placement with the
current chapter order, released chapters and item access policy. It copies and
verifies destinations before adopting them and retiring obsolete sources.
Inserted files in a released chapter are published; files moved into a future
chapter return to `internal`. Originals are retained independently.

Scheduled publication and immediate publication use the same durable work.
Passing a date alone does not release content. Reader metadata changes after
storage work completes; a retry resumes the recorded attempt. A copied file
can be fetched directly while the rest of an operation is still copying:
folder access does not imply atomic multi-object S3 exposure.

Withdrawal and public/private changes use the same reconciliation. Exact-key
retirement, retained references, late writes and existing temporary expiry must
remain correct. Existing public caches and already downloaded bytes cannot be
retracted by a database visibility change.

Downloads of already served files use their public/private gateway paths.
Retained originals and `HostOnly` outputs use the existing signed archive
transfer endpoint in single-file mode, with access, completed-source revision
and host download policy checked again at transfer time. These transfers are
uncached binary attachments; the grant retains the original MIME type and
filename. They never issue a reader token or gateway URL for internal storage.
Uploaded source ZIPs are never reader downloads, even when selected as a
literal download file: they can contain future chapters. Reader ZIPs are
assembled only from the completed released set; editors use their separate
original-file interface.
Same-rung MP4 presets have separate allocations so publishing an ordinary
preset cannot move a `HostOnly` preset into a served area. This cannot prevent
saving equivalent bytes already available through an ordinary served preset.

Migration must preserve source bytes and original IDs, explicitly map old
chapter hashes/ranges to virtual paths, and convert parent-wide entitlement
grants to exact edition rights. Unresolved mappings refuse cutover. Old served
paths cannot remain a bypass to originals or unreleased media. Published SQL
migration bytes remain immutable.
