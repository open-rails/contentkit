# Item access and durable chapter publication

Each gallery edition is an independently identified content item. Its exact
`private` folder is an all-or-nothing token scope. Access to one edition never
grants access to another edition of the gallery. Public files need no token;
reader tokens cannot open `tmp`, `internal` or manifest files.

An explicitly authorized read-only auditor can request administrative
inspection of ready reader derivatives before release. These requests return
short-lived exact preview URLs, never a folder cookie, editing controls,
undeclared original uploads or `HostOnly`/download-only outputs. Declared reader
audio may pass through; this does
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
S3 holds the bytes, named by their hash. Renames update marker references, and removing a
marker requires an explicit replacement rather than guessing another file.

Uploads are staged in `tmp`, then placed under their hash in `internal`, where
prepared outputs wait. The release job reconciles placement with the current
chapter order, released chapters and item access policy: it copies files to
their served area, writes the manifest naming the copies, and retires the old
ones. Inserted files in a released chapter are released; files moved into a
future chapter return to `internal`. Page originals stay in `internal`.

Scheduled publication and immediate publication use the same durable work.
Passing a date alone does not release content. Reader metadata changes when
the manifest names the copies; a retry re-runs from the manifest. A copied file
can be fetched directly while the rest of an operation is still copying:
folder access does not imply atomic multi-object S3 exposure.

Withdrawal and public/private changes use the same reconciliation; files moved
out of a served area are retired and deleted at once. Existing public caches and already downloaded bytes cannot be
retracted by a database visibility change.

A video's download streams its master (the original, by default) through the
API from where it is stored; originals never move. `HostOnly` outputs stay in
`internal` and reach readers only inside the ZIP. Uploaded source ZIPs are never reader downloads:
they can contain future chapters. Reader ZIPs are assembled only from the
released set; editors use their separate original-file interface.

Migration must preserve source bytes and original IDs, explicitly map old
chapter hashes/ranges to virtual paths, and convert parent-wide entitlement
grants to exact edition rights. Unresolved mappings refuse cutover. Old served
paths cannot remain a bypass to originals or unreleased media. Published SQL
migration bytes remain immutable.
