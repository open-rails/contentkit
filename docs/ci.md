# CI execution

- Branch pushes: build and vet only. Superseded runs are cancelled.
- Pull requests: one Go test suite with its native dependencies; apps also run one frontend test/lint/build job. The injected-code scan remains a separate merge check.
- Source-derived contract drift stays covered by Go tests; the app frontend build typechecks consumers. No duplicate contract/frontend installation job.
- Expensive full-stack/browser qualification is explicit workflow_dispatch or a local command, not an automatic per-merge run. Deferred environment-dependent tests are named explicitly in the strict runner allow-list.
- App Docker publishing runs on version tags or workflow_dispatch, retaining native multi-architecture images and smoke qualification. No image rebuild after each source commit.
- These are execution-policy changes, not measured timing claims or weaker correctness assertions. Do not treat a manual suite as already passed.
