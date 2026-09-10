-- Release 与 Build 已成为平级领域；移除发布侧遗留的通用表名，并同步表内对象前缀。
ALTER TABLE operations RENAME TO release_operations;

DO $migration$
DECLARE
    object_record record;
BEGIN
    FOR object_record IN
        SELECT conname AS old_name
        FROM pg_constraint
        WHERE conrelid = 'release_operations'::regclass
          AND conname LIKE 'operations_%'
    LOOP
        EXECUTE format(
            'ALTER TABLE release_operations RENAME CONSTRAINT %I TO %I',
            object_record.old_name,
            'release_' || object_record.old_name
        );
    END LOOP;

    FOR object_record IN
        SELECT indexname AS old_name
        FROM pg_indexes
        WHERE schemaname = current_schema()
          AND tablename = 'release_operations'
          AND indexname LIKE 'operations_%'
    LOOP
        EXECUTE format(
            'ALTER INDEX %I RENAME TO %I',
            object_record.old_name,
            'release_' || object_record.old_name
        );
    END LOOP;
END
$migration$;
