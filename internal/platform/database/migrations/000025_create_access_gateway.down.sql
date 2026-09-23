DELETE FROM internal_event_outbox WHERE topic = 'project_gateway.reconcile.v1';
ALTER TABLE internal_event_outbox DROP CONSTRAINT internal_event_outbox_topic_check;
ALTER TABLE internal_event_outbox ADD CONSTRAINT internal_event_outbox_topic_check CHECK (topic IN (
    'webhook_delivery.received.v1',
    'build_operation.changed.v1',
    'release_operation.changed.v1',
    'delivery_run.reconcile.v1'
));
DROP TABLE project_gateway_sync;
DROP TABLE access_routes;
DROP TABLE access_hosts;
DROP TABLE access_secret_bindings;
