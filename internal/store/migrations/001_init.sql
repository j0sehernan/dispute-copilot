CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE disputes (
    id               uuid PRIMARY KEY,
    source           text        NOT NULL,
    input_kind       text        NOT NULL,
    raw_input        text        NOT NULL,
    dedupe_key       text        NOT NULL UNIQUE,
    status           text        NOT NULL,

    case_id          text,
    network          text,
    reason_code      text,
    amount_minor     bigint,
    currency         char(3),
    transaction_id   text,
    transaction_date date,
    opened_at        date,
    respond_by       date,
    cardholder_claim text,

    category         text,
    rule_id          text,
    analysis         jsonb,
    letter_subject   text        NOT NULL DEFAULT '',
    letter_body      text        NOT NULL DEFAULT '',
    letter_edited    boolean     NOT NULL DEFAULT false,

    decision         text,
    reviewed_by      text,
    failure_reason   text,

    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    ready_at         timestamptz,
    approved_at      timestamptz
);

CREATE INDEX disputes_created_at_idx ON disputes (created_at DESC);

CREATE TABLE evidence (
    dispute_id uuid        NOT NULL REFERENCES disputes (id) ON DELETE CASCADE,
    source     text        NOT NULL,
    ref        text        NOT NULL,
    found      boolean     NOT NULL,
    data       jsonb,
    fetched_at timestamptz NOT NULL,
    PRIMARY KEY (dispute_id, source)
);

-- Append-only. Never store record contents or personal data here.
CREATE TABLE audit_events (
    id             bigserial PRIMARY KEY,
    dispute_id     uuid        NOT NULL REFERENCES disputes (id) ON DELETE CASCADE,
    step           text        NOT NULL,
    kind           text        NOT NULL,
    name           text        NOT NULL DEFAULT '',
    prompt_version text        NOT NULL DEFAULT '',
    model          text        NOT NULL DEFAULT '',
    input_tokens   integer     NOT NULL DEFAULT 0,
    output_tokens  integer     NOT NULL DEFAULT 0,
    latency_ms     integer     NOT NULL DEFAULT 0,
    ok             boolean     NOT NULL,
    detail         jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_events_dispute_idx ON audit_events (dispute_id, id);

CREATE TABLE jobs (
    id           bigserial PRIMARY KEY,
    kind         text        NOT NULL,
    dispute_id   uuid        NOT NULL REFERENCES disputes (id) ON DELETE CASCADE,
    status       text        NOT NULL DEFAULT 'queued',
    attempts     integer     NOT NULL DEFAULT 0,
    max_attempts integer     NOT NULL DEFAULT 3,
    run_at       timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    last_error   text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX jobs_ready_idx ON jobs (run_at) WHERE status = 'queued';
CREATE INDEX jobs_running_idx ON jobs (locked_until) WHERE status = 'running';

-- One row per chunk and embedding model, so changing the embedding provider
-- only means indexing again. The vector column has no fixed dimension for
-- the same reason; with a few dozen chunks a sequential scan is fine.
CREATE TABLE rule_chunks (
    id           text   NOT NULL,
    embed_model  text   NOT NULL,
    doc_id       text   NOT NULL,
    network      text   NOT NULL,
    heading      text   NOT NULL,
    content      text   NOT NULL,
    content_hash text   NOT NULL,
    embedding    vector NOT NULL,
    PRIMARY KEY (id, embed_model)
);
