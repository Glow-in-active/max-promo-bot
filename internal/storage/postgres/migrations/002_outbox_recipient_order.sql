CREATE INDEX outbox_recipient_pending_idx ON outbox (user_id, seq)
    WHERE state IN ('pending', 'retry', 'processing');
