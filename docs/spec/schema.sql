-- ssh-gate-oss 仕様要件データベース
-- 使い方: sqlite3 docs/spec/ssh-gate-spec.sqlite < docs/spec/schema.sql

-- 仕様要件
CREATE TABLE IF NOT EXISTS requirements (
  id          TEXT PRIMARY KEY,               -- REQ-001
  category    TEXT NOT NULL,                  -- bugfix / multi-session / mcp / ui / persistence / security
  title       TEXT NOT NULL,
  description TEXT NOT NULL DEFAULT '',
  source      TEXT NOT NULL,                  -- user / committee / design
  source_note TEXT NOT NULL DEFAULT '',       -- 根拠（ユーザー発言の引用など）
  priority    TEXT NOT NULL DEFAULT 'medium'  CHECK (priority IN ('critical','high','medium','low')),
  status      TEXT NOT NULL DEFAULT 'proposed'
              CHECK (status IN ('proposed','accepted','in_progress','implemented','verified','rejected','deferred')),
  phase       TEXT NOT NULL DEFAULT '',       -- 実装フェーズ
  acceptance  TEXT NOT NULL DEFAULT '',       -- 受け入れ条件
  created_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%S','now','localtime')),
  updated_at  TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%S','now','localtime'))
);

-- スコープ（やる / やらない / 後回し）
CREATE TABLE IF NOT EXISTS scope (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  item           TEXT NOT NULL,
  decision       TEXT NOT NULL CHECK (decision IN ('in','out','later')),
  rationale      TEXT NOT NULL DEFAULT '',
  requirement_id TEXT REFERENCES requirements(id),
  decided_by     TEXT NOT NULL DEFAULT '',    -- user / committee / claude
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%S','now','localtime'))
);

-- 委員会所見
CREATE TABLE IF NOT EXISTS committee_findings (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  review_round   TEXT NOT NULL,               -- 例: 2026-10-27 v0
  lens           TEXT NOT NULL,
  severity       TEXT NOT NULL,
  kind           TEXT NOT NULL,
  location       TEXT NOT NULL,
  issue          TEXT NOT NULL,
  recommendation TEXT NOT NULL,
  disposition    TEXT NOT NULL DEFAULT 'open' CHECK (disposition IN ('open','adopted','rejected','deferred')),
  requirement_id TEXT REFERENCES requirements(id)
);

-- 変更履歴（要件のステータス変更などを追記）
CREATE TABLE IF NOT EXISTS requirement_log (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  requirement_id TEXT NOT NULL REFERENCES requirements(id),
  note           TEXT NOT NULL,
  created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%S','now','localtime'))
);
