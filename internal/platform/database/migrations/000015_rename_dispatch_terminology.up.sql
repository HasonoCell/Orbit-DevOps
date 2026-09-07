-- 统一调度顺序、消息协议和运输领取的术语，同时保留已有数据与升级路径。
ALTER TABLE operations
    RENAME COLUMN dispatch_generation TO current_dispatch_sequence;
ALTER TABLE operations
    RENAME CONSTRAINT operations_dispatch_generation_check TO operations_current_dispatch_sequence_check;

ALTER TABLE operation_dispatches
    RENAME COLUMN generation TO sequence;
ALTER TABLE operation_dispatches
    RENAME COLUMN version TO protocol_version;
ALTER TABLE operation_dispatches
    RENAME COLUMN reason TO dispatch_reason;
ALTER TABLE operation_dispatches
    RENAME COLUMN delivery_count TO reservation_count;

ALTER TABLE operation_dispatches
    RENAME CONSTRAINT operation_dispatches_generation_check TO operation_dispatches_sequence_check;
ALTER TABLE operation_dispatches
    RENAME CONSTRAINT operation_dispatches_delivery_count_check TO operation_dispatches_reservation_count_check;
ALTER TABLE operation_dispatches
    RENAME CONSTRAINT operation_dispatches_operation_id_generation_key TO operation_dispatches_operation_id_sequence_key;
