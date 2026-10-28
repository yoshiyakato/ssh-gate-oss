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

-- ---------------------------------------------------------------------------
-- 参考: アプリ本体 DB（ssh-gete.sqlite）の v3 追加分。実体は storage.go の migrate()。
-- このファイルを流しても作られないよう、すべてコメントにしている。
--
-- PRAGMA user_version = 3   -- v2 以下は開く前に VACUUM INTO <path>.bak-v<旧版> で退避
--
-- CREATE TABLE agent_tokens (
--   name         TEXT PRIMARY KEY,          -- エージェント名（agent_policies.agent_name と同じ名前空間）
--   token_hash   TEXT NOT NULL UNIQUE,      -- SHA-256 hex。平文は発行時に1度だけ返す（接頭辞 sga_）
--   bypass       INTEGER NOT NULL DEFAULT 0, -- このトークンで承認省略を名乗ってよいか
--   preview      TEXT NOT NULL DEFAULT '',  -- 秘密でないヒント（sga_…末尾4文字）
--   created_at   INTEGER NOT NULL           -- ms
-- );
--
-- ALTER TABLE agent_policies ADD COLUMN max_risk       TEXT    NOT NULL DEFAULT 'Medium'; -- Low|Medium（High は常に承認）
-- ALTER TABLE agent_policies ADD COLUMN scope_conn_ids TEXT    NOT NULL DEFAULT '[]';     -- JSON 配列。空 = 全接続先
-- ALTER TABLE agent_policies ADD COLUMN granted_at     INTEGER NOT NULL DEFAULT 0;        -- ms
-- ALTER TABLE agent_policies ADD COLUMN granted_by     TEXT    NOT NULL DEFAULT '';
-- ALTER TABLE agent_policies ADD COLUMN expires_at     INTEGER NOT NULL DEFAULT 0;        -- ms。既定は付与から8時間
--   -- v2 からの移行: approval_bypass=1 かつ expires_at=0 の行は granted_at=updated_at、
--   -- expires_at=updated_at+8時間 にする（8時間より古い付与は移行後すでに失効扱い）
--   -- 有効 = approval_bypass=1 AND expires_at > 現在。取り消しは approval_bypass=0, expires_at=0
--
-- ALTER TABLE command_requests ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''; -- RFC3339。pending の期限
-- ALTER TABLE command_requests ADD COLUMN target_fp  TEXT NOT NULL DEFAULT ''; -- 要求時の user@host:port
--
-- ALTER TABLE connections ADD COLUMN data_bits INTEGER NOT NULL DEFAULT 8;      -- 5..8
-- ALTER TABLE connections ADD COLUMN parity    TEXT    NOT NULL DEFAULT 'none'; -- none|odd|even|mark|space
-- ALTER TABLE connections ADD COLUMN stop_bits TEXT    NOT NULL DEFAULT '1';    -- 1|1.5|2
--
-- CREATE INDEX idx_command_requests_client ON command_requests(requested_by, client_request_id);
--   -- (requested_by, client_request_id) は created_at から24時間以内なら同一要求として既存 ID を返す
