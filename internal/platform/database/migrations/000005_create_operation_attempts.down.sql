DROP TABLE operation_attempts;
ALTER TABLE operations DROP CONSTRAINT operations_terminal_result_check;
ALTER TABLE operations DROP CONSTRAINT operations_lease_state_check;
