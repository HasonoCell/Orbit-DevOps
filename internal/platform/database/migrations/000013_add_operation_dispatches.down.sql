DROP TABLE operation_dispatches;
DROP INDEX operations_expired_lease_idx;
ALTER TABLE operations DROP COLUMN dispatch_generation;
