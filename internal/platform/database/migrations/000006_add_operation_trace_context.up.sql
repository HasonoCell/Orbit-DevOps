ALTER TABLE operations
    ADD COLUMN traceparent text NOT NULL DEFAULT '',
    ADD COLUMN tracestate text NOT NULL DEFAULT '';
