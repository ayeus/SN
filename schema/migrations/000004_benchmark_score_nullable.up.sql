-- AyeusANN Schema v4: Nullable GPU compute benchmark score
-- Allows benchmarks to record NULL when GPU compute benchmarks are unmeasured,
-- rather than fabricating synthetic scores.

ALTER TABLE benchmarks ALTER COLUMN score_compute DROP NOT NULL;
