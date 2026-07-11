CREATE TABLE IF NOT EXISTS articles (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    guid TEXT,
    title TEXT NOT NULL,
    original_url TEXT NOT NULL,
    canonical_url TEXT,
    normalized_url TEXT NOT NULL,
    normalized_title TEXT NOT NULL,
    source TEXT NOT NULL,
    published_at TEXT,
    updated_at TEXT,
    first_seen_at TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    ranking_score INTEGER NOT NULL DEFAULT 0,
    score_breakdown TEXT NOT NULL DEFAULT '{}'
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_articles_normalized_url
    ON articles(normalized_url);
CREATE INDEX IF NOT EXISTS idx_articles_source_guid
    ON articles(source, guid);
CREATE INDEX IF NOT EXISTS idx_articles_normalized_title
    ON articles(normalized_title);

CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY,
    run_type TEXT NOT NULL,
    started_at TEXT NOT NULL,
    finished_at TEXT,
    result TEXT NOT NULL DEFAULT 'running',
    fetched_count INTEGER NOT NULL DEFAULT 0,
    selected_count INTEGER NOT NULL DEFAULT 0,
    posted_count INTEGER NOT NULL DEFAULT 0,
    channel_id TEXT,
    local_date TEXT,
    last_error TEXT
);
CREATE INDEX IF NOT EXISTS idx_runs_started_at ON runs(started_at DESC);

CREATE TABLE IF NOT EXISTS feed_fetches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    feed_name TEXT NOT NULL,
    feed_url TEXT NOT NULL,
    checked_at TEXT NOT NULL,
    status TEXT NOT NULL,
    item_count INTEGER NOT NULL DEFAULT 0,
    last_error TEXT
);
CREATE INDEX IF NOT EXISTS idx_feed_fetches_name_time
    ON feed_fetches(feed_name, checked_at DESC);

CREATE TABLE IF NOT EXISTS delivery_batches (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL,
    local_date TEXT NOT NULL,
    batch_index INTEGER NOT NULL,
    nonce TEXT NOT NULL UNIQUE,
    state TEXT NOT NULL,
    message_id TEXT,
    last_error TEXT,
    created_at TEXT NOT NULL,
    sent_at TEXT,
    UNIQUE(run_id, batch_index)
);

CREATE TABLE IF NOT EXISTS deliveries (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    article_id INTEGER NOT NULL REFERENCES articles(id),
    article_guid TEXT,
    normalized_url TEXT NOT NULL,
    normalized_title TEXT NOT NULL,
    source TEXT NOT NULL,
    batch_id INTEGER REFERENCES delivery_batches(id),
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    channel_id TEXT NOT NULL,
    local_date TEXT NOT NULL,
    run_type TEXT NOT NULL,
    state TEXT NOT NULL,
    sent_at TEXT,
    UNIQUE(article_id, channel_id, local_date)
);
CREATE INDEX IF NOT EXISTS idx_deliveries_channel_state
    ON deliveries(channel_id, state, sent_at);

CREATE TABLE IF NOT EXISTS daily_briefs (
    channel_id TEXT NOT NULL,
    local_date TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    state TEXT NOT NULL,
    created_at TEXT NOT NULL,
    completed_at TEXT,
    PRIMARY KEY(channel_id, local_date)
);
