-- 只有仍保留原始 Delivery 的 Run 才能恢复 S5 初始外键。
DELETE FROM delivery_runs WHERE webhook_delivery_id IS NULL;
ALTER TABLE delivery_runs
    DROP CONSTRAINT delivery_runs_webhook_delivery_id_fkey,
    ADD CONSTRAINT delivery_runs_webhook_delivery_id_fkey
        FOREIGN KEY (webhook_delivery_id) REFERENCES webhook_deliveries (id),
    ALTER COLUMN webhook_delivery_id SET NOT NULL,
    DROP COLUMN tracestate,
    DROP COLUMN traceparent,
    DROP COLUMN trigger_received_at,
    DROP COLUMN trigger_forced,
    DROP COLUMN trigger_git_ref,
    DROP COLUMN trigger_repository_full_name,
    DROP COLUMN trigger_event_type;
