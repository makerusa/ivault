CREATE TABLE IF NOT EXISTS config (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS sessions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    started_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    ended_at     DATETIME,
    status       TEXT DEFAULT 'active',
    files_found  INTEGER DEFAULT 0,
    files_copied INTEGER DEFAULT 0,
    bytes_copied INTEGER DEFAULT 0
);

CREATE TABLE IF NOT EXISTS files (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      INTEGER REFERENCES sessions(id),
    filename        TEXT NOT NULL,
    size_bytes      INTEGER NOT NULL DEFAULT 0,
    checksum_sha256 TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'discovered',
    discovered_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    copied_at       DATETIME,
    queued_at       DATETIME,
    uploaded_at     DATETIME,
    deleted_at      DATETIME,
    upload_attempts INTEGER DEFAULT 0,
    destination_id  INTEGER,
    remote_path     TEXT,
    error_message   TEXT
);

CREATE TABLE IF NOT EXISTS destinations (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT NOT NULL,
    type         TEXT NOT NULL,
    priority     INTEGER NOT NULL DEFAULT 0,
    config_json  TEXT NOT NULL DEFAULT '{}',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_at   DATETIME DEFAULT CURRENT_TIMESTAMP,
    last_used_at DATETIME,
    last_ok_at   DATETIME,
    last_error   TEXT
);

CREATE TABLE IF NOT EXISTS logs (
    id        INTEGER PRIMARY KEY AUTOINCREMENT,
    ts        DATETIME DEFAULT CURRENT_TIMESTAMP,
    level     TEXT NOT NULL,
    component TEXT NOT NULL,
    message   TEXT NOT NULL,
    data_json TEXT
);

CREATE TABLE IF NOT EXISTS upload_queue (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    file_id        INTEGER REFERENCES files(id),
    destination_id INTEGER REFERENCES destinations(id),
    enqueued_at    DATETIME DEFAULT CURRENT_TIMESTAMP,
    attempts       INTEGER DEFAULT 0,
    last_attempt   DATETIME,
    status         TEXT DEFAULT 'pending'
);

CREATE INDEX IF NOT EXISTS idx_files_state ON files(state);
CREATE INDEX IF NOT EXISTS idx_files_checksum ON files(checksum_sha256);
CREATE INDEX IF NOT EXISTS idx_logs_ts ON logs(ts);
CREATE INDEX IF NOT EXISTS idx_upload_queue_status ON upload_queue(status);
