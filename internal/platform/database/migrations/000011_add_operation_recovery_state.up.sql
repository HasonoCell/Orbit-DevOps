ALTER TABLE operations
    ADD COLUMN recovery_required boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT operations_recovery_state_check
        CHECK (NOT recovery_required OR status = 'pending');
