-- database: :memory:
-- schema_version tracks applied migrations so startup only runs new ones
CREATE TABLE IF NOT EXISTS
    schema_version (
        version INTEGER PRIMARY KEY,
        applied_at DATETIME NOT NULL DEFAULT (datetime('now'))
    );

CREATE TABLE IF NOT EXISTS
    apps (
        id TEXT PRIMARY KEY,
        name TEXT NOT NULL UNIQUE,
        repo_url TEXT NOT NULL,
        branch TEXT NOT NULL DEFAULT 'main',
        webhook_secret TEXT NOT NULL,
        env_file TEXT NOT NULL DEFAULT '',
        health_check_url TEXT NOT NULL DEFAULT '',
        current_deploy_id TEXT,
        created_at DATETIME NOT NULL DEFAULT (datetime('now'))
    );

CREATE TABLE IF NOT EXISTS
    deploys (
        id TEXT PRIMARY KEY,
        app_id TEXT NOT NULL REFERENCES apps (id),
        git_sha TEXT NOT NULL,
        image_tag TEXT NOT NULL,
        status TEXT NOT NULL DEFAULT 'pending',
        trigger TEXT NOT NULL DEFAULT 'manual',
        started_at DATETIME NOT NULL DEFAULT (datetime('now')),
        finished_at DATETIME,
        error_msg TEXT
    );

CREATE INDEX IF NOT EXISTS idx_deploys_app_id ON deploys (app_id);

CREATE INDEX IF NOT EXISTS idx_deploys_status ON deploys (status);

INSERT OR IGNORE INTO
    schema_version (version)
VALUES
    (1);
