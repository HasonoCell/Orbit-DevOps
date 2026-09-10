-- 回退时恢复历史物理名称，供旧版本二进制继续访问。
DO $migration$
DECLARE
    object_record record;
BEGIN
    FOR object_record IN
        SELECT conname AS old_name
        FROM pg_constraint
        WHERE conrelid = 'release_operations'::regclass
          AND conname LIKE 'release_operations_%'
    LOOP
        EXECUTE format(
            'ALTER TABLE release_operations RENAME CONSTRAINT %I TO %I',
            object_record.old_name,
            regexp_replace(object_record.old_name, '^release_operations_', 'operations_')
        );
    END LOOP;

    FOR object_record IN
        SELECT indexname AS old_name
        FROM pg_indexes
        WHERE schemaname = current_schema()
          AND tablename = 'release_operations'
          AND indexname LIKE 'release_operations_%'
    LOOP
        EXECUTE format(
            'ALTER INDEX %I RENAME TO %I',
            object_record.old_name,
            regexp_replace(object_record.old_name, '^release_operations_', 'operations_')
        );
    END LOOP;
END
$migration$;

ALTER TABLE release_operations RENAME TO operations;
