-- 只有 Operation 的语义状态变化才唤醒 Pipeline；Lease 续期与 Dispatch 运输不会产生事件。
CREATE FUNCTION emit_operation_changed_event() RETURNS trigger AS $$
DECLARE
    event_topic text;
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.status = OLD.status THEN
        RETURN NEW;
    END IF;

    IF TG_TABLE_NAME = 'build_operations' THEN
        event_topic := 'build_operation.changed.v1';
    ELSE
        event_topic := 'release_operation.changed.v1';
    END IF;

    INSERT INTO internal_event_outbox (
        id, topic, aggregate_id, protocol_version, state, available_at,
        next_dispatch_at, traceparent, tracestate, created_at, updated_at
    ) VALUES (
        gen_random_uuid(), event_topic, NEW.id, 1, 'pending', NEW.updated_at,
        NEW.updated_at, NEW.traceparent, NEW.tracestate, NEW.updated_at, NEW.updated_at
    );
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER build_operation_changed_event
    AFTER INSERT OR UPDATE OF status ON build_operations
    FOR EACH ROW EXECUTE FUNCTION emit_operation_changed_event();

CREATE TRIGGER release_operation_changed_event
    AFTER INSERT OR UPDATE OF status ON release_operations
    FOR EACH ROW EXECUTE FUNCTION emit_operation_changed_event();
