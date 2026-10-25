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
