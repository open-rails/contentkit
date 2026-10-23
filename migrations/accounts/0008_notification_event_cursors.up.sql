-- Shared account inbox replicas advance this cursor with their inbox writes.
CREATE TABLE IF NOT EXISTS notification_event_cursors (
    namespace text NOT NULL,
    system text NOT NULL,
    origin_scope text NOT NULL,
    cursor text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (namespace, system, origin_scope)
);
