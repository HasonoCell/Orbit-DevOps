ALTER TABLE operation_dispatches
    RENAME CONSTRAINT operation_dispatches_operation_id_sequence_key TO operation_dispatches_operation_id_generation_key;
ALTER TABLE operation_dispatches
    RENAME CONSTRAINT operation_dispatches_reservation_count_check TO operation_dispatches_delivery_count_check;
ALTER TABLE operation_dispatches
    RENAME CONSTRAINT operation_dispatches_sequence_check TO operation_dispatches_generation_check;

ALTER TABLE operation_dispatches
    RENAME COLUMN reservation_count TO delivery_count;
ALTER TABLE operation_dispatches
    RENAME COLUMN dispatch_reason TO reason;
ALTER TABLE operation_dispatches
    RENAME COLUMN protocol_version TO version;
ALTER TABLE operation_dispatches
    RENAME COLUMN sequence TO generation;

ALTER TABLE operations
    RENAME CONSTRAINT operations_current_dispatch_sequence_check TO operations_dispatch_generation_check;
ALTER TABLE operations
    RENAME COLUMN current_dispatch_sequence TO dispatch_generation;
