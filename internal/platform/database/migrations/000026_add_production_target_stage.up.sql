ALTER TABLE deployment_targets
    DROP CONSTRAINT deployment_targets_stage_check;

ALTER TABLE deployment_targets
    ADD CONSTRAINT deployment_targets_stage_check
    CHECK (stage IN ('development', 'production'));
