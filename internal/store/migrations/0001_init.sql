CREATE TABLE IF NOT EXISTS message_group (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    group_name          TEXT NOT NULL UNIQUE,
    worker_count        INT  NOT NULL DEFAULT 50,
    upstream_timeout_ms INT  NOT NULL DEFAULT 30000,
    rpc_timeout_ms      INT  NOT NULL DEFAULT 60000,
    ref_field           TEXT NOT NULL DEFAULT 'customer_order_id',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS message_group_url (
    id               BIGSERIAL PRIMARY KEY,
    message_group_id UUID NOT NULL REFERENCES message_group(id) ON DELETE CASCADE,
    url              TEXT NOT NULL,
    is_active        BOOLEAN NOT NULL DEFAULT true,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (message_group_id, url)
);

CREATE INDEX IF NOT EXISTS idx_message_group_url_parent
    ON message_group_url (message_group_id) WHERE is_active;

CREATE TABLE IF NOT EXISTS request_logs (
    trace_id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    message_group_id UUID,
    group_name       TEXT NOT NULL,
    status           TEXT NOT NULL,
    business_ref     TEXT,
    caller_trace_id  TEXT,
    client_ip        TEXT,
    request_body     JSONB,
    response_body    JSONB,
    http_status      INT,
    attempt_count    INT NOT NULL DEFAULT 0,
    total_ms         INT,
    error_message    TEXT,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS attempt_logs (
    id                   BIGSERIAL PRIMARY KEY,
    trace_id             UUID NOT NULL REFERENCES request_logs(trace_id) ON DELETE CASCADE,
    seq                  INT  NOT NULL,
    message_group_url_id BIGINT,
    url                  TEXT NOT NULL,
    http_status          INT,
    duration_ms          INT,
    outcome              TEXT NOT NULL,
    response_body        TEXT,
    error_message        TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (trace_id, seq)
);

CREATE INDEX IF NOT EXISTS idx_request_logs_pending
    ON request_logs (created_at) WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS idx_request_logs_ref
    ON request_logs (business_ref) WHERE business_ref IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_request_logs_group
    ON request_logs (message_group_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_attempt_logs_url
    ON attempt_logs (message_group_url_id, created_at DESC);
