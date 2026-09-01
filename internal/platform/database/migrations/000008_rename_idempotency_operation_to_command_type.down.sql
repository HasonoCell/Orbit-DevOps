ALTER TABLE idempotency_records
    RENAME COLUMN command_type TO operation;
