CREATE TABLE users (
    uid          TEXT PRIMARY KEY,
    email        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE devices (
    uid          TEXT NOT NULL REFERENCES users (uid) ON DELETE CASCADE,
    device_id    TEXT NOT NULL,
    name         TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    app_version  TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (uid, device_id)
);

CREATE SEQUENCE records_seq;

CREATE TABLE records (
    uid        TEXT NOT NULL REFERENCES users (uid) ON DELETE CASCADE,
    tbl        TEXT NOT NULL,
    pk         TEXT NOT NULL,
    data       JSONB,
    deleted    BOOLEAN NOT NULL DEFAULT false,
    hlc        BIGINT NOT NULL,
    device_id  TEXT NOT NULL,
    seq        BIGINT NOT NULL DEFAULT nextval('records_seq'),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (uid, tbl, pk)
);

CREATE INDEX records_uid_seq ON records (uid, seq);
