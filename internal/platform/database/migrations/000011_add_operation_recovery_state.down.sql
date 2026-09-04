ALTER TABLE operations
    DROP CONSTRAINT operations_recovery_state_check,
    DROP COLUMN recovery_required;
