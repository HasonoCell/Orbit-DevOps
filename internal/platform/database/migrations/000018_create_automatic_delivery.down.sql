DROP TABLE IF EXISTS internal_event_outbox;
DROP TABLE IF EXISTS delivery_runs;
DROP TABLE IF EXISTS webhook_deliveries;
ALTER TABLE delivery_pipelines DROP CONSTRAINT IF EXISTS delivery_pipelines_current_revision_fk;
DROP TABLE IF EXISTS delivery_pipeline_revisions;
DROP TABLE IF EXISTS delivery_pipelines;
