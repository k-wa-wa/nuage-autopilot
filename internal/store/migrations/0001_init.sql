-- DESIGN.md 6.3 節のスキーマ。DB は真実ではなく GitHub の写像にすぎないため、
-- 外部キーは参照整合性の補助として使うが、GitHub 側の削除操作をここから連鎖させることはしない。

CREATE TABLE items (
  id           INTEGER PRIMARY KEY,
  repo         TEXT    NOT NULL,
  number       INTEGER NOT NULL,
  kind         TEXT    NOT NULL,
  phase        TEXT    NOT NULL,
  parent_id    INTEGER REFERENCES items(id),
  session_id   TEXT,
  head_sha     TEXT,
  cost_usd     REAL    NOT NULL DEFAULT 0,
  runs         INTEGER NOT NULL DEFAULT 0,
  last_seen_at TEXT,
  updated_at   TEXT    NOT NULL,
  UNIQUE(repo, number)
);

CREATE INDEX idx_items_phase ON items(phase);
CREATE INDEX idx_items_parent_id ON items(parent_id);

CREATE TABLE events (
  id           INTEGER PRIMARY KEY,
  dedup_key    TEXT    NOT NULL UNIQUE,
  item_id      INTEGER NOT NULL REFERENCES items(id),
  type         TEXT    NOT NULL,
  actor        TEXT    NOT NULL,
  body         TEXT,
  created_at   TEXT    NOT NULL,
  processed_at TEXT
);

CREATE INDEX idx_events_unprocessed ON events(created_at) WHERE processed_at IS NULL;
CREATE INDEX idx_events_item_id ON events(item_id);

CREATE TABLE leases (
  item_id    INTEGER PRIMARY KEY REFERENCES items(id),
  holder     TEXT NOT NULL,
  expires_at TEXT NOT NULL
);

CREATE TABLE cursors (
  source        TEXT PRIMARY KEY,
  etag          TEXT,
  last_modified TEXT,
  since         TEXT,
  polled_at     TEXT
);
