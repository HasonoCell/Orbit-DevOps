-- 离线切换批次与意图重建共用事务；中断后用同一批次安全重跑。
CREATE TABLE dispatch_preparations (
    id uuid PRIMARY KEY,
    scheduled integer NOT NULL DEFAULT 0 CHECK (scheduled >= 0),
    completed_at timestamptz NOT NULL
);
