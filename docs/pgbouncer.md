# PgBouncer

ContentKit's queries can go through PgBouncer in transaction mode. What needs one server session for longer than a transaction goes straight to PostgreSQL.

## Engine

- `Deps.Postgres` is the PgBouncer pool. Set `DefaultQueryExecMode` to `cache_describe` (`default_query_exec_mode=cache_describe` in its URL): `New` refuses `cache_statement` and `describe_exec`, which run a statement's prepare and execute in separate round trips that PgBouncer may give to different server connections.
- `Deps.PostgresSession` is a `pgxpool.ParseConfig` of the same database and role at PostgreSQL's direct endpoint. ContentKit dials and closes these connections itself:
  - River's `LISTEN`: one connection in each process that runs `StartJobs`. It only wakes workers; they also poll the job table, so a lost notification delays a job and loses none;
  - source and video locks: one shared session while any is held, plus one per waiter on a key someone holds;
  - migrations: a temporary pool while `New` migrates.
- Without `PostgresSession` all of this uses `Deps.Postgres`, which must then be direct (or a session-mode pooler).
- `MigratePostgres` and `MigrateClickHouse` hold session locks: pass them a direct pool.

## Media worker

`DATABASE_URL` may be the PgBouncer URL when `DATABASE_SESSION_URL` is the direct one. The worker then uses `cache_describe` unless the URL names another mode.

## PgBouncer settings

- `pool_mode = transaction`.
- `max_prepared_statements` may be anything: ContentKit prepares no named statement. CI runs with `0`.
- Keep startup parameters PgBouncer does not track (`search_path`, `options`) out of the pooled URL, or list them in `ignore_startup_parameters`.
- Size PostgreSQL's `max_connections` for PgBouncer's server pools plus the direct sessions above.
