ALTER TABLE idempotency_records
    RENAME COLUMN operation TO command_type;
