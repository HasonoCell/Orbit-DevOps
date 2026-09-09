DROP INDEX IF EXISTS releases_image_artifact_idx;
ALTER TABLE releases DROP COLUMN IF EXISTS image_artifact_id;
DROP TABLE IF EXISTS image_artifacts;
DROP TABLE IF EXISTS build_dispatches;
DROP TABLE IF EXISTS build_attempts;
DROP TABLE IF EXISTS build_operations;
DROP TABLE IF EXISTS builds;
