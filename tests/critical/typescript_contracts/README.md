# Public TypeScript contract workflows

This child extends the approved critical suite through the built
`@openrails/contentkit-ui` client and real ContentKit HTTP routes. The Go fixture
supplies real AuthKit identities and owned PostgreSQL/S3/ClickHouse/OpenRails
resources through existing public test helpers. Node executes framework-free
client contracts; browser tests remain responsible for browser-only behavior.

Work is grouped by domain: publication/discovery, moderation/bans, entities,
notifications, and media. Each selected method needs an asserted returned value
or persistent state effect, plus applicable authority/error controls. Merely
calling a method, counting calls, or obtaining an unauthenticated refusal does
not establish its functionality.

The parent owns coverage.json and final scenario/operation/binding qualification.
This child supplies exact executable mappings and passing receipts. The media
resilience child owns ItemUploadQueue and uncertain-commit/resume behavior;
this child owns basic media method return contracts and the remaining public
TypeScript workflows. Existing browser drivers and shared fixtures remain under
their current owners unless coordinated explicitly.

The final child contains eight workflows covering 62 additional selected bindings:
publication/catalog (10), moderation/bans (12), discovery/entities (14), inbox
receipts (4), image I/O (13), named/inline images and download (3), abort/discard
(2), and video/HLS/native browser transport (4). The parent supplies the existing
value/scope/browser contracts; the media resilience child supplies queue and
multipart-resume contracts and publishWhenReady's actual not_ready retry.

Named and body images use an ordinary post's media kind. Gallery publication
serves its declared page/playback snapshot; its ZIP download is tested through
that gallery contract. Edits retain original source bytes and published access,
and locking a post hides its actual body-image URLs.

Only static built SDK/vendor assets are fulfilled by the transport browser
harness. API, gateway and S3 requests reach real local services. Chromium's local
network permission is granted only to the fixture origin so it can reach those
loopback services; normal CORS and browser security remain enabled. A real gateway
proxy records whether API credentials leak on off-origin HLS requests. JPEG
frame dimensions are decoded independently in Go.
