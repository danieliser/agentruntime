-- Additive terminal-classification proof. Existing receipts retain an empty
-- stored value and are read as their state name; new forced terminations can
-- be distinguished from caller cancellation without rebuilding core tables.
ALTER TABLE terminal_receipts
ADD COLUMN terminal_reason TEXT NOT NULL DEFAULT '' CHECK (terminal_reason IN (
    '', 'completed', 'failed', 'cancelled', 'terminated',
    'timed_out', 'crashed', 'indeterminate'
));
