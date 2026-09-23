# CI execution

- Branch pushes: build and vet only. Superseded runs are cancelled.
- Pull requests: one Go test suite (`go test ./... -race`) against real PGroonga Postgres and ClickHouse+Keeper; pgvector is installed only so the legacy-lineage convergence test can apply the pre-ContentKit baseline. The injected-code scan remains a separate merge check, run by the shared [open-rails/helpers workflow](https://github.com/open-rails/helpers/blob/master/docs/injection-scan.md) pinned by commit SHA.
- Expensive full-stack qualification is explicit `workflow_dispatch` or a local command, not an automatic per-merge run.
- These are execution-policy choices, not measured timing claims. Do not treat a manual suite as already passed.
