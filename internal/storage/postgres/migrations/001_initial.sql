CREATE SEQUENCE IF NOT EXISTS demo_user_ids START 1000000000000;

CREATE TABLE campaigns (
    id text PRIMARY KEY,
    name text NOT NULL,
    status text NOT NULL CHECK (status IN ('active', 'closed')),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    timezone text NOT NULL,
    final_draw_id text NOT NULL,
    main_prize text NOT NULL,
    rules text NOT NULL,
    CHECK (starts_at < ends_at)
);

CREATE TABLE users (
    id bigint PRIMARY KEY CHECK (id > 0),
    started_at timestamptz NOT NULL
);

CREATE TABLE campaign_users (
    campaign_id text NOT NULL REFERENCES campaigns(id),
    user_id bigint NOT NULL REFERENCES users(id),
    started_at timestamptz NOT NULL,
    PRIMARY KEY (campaign_id, user_id)
);

CREATE TABLE days (
    id text PRIMARY KEY,
    campaign_id text NOT NULL REFERENCES campaigns(id),
    number integer NOT NULL CHECK (number BETWEEN 1 AND 10),
    starts_at timestamptz NOT NULL,
    ends_at timestamptz NOT NULL,
    partner text NOT NULL,
    partner_url text NOT NULL,
    channel_id bigint NOT NULL,
    instruction text NOT NULL,
    draw_id text NOT NULL UNIQUE,
    UNIQUE (id, campaign_id),
    UNIQUE (campaign_id, number),
    CHECK (starts_at < ends_at)
);

CREATE TABLE draws (
    id text PRIMARY KEY,
    campaign_id text NOT NULL REFERENCES campaigns(id),
    day_id text REFERENCES days(id),
    kind text NOT NULL CHECK (kind IN ('daily', 'final')),
    state text NOT NULL CHECK (state IN ('open', 'drawn', 'approved')),
    closes_at timestamptz NOT NULL,
    winner_count integer NOT NULL CHECK (winner_count > 0),
    reserve_count integer NOT NULL CHECK (reserve_count >= 0),
    prize text NOT NULL,
    claim_ttl_ns bigint NOT NULL CHECK (claim_ttl_ns > 0),
    candidates jsonb NOT NULL DEFAULT '[]',
    selection jsonb NOT NULL DEFAULT '{"winners":[],"reserve":[]}',
    drawn_at timestamptz,
    approved_at timestamptz,
    FOREIGN KEY (day_id, campaign_id) REFERENCES days(id, campaign_id),
    CHECK ((kind = 'daily') = (day_id IS NOT NULL)),
    CHECK (state = 'open' OR drawn_at IS NOT NULL),
    CHECK (state <> 'approved' OR approved_at IS NOT NULL)
);
CREATE INDEX draws_due_idx ON draws (closes_at) WHERE state = 'open';

CREATE TABLE promo_pools (
    day_id text PRIMARY KEY REFERENCES days(id)
);

CREATE TABLE promos (
    id text PRIMARY KEY,
    day_id text NOT NULL REFERENCES days(id),
    value text NOT NULL UNIQUE,
    user_id bigint REFERENCES users(id),
    assigned_at timestamptz,
    UNIQUE (day_id, user_id),
    CHECK ((user_id IS NULL) = (assigned_at IS NULL))
);
CREATE INDEX promos_available_idx ON promos (day_id, id) WHERE user_id IS NULL;

CREATE TABLE activities (
    campaign_id text NOT NULL REFERENCES campaigns(id),
    user_id bigint NOT NULL REFERENCES users(id),
    day_id text NOT NULL REFERENCES days(id),
    source text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (campaign_id, user_id, day_id),
    FOREIGN KEY (day_id, campaign_id) REFERENCES days(id, campaign_id)
);

CREATE TABLE entries (
    draw_id text NOT NULL REFERENCES draws(id),
    user_id bigint NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL,
    PRIMARY KEY (draw_id, user_id)
);

CREATE TABLE promo_requests (
    id bigserial PRIMARY KEY,
    campaign_id text NOT NULL REFERENCES campaigns(id),
    user_id bigint NOT NULL REFERENCES users(id),
    created_at timestamptz NOT NULL
);

CREATE TABLE slots (
    id text PRIMARY KEY,
    draw_id text NOT NULL REFERENCES draws(id),
    position integer NOT NULL CHECK (position >= 0),
    UNIQUE (id, draw_id),
    UNIQUE (draw_id, position)
);

CREATE TABLE offers (
    id text PRIMARY KEY,
    slot_id text NOT NULL REFERENCES slots(id),
    draw_id text NOT NULL REFERENCES draws(id),
    user_id bigint NOT NULL REFERENCES users(id),
    status text NOT NULL CHECK (status IN ('pending', 'notified', 'claimed', 'expired')),
    sent_at timestamptz,
    deadline timestamptz,
    claimed_at timestamptz,
    name text NOT NULL DEFAULT '',
    consent_at timestamptz,
    FOREIGN KEY (slot_id, draw_id) REFERENCES slots(id, draw_id),
    CHECK (status <> 'notified' OR (sent_at IS NOT NULL AND deadline IS NOT NULL)),
    CHECK (status <> 'claimed' OR (claimed_at IS NOT NULL AND consent_at IS NOT NULL))
);
CREATE UNIQUE INDEX offers_active_slot_idx ON offers (slot_id) WHERE status <> 'expired';
CREATE UNIQUE INDEX offers_draw_user_idx ON offers (draw_id, user_id);
CREATE INDEX offers_due_idx ON offers (deadline) WHERE status = 'notified';

CREATE TABLE pending_claims (
    user_id bigint PRIMARY KEY REFERENCES users(id),
    offer_id text NOT NULL REFERENCES offers(id),
    name text NOT NULL DEFAULT ''
);

CREATE TABLE outbox (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    dedup_key text NOT NULL UNIQUE,
    campaign_id text NOT NULL REFERENCES campaigns(id),
    user_id bigint NOT NULL REFERENCES users(id),
    text text NOT NULL,
    buttons jsonb NOT NULL DEFAULT '[]',
    offer_id text REFERENCES offers(id),
    banner boolean NOT NULL DEFAULT false,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'processing', 'retry', 'delivered', 'failed')),
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    lease_until timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL
);
CREATE INDEX outbox_ready_idx ON outbox (next_attempt_at, seq) WHERE state IN ('pending', 'retry', 'processing');
CREATE INDEX outbox_campaign_idx ON outbox (campaign_id, seq);
CREATE INDEX outbox_offer_idx ON outbox (offer_id, seq DESC) WHERE offer_id IS NOT NULL;

CREATE TABLE inbox (
    seq bigserial UNIQUE,
    id text PRIMARY KEY,
    dedup_key text NOT NULL UNIQUE,
    command jsonb NOT NULL,
    state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'processing', 'retry', 'done', 'failed')),
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error text NOT NULL DEFAULT ''
);
CREATE INDEX inbox_ready_idx ON inbox (next_attempt_at, seq) WHERE state IN ('pending', 'retry', 'processing');

CREATE TABLE audit_events (
    id bigserial PRIMARY KEY,
    campaign_id text NOT NULL REFERENCES campaigns(id),
    action text NOT NULL,
    detail text NOT NULL,
    created_at timestamptz NOT NULL
);
