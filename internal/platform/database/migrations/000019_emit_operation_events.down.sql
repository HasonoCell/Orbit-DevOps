DROP TRIGGER IF EXISTS release_operation_changed_event ON release_operations;
DROP TRIGGER IF EXISTS build_operation_changed_event ON build_operations;
DROP FUNCTION IF EXISTS emit_operation_changed_event();
