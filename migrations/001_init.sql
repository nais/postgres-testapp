CREATE TABLE events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    instance text NOT NULL,
    kind text NOT NULL,
    message text NOT NULL
);

CREATE INDEX events_created_at_idx ON events (created_at DESC);
