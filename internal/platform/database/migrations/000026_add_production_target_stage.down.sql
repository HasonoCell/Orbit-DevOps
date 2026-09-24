DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM deployment_targets WHERE stage = 'production') THEN
        RAISE EXCEPTION 'cannot remove production target stage while production targets exist';
    END IF;
END $$;

ALTER TABLE deployment_targets
    DROP CONSTRAINT deployment_targets_stage_check;

ALTER TABLE deployment_targets
    ADD CONSTRAINT deployment_targets_stage_check
    CHECK (stage = 'development');
