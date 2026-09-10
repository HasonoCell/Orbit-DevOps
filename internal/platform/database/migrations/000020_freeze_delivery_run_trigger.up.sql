ALTER TABLE delivery_runs
    ADD COLUMN trigger_event_type text,
    ADD COLUMN trigger_repository_full_name text,
    ADD COLUMN trigger_git_ref text,
    ADD COLUMN trigger_forced boolean,
    ADD COLUMN trigger_received_at timestamptz,
    ADD COLUMN traceparent text NOT NULL DEFAULT '',
    ADD COLUMN tracestate text NOT NULL DEFAULT '';

UPDATE delivery_runs r SET
    trigger_event_type = w.event_type,
    trigger_repository_full_name = COALESCE(w.repository_full_name, ''),
    trigger_git_ref = COALESCE(w.git_ref, ''),
    trigger_forced = w.forced,
    trigger_received_at = w.received_at,
    traceparent = w.traceparent,
    tracestate = w.tracestate
FROM webhook_deliveries w WHERE w.id = r.webhook_delivery_id;

ALTER TABLE delivery_runs
    ALTER COLUMN trigger_event_type SET NOT NULL,
    ALTER COLUMN trigger_repository_full_name SET NOT NULL,
    ALTER COLUMN trigger_git_ref SET NOT NULL,
    ALTER COLUMN trigger_forced SET NOT NULL,
    ALTER COLUMN trigger_received_at SET NOT NULL,
    ALTER COLUMN webhook_delivery_id DROP NOT NULL,
    DROP CONSTRAINT delivery_runs_webhook_delivery_id_fkey,
    ADD CONSTRAINT delivery_runs_webhook_delivery_id_fkey
        FOREIGN KEY (webhook_delivery_id) REFERENCES webhook_deliveries (id) ON DELETE SET NULL;
