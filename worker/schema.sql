-- CFSM Lite：只有两张表，不存历史数据。
-- 执行方式：npx wrangler d1 execute cfsm-lite --remote --file=schema.sql

CREATE TABLE IF NOT EXISTS servers (
  id                      TEXT PRIMARY KEY,
  name                    TEXT    NOT NULL DEFAULT '',
  server_group            TEXT    NOT NULL DEFAULT 'Default',
  sort_order              INTEGER NOT NULL DEFAULT 0,
  is_hidden               TEXT    NOT NULL DEFAULT '0',
  created_at              INTEGER NOT NULL,
  -- Agent 相关配置（Lite 第一版只存储，不向 Agent 下发，见 README）
  reset_day               INTEGER NOT NULL DEFAULT 1,
  collect_interval        INTEGER NOT NULL DEFAULT 0,
  report_interval         INTEGER NOT NULL DEFAULT 60,
  interface               TEXT    NOT NULL DEFAULT '',
  ping_mode               TEXT    NOT NULL DEFAULT 'tcp',
  custom_ct               TEXT    NOT NULL DEFAULT '',
  custom_cu               TEXT    NOT NULL DEFAULT '',
  custom_cm               TEXT    NOT NULL DEFAULT '',
  custom_bd               TEXT    NOT NULL DEFAULT '',
  offline_notify_disabled TEXT    NOT NULL DEFAULT '0',
  -- 价格、到期、备注等 Monito 可能读取的兼容字段，整体放在一个 JSON 里
  extra                   TEXT    NOT NULL DEFAULT '{}',
  -- 在线状态机：UNINIT（从未上报）/ ONLINE / OFFLINE
  status                  TEXT    NOT NULL DEFAULT 'UNINIT',
  status_since            INTEGER NOT NULL DEFAULT 0,
  -- 最新一次上报的指标（每台服务器永远只有这一份，上报时覆盖）
  metrics                 TEXT,
  last_seen               INTEGER NOT NULL DEFAULT 0,
  region                  TEXT    NOT NULL DEFAULT '',
  agent_version           TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
