// CFSM Lite Worker
// 只做核心：Agent 上报、Monito 使用的 REST 与 /admin/api、离线/恢复 Bark 通知。
// 不做：历史数据、WebSocket 推送、主题、Turnstile。管理页面在 /admin（见文件末尾）。

const COMPAT_VERSION = '2.8.0'; // 兼容性版本号，可用环境变量 COMPAT_VERSION 覆盖
const MAX_BODY = 512 * 1024;
const DEFAULT_THRESHOLD_SEC = 180;
const ID_RE = /^[A-Za-z0-9._:-]{1,64}$/;
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
const HOST_RE = /^[A-Za-z0-9._:\-[\]]{0,255}$/;
const HOURS_OK = [0.167, 0.5, 1, 6, 12, 24, 48, 96, 168];
const PING_KEYS = ['ct', 'cu', 'cm', 'bd', 'node_1', 'node_2', 'node_3', 'node_4'];
const NUM_KEYS = [
  'cpu', 'ram_total', 'ram_used', 'swap_total', 'swap_used', 'disk_total', 'disk_used',
  'net_rx', 'net_tx', 'net_rx_monthly', 'net_tx_monthly', 'net_in_speed', 'net_out_speed',
  'cpu_cores', 'processes', 'tcp_conn', 'udp_conn',
];
const EXTRA_DEFAULTS = {
  tags: '', note: '', price: '', billing_cycle: 'month', auto_renewal: '0', currency: '¥',
  expire_date: '', traffic_limit: '', traffic_calc_type: 'total',
  auto_update: '0', rx_correction: null, tx_correction: null,
};
const ADMIN_ONLY_FIELDS = ['note', 'auto_update', 'rx_correction', 'tx_correction'];
const DEFAULT_SETTINGS = {
  site_title: 'CFSM Lite',
  tg_notify: '3', // '0' = 关闭通知；2~30 = 离线阈值（分钟）；默认 3 分钟 = 180 秒
  tg_bot_token: '', // 这里存放 Bark 地址（https://api.day.app/<设备Key>）或设备 Key
  notification_timezone: 'Asia/Shanghai',
  ping_mode: 'tcp', // 全局 Ping 探测方式：tcp / icmp（对所有节点生效）
  custom_ct: 'gd-ct-dualstack.ip.zstaticcdn.com',
  custom_cu: 'gd-cu-dualstack.ip.zstaticcdn.com',
  custom_cm: 'gd-cm-dualstack.ip.zstaticcdn.com',
  custom_bd: 'ip.zstaticcdn.com',
  custom_ct_name: '电信',
  custom_cu_name: '联通',
  custom_cm_name: '移动',
  custom_bd_name: 'BGP',
  expire_notify_days: '7,3,1,0', // 到期前几天提醒（逗号分隔，0 表示到期当天）；留空表示不提醒
  expire_notify_hour: '9', // 到期提醒每天几点之后发送（按通知时区）
  traffic_notify_percents: '90,95', // 当月流量用到百分之几时提醒；留空表示不提醒
};
const SAVEABLE_SETTINGS = [
  'site_title', 'tg_notify', 'tg_bot_token', 'notification_timezone', 'ping_mode',
  'custom_ct', 'custom_cu', 'custom_cm', 'custom_bd',
  'custom_ct_name', 'custom_cu_name', 'custom_cm_name', 'custom_bd_name',
  'expire_notify_days', 'expire_notify_hour', 'traffic_notify_percents',
];

// ---------------------------------------------------------------- 通用工具
const enc = new TextEncoder();
const json = (data, status = 200, headers = {}) =>
  new Response(JSON.stringify(data), {
    status,
    headers: { 'content-type': 'application/json; charset=utf-8', ...headers },
  });
const fail = (error, code) => json({ error, code }, code);
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const safeJson = (s, fallback = {}) => {
  try { const v = JSON.parse(s); return v && typeof v === 'object' ? v : fallback; } catch { return fallback; }
};

async function sha256(s) {
  return new Uint8Array(await crypto.subtle.digest('SHA-256', enc.encode(String(s))));
}
// 先各自取 SHA-256 再比较，长度恒定，避免通过耗时推测内容
async function safeEqual(a, b) {
  const [x, y] = await Promise.all([sha256(a), sha256(b)]);
  let diff = 0;
  for (let i = 0; i < 32; i++) diff |= x[i] ^ y[i];
  return diff === 0;
}

// ---------------------------------------------------------------- JWT（HS256，7 天）
const b64u = (buf) =>
  btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const b64uStr = (s) => b64u(enc.encode(s));
const unb64u = (s) => Uint8Array.from(atob(s.replace(/-/g, '+').replace(/_/g, '/')), (c) => c.charCodeAt(0));
const hmacKey = (env) =>
  crypto.subtle.importKey('raw', enc.encode(env.JWT_SECRET || 'cfsm-lite-jwt:' + env.API_SECRET),
    { name: 'HMAC', hash: 'SHA-256' }, false, ['sign', 'verify']);

async function signJwt(env) {
  const now = Math.floor(Date.now() / 1000);
  const head = b64uStr(JSON.stringify({ alg: 'HS256', typ: 'JWT' }));
  const body = b64uStr(JSON.stringify({ sub: 'admin', iat: now, exp: now + 604800 }));
  const sig = await crypto.subtle.sign('HMAC', await hmacKey(env), enc.encode(head + '.' + body));
  return head + '.' + body + '.' + b64u(sig);
}
async function verifyJwt(env, token) {
  try {
    const [h, b, s] = token.split('.');
    if (!h || !b || !s) return false;
    const ok = await crypto.subtle.verify('HMAC', await hmacKey(env), unb64u(s), enc.encode(h + '.' + b));
    if (!ok) return false;
    const payload = JSON.parse(new TextDecoder().decode(unb64u(b)));
    return payload.exp > Date.now() / 1000;
  } catch { return false; }
}
async function isAdmin(request, env) {
  const m = /^Bearer\s+(.+)$/i.exec(request.headers.get('authorization') || '');
  return !!m && (await verifyJwt(env, m[1]));
}

// ---------------------------------------------------------------- 设置与 Bark
async function loadSettings(env) {
  const { results } = await env.DB.prepare("SELECT key, value FROM settings WHERE key NOT LIKE 'ntf:%'").all(); // ntf: 开头的是提醒去重标记，不属于设置
  const s = { ...DEFAULT_SETTINGS };
  for (const r of results || []) s[r.key] = r.value;
  return s;
}
// tg_notify 为 2~30 时，离线阈值 = 该分钟数；其他情况用 180 秒。
// 显示用的在线判断和 Bark 通知共用这一个阈值。
function thresholdSec(s) {
  const n = parseInt(s.tg_notify, 10);
  return n >= 2 && n <= 30 ? n * 60 : DEFAULT_THRESHOLD_SEC;
}
function barkTarget(v) {
  const t = String(v || '').trim();
  if (!t) return '';
  return /^https?:\/\//i.test(t) ? t.replace(/\/+$/, '') : 'https://api.day.app/' + encodeURIComponent(t);
}
const notifyEnabled = (s) => s.tg_notify !== '0' && !!barkTarget(s.tg_bot_token);

async function sendBark(env, s, title, body, tokenOverride) {
  const target = barkTarget(tokenOverride ?? s.tg_bot_token);
  if (!target) return false;
  const retryMs = env.BARK_RETRY_MS !== undefined ? Number(env.BARK_RETRY_MS) : 2000;
  for (let i = 0; i < 3; i++) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 5000);
    try {
      const r = await fetch(target, {
        method: 'POST',
        headers: { 'content-type': 'application/json; charset=utf-8' },
        body: JSON.stringify({ title, body, group: 'CFSM Lite' }),
        signal: ctrl.signal,
      });
      if (r.ok) return true;
    } catch { /* 忽略，下面统一处理重试；不输出地址以免泄露密钥 */ } finally { clearTimeout(timer); }
    if (i < 2) await sleep(retryMs);
  }
  console.log('bark: send failed after 3 attempts');
  return false;
}
function fmtTime(ms, tz) {
  try {
    return new Intl.DateTimeFormat('zh-CN', { timeZone: tz, dateStyle: 'medium', timeStyle: 'medium', hour12: false }).format(new Date(ms));
  } catch { return new Date(ms).toISOString(); }
}
function fmtDuration(ms) {
  const min = Math.max(1, Math.round(ms / 60000));
  return min < 60 ? `${min} 分钟` : `${Math.floor(min / 60)} 小时 ${min % 60} 分钟`;
}


// ---------------------------------------------------------------- 续费、到期与流量配额
const CYCLE_LABEL = {
  month: '月付', quarter: '季付', half_year: '半年付', year: '年付',
  two_years: '两年付', three_years: '三年付', four_years: '四年付', five_years: '五年付',
};
const CURRENCIES = ['¥', '$', '€', '£', '₽', '₣', '₹', '₫', '฿'];
const CALC_LABEL = { total: '上下行合计', up: '仅上行', down: '仅下行', max: '上下行取较大值' };

// 解析流量上限文本：500GB、1TB、1.5T、200G、500（不写单位按 GB）。按 1024 换算成字节；空或无法识别返回 0。
export function parseTrafficLimit(text) {
  const m = /^\s*(\d+(?:\.\d+)?)\s*(?:([KMGT])(?:i?B)?)?\s*$/i.exec(String(text ?? ''));
  if (!m) return 0;
  const mult = { K: 1024, M: 1024 ** 2, G: 1024 ** 3, T: 1024 ** 4 }[(m[2] || 'G').toUpperCase()];
  return Math.round(Number(m[1]) * mult);
}
// 按「流量计算方式」从当月上下行累计里取出用于和上限比较的数值
export function trafficUsed(m, calc) {
  const rx = Number(m.net_rx_monthly) || 0;
  const tx = Number(m.net_tx_monthly) || 0;
  if (calc === 'up') return tx;
  if (calc === 'down') return rx;
  if (calc === 'max') return Math.max(rx, tx);
  return rx + tx;
}
function fmtBytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let v = Number(n) || 0;
  let i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return (i === 0 ? v.toFixed(0) : v.toFixed(v >= 100 ? 0 : 2)) + ' ' + u[i];
}
export function validDate(s) {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(String(s))) return false;
  const d = new Date(s + 'T00:00:00Z');
  return !Number.isNaN(d.getTime()) && d.toISOString().slice(0, 10) === s;
}
export function ymdInTz(ms, tz) {
  try {
    const p = new Intl.DateTimeFormat('en-CA', { timeZone: tz, year: 'numeric', month: '2-digit', day: '2-digit' }).format(new Date(ms));
    if (/^\d{4}-\d{2}-\d{2}$/.test(p)) return p;
  } catch { /* 时区无效时退回 UTC */ }
  return new Date(ms).toISOString().slice(0, 10);
}
function hourInTz(ms, tz) {
  try {
    const h = Number(new Intl.DateTimeFormat('en-GB', { timeZone: tz, hour: '2-digit', hourCycle: 'h23' }).format(new Date(ms)));
    if (Number.isFinite(h)) return h % 24;
  } catch { /* 同上 */ }
  return new Date(ms).getUTCHours();
}
export function daysLeft(expire, today) {
  return Math.round((Date.parse(expire + 'T00:00:00Z') - Date.parse(today + 'T00:00:00Z')) / 86400000);
}
// 当前流量统计周期的起始日（与 Agent 的算法一致）：重置日大于当月天数时取当月最后一天；0 表示不重置
export function periodStart(ms, resetDay, tz) {
  if (!(resetDay > 0)) return 'none';
  const [y, m, d] = ymdInTz(ms, tz).split('-').map(Number);
  const dim = (yy, mm) => new Date(Date.UTC(yy, mm, 0)).getUTCDate();
  let sy = y, sm = m, day = Math.min(resetDay, dim(y, m));
  if (d < day) {
    sm = m - 1;
    if (sm === 0) { sm = 12; sy = y - 1; }
    day = Math.min(resetDay, dim(sy, sm));
  }
  return `${sy}-${String(sm).padStart(2, '0')}-${String(day).padStart(2, '0')}`;
}
// 解析 "7,3,1,0" 这样的整数列表；不合法返回 null，空串返回空数组
export function parseIntList(text, min, max, limit) {
  const t = String(text ?? '').trim();
  if (t === '') return [];
  const parts = t.split(/[,，\s]+/).filter(Boolean);
  const nums = parts.map(Number);
  if (parts.length > limit || nums.some((n) => !Number.isInteger(n) || n < min || n > max)) return null;
  return [...new Set(nums)];
}
// 到期提醒档位：days 为距到期的天数。取「不小于 days 的最小档位」；已过期单独记为 -1；超出最大档位返回 null
export function expireStage(days, stagesDesc) {
  if (days < 0) return -1;
  const ok = stagesDesc.filter((x) => x >= days);
  return ok.length ? Math.min(...ok) : null;
}
// 流量提醒档位：取不大于当前百分比的最大档位
export function trafficStage(pct, stagesAsc) {
  let best = null;
  for (const x of stagesAsc) if (pct >= x) best = x;
  return best;
}
function priceText(extra) {
  const p = String(extra.price ?? '').trim();
  if (p === '') return '';
  if (Number(p) <= 0) return '免费';
  return `${extra.currency || ''}${p} / ${CYCLE_LABEL[extra.billing_cycle] || extra.billing_cycle}`;
}
// 校验并规范化节点的价格、周期、到期日、流量上限等附加字段
function normExtra(k, raw) {
  const v = String(raw ?? '').trim();
  switch (k) {
    case 'price':
      if (v === '') return { value: '' };
      if (!/^-?\d{1,9}(\.\d{1,4})?$/.test(v)) return { error: 'invalidPrice' };
      return { value: Number(v).toFixed(2) };
    case 'billing_cycle': return CYCLE_LABEL[v] ? { value: v } : { error: 'invalidBillingCycle' };
    case 'currency': return CURRENCIES.includes(v) ? { value: v } : { error: 'invalidCurrency' };
    case 'auto_renewal': case 'auto_update': return { value: v === '1' ? '1' : '0' };
    case 'expire_date': return v === '' || validDate(v) ? { value: v } : { error: 'invalidExpireDate' };
    case 'traffic_limit': return v === '' || parseTrafficLimit(v) > 0 ? { value: v } : { error: 'invalidTrafficLimit' };
    case 'traffic_calc_type': return { value: CALC_LABEL[v] ? v : 'total' };
    case 'note': return { value: v.slice(0, 500) };
    default: return { value: v.slice(0, 128) };
  }
}

// ---------------------------------------------------------------- MD5（Agent 配置协商需要，Web Crypto 不保证支持 MD5，所以自己实现）
export function md5hex(str) {
  const bytes = enc.encode(str);
  const S = [7, 12, 17, 22, 5, 9, 14, 20, 4, 11, 16, 23, 6, 10, 15, 21];
  const K = new Uint32Array(64);
  for (let i = 0; i < 64; i++) K[i] = Math.floor(Math.abs(Math.sin(i + 1)) * 4294967296) >>> 0;
  const blocks = ((bytes.length + 8) >> 6) + 1;
  const buf = new Uint8Array(blocks * 64);
  buf.set(bytes);
  buf[bytes.length] = 0x80;
  const dv = new DataView(buf.buffer);
  dv.setUint32(blocks * 64 - 8, (bytes.length << 3) >>> 0, true);
  dv.setUint32(blocks * 64 - 4, Math.floor(bytes.length / 0x20000000), true);
  let a0 = 0x67452301, b0 = 0xefcdab89, c0 = 0x98badcfe, d0 = 0x10325476;
  for (let off = 0; off < buf.length; off += 64) {
    let A = a0, B = b0, C = c0, D = d0;
    for (let i = 0; i < 64; i++) {
      let F, g;
      if (i < 16) { F = (B & C) | (~B & D); g = i; }
      else if (i < 32) { F = (D & B) | (~D & C); g = (5 * i + 1) % 16; }
      else if (i < 48) { F = B ^ C ^ D; g = (3 * i + 5) % 16; }
      else { F = C ^ (B | ~D); g = (7 * i) % 16; }
      F = (F + A + K[i] + dv.getUint32(off + g * 4, true)) >>> 0;
      A = D; D = C; C = B;
      const sh = S[(i >> 4) * 4 + (i % 4)];
      B = (B + ((F << sh) | (F >>> (32 - sh)))) >>> 0;
    }
    a0 = (a0 + A) >>> 0; b0 = (b0 + B) >>> 0; c0 = (c0 + C) >>> 0; d0 = (d0 + D) >>> 0;
  }
  const out = new DataView(new ArrayBuffer(16));
  [a0, b0, c0, d0].forEach((v, i) => out.setUint32(i * 4, v, true));
  return [...new Uint8Array(out.buffer)].map((x) => x.toString(16).padStart(2, '0')).join('');
}

// Agent 动态配置串：字段顺序固定（上游协议要求），不接受空格、换行、百分号编码，总长不超过 512 字节。
// schema 3：基础字段；4：+connection_mode；5：+wss_report_interval；6：+ping_mode。
// 更高版本的格式未知，返回 null（调用方回 204，让 Agent 继续用本地配置，不会出错）。
export function buildAgentConfig(row, s, schema) {
  if (!(schema >= 3 && schema <= 6)) return null;
  // 节点自己的目标 > 全局设置 > 内置默认；全局留空等于使用内置默认
  const pick = (own, site, key) => (own && own.trim()) || (site && site.trim()) || DEFAULT_SETTINGS[key];
  const parts = [
    ['collect_interval', row.collect_interval], ['report_interval', row.report_interval],
    ['reset_day', row.reset_day], ['schema_version', schema],
    ['custom_ct', pick(row.custom_ct, s.custom_ct, 'custom_ct')], ['custom_cu', pick(row.custom_cu, s.custom_cu, 'custom_cu')],
    ['custom_cm', pick(row.custom_cm, s.custom_cm, 'custom_cm')], ['custom_bd', pick(row.custom_bd, s.custom_bd, 'custom_bd')],
    ['interface', row.interface || ''],
  ];
  if (schema >= 4) parts.push(['connection_mode', 'http']); // Lite 不提供 WebSocket，固定走 HTTP 上报
  if (schema >= 5) parts.push(['wss_report_interval', 2]);
  if (schema >= 6) parts.push(['ping_mode', s.ping_mode === 'icmp' ? 'icmp' : 'tcp']); // 全局设置；节点表里的 ping_mode 列不再使用
  return parts.map(([k, v]) => `${k}=${v}`).join('&');
}

// ---------------------------------------------------------------- 指标规范化
const num = (v) => { const n = Number(v); return Number.isFinite(n) ? n : 0; };
const reach = (v) => (v === undefined || v === null || v === '' || v === 0 || v === '0' ? '0' : '1');
// 上游语义：false/"false" = 未配置；null/"null" = 本轮探测超时；数字 = 有效值
function pingVal(v) {
  if (v === undefined || v === false || v === 'false') return false;
  if (v === null || v === 'null' || v === '') return null;
  const n = Number(v);
  return Number.isFinite(n) ? n : null;
}
function normalizeMetrics(m) {
  const o = {};
  for (const k of NUM_KEYS) o[k] = num(m[k]);
  o.load_avg = String(m.load_avg ?? '0 0 0').slice(0, 64);
  o.boot_time = String(m.boot_time ?? '0').slice(0, 32);
  for (const k of ['os', 'arch', 'kernel_version', 'cpu_info']) o[k] = String(m[k] ?? '').slice(0, 256);
  o.ip_v4 = reach(m.ip_v4);
  o.ip_v6 = reach(m.ip_v6);
  o.gpu_info = Array.isArray(m.gpu_info) ? JSON.stringify(m.gpu_info).slice(0, 4096)
    : typeof m.gpu_info === 'string' ? m.gpu_info.slice(0, 4096) : null;
  if (m.disk && typeof m.disk === 'object') {
    const d = {};
    for (const k of ['read_bps', 'write_bps', 'read_iops', 'write_iops', 'await_ms', 'util']) d[k] = num(m.disk[k]);
    if (Object.values(d).some((x) => x !== 0)) o.disk = d; // 6 项全为 0 时与上游一致：不返回 disk
  }
  for (const k of PING_KEYS) {
    const pingSrc = k === 'bd' ? (m.ping_bd ?? m.ping_bgp) : m['ping_' + k]; // Agent 文档里写作 ping_bgp
    o['ping_' + k] = pingVal(pingSrc);
    o['loss_' + k] = pingVal(m['loss_' + k]);
  }
  return o;
}
function pickMetrics(b) {
  if (b.metrics && typeof b.metrics === 'object') return b.metrics;
  if (b.payload?.metrics && typeof b.payload.metrics === 'object') return b.payload.metrics;
  const samples = Array.isArray(b.samples) ? b.samples : Array.isArray(b.batch) ? b.batch : null;
  if (samples && samples.length) {
    const last = [...samples].sort((x, y) => num(x?.ts) - num(y?.ts)).pop();
    const m = last?.metrics || last?.data || last?.payload || last;
    if (m && typeof m === 'object') return m;
  }
  return null;
}

// ---------------------------------------------------------------- 服务器行 -> API 对象
function shape(row, ctx) {
  const extra = { ...EXTRA_DEFAULTS, ...safeJson(row.extra) };
  const m = row.metrics ? safeJson(row.metrics) : normalizeMetrics({});
  const online = row.last_seen > 0 && ctx.now - row.last_seen <= ctx.thresholdMs;
  const out = {
    id: row.id, name: row.name, server_group: row.server_group,
    tags: extra.tags, note: extra.note, price: extra.price, billing_cycle: extra.billing_cycle,
    auto_renewal: extra.auto_renewal, currency: extra.currency, expire_date: extra.expire_date,
    traffic_limit: extra.traffic_limit, traffic_calc_type: extra.traffic_calc_type,
    auto_update: extra.auto_update, rx_correction: extra.rx_correction, tx_correction: extra.tx_correction,
    interface: row.interface, reset_day: row.reset_day, collect_interval: row.collect_interval,
    report_interval: row.report_interval, ping_mode: row.ping_mode,
    custom_ct: row.custom_ct, custom_cu: row.custom_cu, custom_cm: row.custom_cm, custom_bd: row.custom_bd,
    offline_notify_disabled: row.offline_notify_disabled, is_hidden: row.is_hidden, sort_order: row.sort_order,
    timestamp: row.created_at, ...m,
    region: row.region, agent_version: row.agent_version,
    last_updated: row.last_seen, is_online: online,
  };
  const limit = parseTrafficLimit(extra.traffic_limit);
  const used = trafficUsed(m, extra.traffic_calc_type);
  out.lite_traffic_limit_bytes = limit;
  out.lite_traffic_used_bytes = used;
  out.lite_traffic_percent = limit ? Math.round((used / limit) * 1000) / 10 : null;
  out.lite_expire_days_left = validDate(extra.expire_date) ? daysLeft(extra.expire_date, ymdInTz(ctx.now, ctx.tz)) : null;
  if (!ctx.admin) for (const k of ADMIN_ONLY_FIELDS) delete out[k];
  return out;
}
async function listShaped(env, ctx, { includeHidden }) {
  const { results } = await env.DB.prepare('SELECT * FROM servers ORDER BY sort_order ASC, created_at ASC').all();
  return (results || []).filter((r) => includeHidden || r.is_hidden !== '1').map((r) => shape(r, ctx));
}

// ---------------------------------------------------------------- /update（Agent 上报）
async function handleUpdate(request, env, ctx) {
  if (Number(request.headers.get('content-length') || 0) > MAX_BODY) return fail('Payload Too Large', 413);
  const text = await request.text();
  if (text.length > MAX_BODY) return fail('Payload Too Large', 413);
  let b;
  try { b = JSON.parse(text); } catch { return fail('Invalid JSON', 400); }
  if (!b || typeof b !== 'object' || Array.isArray(b)) return fail('Invalid report payload', 400);

  const id = typeof b.id === 'string' ? b.id : '';
  if (!ID_RE.test(id)) return fail('Invalid server ID', 400);
  if (typeof b.secret !== 'string' || !(await safeEqual(b.secret, env.API_SECRET))) return fail('Invalid secret', 401);

  const row = await env.DB.prepare(
    'SELECT id, name, status, status_since, offline_notify_disabled, collect_interval, report_interval, reset_day, ' +
    'interface, ping_mode, custom_ct, custom_cu, custom_cm, custom_bd FROM servers WHERE id = ?',
  ).bind(id).first();
  if (!row) return fail('Server not found', 404);

  const m = pickMetrics(b);
  if (!m) {
    // Agent 的流量修正确认包不带 metrics；Lite 从不下发修正，直接确认
    if ('rx_correction' in b || 'tx_correction' in b) return new Response('OK', { headers: { 'content-type': 'text/plain' } });
    return fail('Missing metrics', 400);
  }

  const now = Date.now();
  const region = request.cf?.country || request.headers.get('cf-ipcountry') || '';
  const version = (request.headers.get('x-agent-version') || '').slice(0, 32);
  // batch 在 D1 里是一个事务：状态切换与指标写入不会被 Cron 插进来打断
  const results = await env.DB.batch([
    env.DB.prepare("UPDATE servers SET status = 'ONLINE', status_since = ? WHERE id = ? AND status = 'OFFLINE'").bind(now, id),
    env.DB.prepare(
      "UPDATE servers SET metrics = ?, last_seen = ?, region = ?, agent_version = ?, " +
      "status_since = CASE WHEN status = 'UNINIT' THEN ? ELSE status_since END, " +
      "status = CASE WHEN status = 'UNINIT' THEN 'ONLINE' ELSE status END WHERE id = ?",
    ).bind(JSON.stringify(normalizeMetrics(m)), now, region, version, now, id),
  ]);

  // 只有真正完成 OFFLINE -> ONLINE 的那一次上报才发恢复通知
  if (results[0]?.meta?.changes === 1 && row.offline_notify_disabled !== '1') {
    ctx.waitUntil((async () => {
      const s = await loadSettings(env);
      if (!notifyEnabled(s)) return;
      await sendBark(env, s, `${row.name} 已恢复`,
        `恢复时间：${fmtTime(now, s.notification_timezone)}\n离线时长：约 ${fmtDuration(now - row.status_since)}`);
    })());
  }
  // 动态配置协商：旧版 Agent（无 schema 头）回 OK；新版 Agent 配置没变回 204，变了回完整配置串
  const schema = parseInt(request.headers.get('x-agent-config-schema') || '', 10);
  if (!schema) return new Response('OK', { headers: { 'content-type': 'text/plain' } });
  const body = buildAgentConfig(row, await loadSettings(env), schema);
  if (body === null) return new Response(null, { status: 204 });
  const sum = md5hex(body);
  if ((request.headers.get('x-agent-config-md5') || '') === sum) return new Response(null, { status: 204 });
  return new Response(body, {
    headers: {
      'content-type': 'application/x-www-form-urlencoded; charset=utf-8',
      'x-agent-config-schema': String(schema), 'x-agent-config-md5': sum,
    },
  });
}

// ---------------------------------------------------------------- 公开 REST
async function handlePublic(request, env, url, path) {
  const admin = await isAdmin(request, env);
  const isPublic = env.IS_PUBLIC === 'true';
  if (path === '/api/config') {
    const s = await loadSettings(env);
    return json({
      version: env.COMPAT_VERSION || COMPAT_VERSION, is_public: isPublic, authorization: admin,
      turnstile_enabled: false, turnstile_login_enabled: false, turnstile_site_key: '',
      custom_ct_name: s.custom_ct_name, custom_cu_name: s.custom_cu_name,
      custom_cm_name: s.custom_cm_name, custom_bd_name: s.custom_bd_name,
      site_title: s.site_title, display_mode: 'bar', preferred_theme: 'auto', default_language: 'auto',
      verified: false, turnstile_verified: null, theme_options: {},
      frontend_ws_timeout_minutes: 0, long_history_points: 120, latency_window: { points: 20, hours: 2 },
    });
  }
  if (!isPublic && !admin) return fail('Unauthorized', 401);

  const s = await loadSettings(env);
  const ctx = { now: Date.now(), thresholdMs: thresholdSec(s) * 1000, admin, tz: s.notification_timezone };

  if (path === '/api/servers') {
    const servers = (await listShaped(env, ctx, { includeHidden: admin })).map((x) => ({ ...x, ping: [], loss: [] }));
    const online = servers.filter((x) => x.is_online);
    const regionStats = {};
    for (const x of servers) if (x.region) regionStats[x.region] = (regionStats[x.region] || 0) + 1;
    const sum = (arr, k) => arr.reduce((a, x) => a + x[k], 0);
    return json({
      servers, latestReportUpdates: [],
      stats: {
        total: servers.length, online: online.length, offline: servers.length - online.length,
        globalSpeedIn: sum(online, 'net_in_speed'), globalSpeedOut: sum(online, 'net_out_speed'),
        globalNetTx: sum(servers, 'net_tx'), globalNetRx: sum(servers, 'net_rx'),
      },
      regionStats,
      sysConfig: { show_price: true, show_expire: true, show_tf: true, show_three_net_details: true, display_mode: 'bar' },
    });
  }

  const id = url.searchParams.get('id');
  if (path === '/api/server') {
    if (!id) return fail('Missing ID', 400);
    const row = await env.DB.prepare('SELECT * FROM servers WHERE id = ?').bind(id).first();
    if (!row || (row.is_hidden === '1' && !admin)) return fail('Server not found', 404);
    return json({ ...shape(row, ctx), latestReportUpdates: [], sysConfig: { long_history_points: 120 } });
  }
  if (path === '/api/history/all') {
    if (!id) return fail('Missing ID', 400);
    const hours = Number(url.searchParams.get('hours') ?? 24);
    if (!HOURS_OK.includes(hours)) return fail('Invalid hours', 400);
    if (hours > 24 && !admin) return fail('Unauthorized', 401);
    const row = await env.DB.prepare('SELECT id, is_hidden FROM servers WHERE id = ?').bind(id).first();
    if (!row || (row.is_hidden === '1' && !admin)) return fail('Server not found', 404);
    return json([], 200, { 'X-Cache': 'MISS' }); // Lite 不保存历史
  }
  return fail('Not Found', 404);
}

// ---------------------------------------------------------------- /admin/api
async function handleAdmin(request, env) {
  if (request.method !== 'POST') return fail('Method Not Allowed', 405);
  let b;
  try { b = await request.json(); } catch { return fail('Invalid JSON', 400); }
  if (!b || typeof b !== 'object') return fail('Invalid JSON', 400);
  const action = b.action;

  if (action === 'login') {
    if (!b.username || !b.password) return fail('missingCredentials', 400);
    const userOk = await safeEqual(b.username, env.ADMIN_USERNAME || 'admin');
    const passOk = await safeEqual(b.password, env.ADMIN_PASSWORD || env.API_SECRET);
    if (!(userOk && passOk)) return fail('invalidCredentials', 401);
    return json({ success: true, token: await signJwt(env), message: 'loginSuccessful' });
  }
  if (action === 'clear_theme_preview_auth') return json({ success: true });
  if (!(await isAdmin(request, env))) return fail('Unauthorized', 401);

  const s = await loadSettings(env);
  const ctx = { now: Date.now(), thresholdMs: thresholdSec(s) * 1000, admin: true, tz: s.notification_timezone };

  switch (action) {
    case 'get_settings': {
      const { tg_bot_token, ...rest } = s;
      return json({
        success: true,
        settings: {
          ...rest, tg_bot_token: '', tg_bot_token_configured: !!tg_bot_token, // Bark 地址只写不读
          is_public: env.IS_PUBLIC === 'true' ? 'true' : 'false', show_price: 'true', show_expire: 'true',
          show_tf: 'true', display_mode: 'bar', preferred_theme: 'auto', default_language: 'auto',
          frontend_ws_timeout_minutes: '0', long_history_points: '120', tg_chat_id: '',
          expire_reminder: '0', expire_notification_time: '12', turnstile_enabled: 'false',
          turnstile_login_enabled: 'false', turnstile_site_key: '', theme_url: '',
          username: env.ADMIN_USERNAME || 'admin',
        },
        agent_install_ready: REPO_RE.test(String(env.AGENT_REPO || '').trim()),
        api_secret: env.API_SECRET, // 与上游一致：登录后可见，Monito 生成 Agent 安装命令时需要
      });
    }
    case 'save_settings': return saveSettings(env, b.settings);
    case 'send_test_notification': {
      const token = (typeof b.tg_bot_token === 'string' && b.tg_bot_token.trim()) || s.tg_bot_token;
      if (!barkTarget(token)) return fail('tgBotTokenRequired', 400);
      const ok = await sendBark(env, s, 'CFSM Lite 测试通知', '收到这条消息说明 Bark 配置正确', token);
      return ok ? json({ success: true, message: 'testNotificationSent' }) : fail('testNotificationFailed', 400);
    }
    case 'list': {
      const servers = await listShaped(env, ctx, { includeHidden: true });
      const online = servers.filter((x) => x.is_online);
      const sum = (arr, k) => arr.reduce((a, x) => a + x[k], 0);
      return json({
        success: true, servers,
        stats: {
          total: servers.length, online: online.length, offline: servers.length - online.length,
          total_cpu: sum(online, 'cpu'), total_net_in: sum(online, 'net_in_speed'), total_net_out: sum(online, 'net_out_speed'),
          avg_cpu: online.length ? (sum(online, 'cpu') / online.length).toFixed(2) : 0,
        },
      });
    }
    case 'add': {
      const name = String(b.name ?? '').trim();
      if (name.length < 1 || name.length > 100) return fail('服务器名称无效', 400);
      const id = crypto.randomUUID();
      const max = await env.DB.prepare('SELECT COALESCE(MAX(sort_order), -1) AS m FROM servers').first();
      await env.DB.prepare('INSERT INTO servers (id, name, server_group, sort_order, created_at) VALUES (?, ?, ?, ?, ?)')
        .bind(id, name, String(b.server_group || 'Default').slice(0, 64), (max?.m ?? -1) + 1, Date.now()).run();
      return json({ success: true, id, message: 'serverAdded' });
    }
    case 'edit': return editServer(env, b);
    case 'delete': {
      if (!UUID_RE.test(String(b.id || ''))) return fail('invalidServerId', 400);
      await env.DB.prepare('DELETE FROM servers WHERE id = ?').bind(b.id).run();
      await clearMarks(env, [b.id]);
      return json({ success: true, message: 'serverDeleted' });
    }
    case 'batch_delete': {
      if (!Array.isArray(b.ids) || !b.ids.length) return fail('selectServersToDelete', 400);
      if (!b.ids.every((x) => UUID_RE.test(String(x)))) return fail('invalidServerIdInList', 400);
      await env.DB.batch(b.ids.map((x) => env.DB.prepare('DELETE FROM servers WHERE id = ?').bind(x)));
      await clearMarks(env, b.ids);
      return json({ success: true, message: 'batchDeleted' });
    }
    case 'save_order': {
      if (!Array.isArray(b.orders) || !b.orders.length) return fail('missingSortData', 400);
      if (!b.orders.every((x) => UUID_RE.test(String(x)))) return fail('invalidSortId', 400);
      await env.DB.batch(b.orders.map((x, i) => env.DB.prepare('UPDATE servers SET sort_order = ? WHERE id = ?').bind(i, x)));
      return json({ success: true, message: 'sortOrderSaved' });
    }
    case 'export_servers':
      return json({ success: true, servers: await listShaped(env, ctx, { includeHidden: true }), message: 'serversExported' });
    case 'import_servers': return importServers(env, b.servers);
    default: return fail('unknownAction', 400);
  }
}

async function saveSettings(env, input) {
  if (!input || typeof input !== 'object') return fail('invalidSettings', 400);
  const writes = [];
  const ignored = [];
  for (const [k, raw] of Object.entries(input)) {
    if (!SAVEABLE_SETTINGS.includes(k)) { ignored.push(k); continue; }
    const v = typeof raw === 'string' ? raw.trim() : String(raw ?? '');
    if (k === 'tg_bot_token' && !v) continue; // 空值不覆盖：Bark 地址不会被设置页回传的空字符串清掉
    if (k === 'tg_notify' && !(v === '0' || (/^\d+$/.test(v) && +v >= 2 && +v <= 30))) return fail('invalidNotifyValue', 400);
    if (k === 'ping_mode' && !['tcp', 'icmp'].includes(v)) return fail('invalidPingMode', 400);
    if (k === 'notification_timezone') {
      try { new Intl.DateTimeFormat('zh-CN', { timeZone: v }); } catch { return fail('invalidTimezone', 400); }
    }
    if (/^custom_(ct|cu|cm|bd)$/.test(k) && !HOST_RE.test(v)) return fail('invalidPingNodeFormat', 400);
    let out = v;
    if (k === 'expire_notify_days') {
      const l = parseIntList(v, 0, 365, 6);
      if (!l) return fail('invalidExpireDays', 400);
      out = l.sort((a, b) => b - a).join(',');
    }
    if (k === 'expire_notify_hour') {
      if (!/^\d{1,2}$/.test(v) || +v > 23) return fail('invalidExpireHour', 400);
      out = String(+v);
    }
    if (k === 'traffic_notify_percents') {
      const l = parseIntList(v, 1, 100, 4);
      if (!l) return fail('invalidTrafficPercents', 400);
      out = l.sort((a, b) => a - b).join(',');
    }
    writes.push(env.DB.prepare('INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value')
      .bind(k, out.slice(0, 512)));
  }
  if (writes.length) await env.DB.batch(writes);
  return json({ success: true, message: 'updateSuccess', ...(ignored.length ? { ignored } : {}) });
}

// 解析并校验服务器配置字段；只处理出现的字段，缺省的保持原值（比上游整行覆盖更安全）
function parseServerFields(b) {
  const cols = {};
  const extra = {};
  const bad = (e) => ({ error: e });
  if ('name' in b) {
    const n = String(b.name ?? '').trim();
    if (n.length < 1 || n.length > 100) return bad('服务器名称无效');
    cols.name = n;
  }
  if ('server_group' in b) cols.server_group = String(b.server_group || 'Default').slice(0, 64);
  if ('is_hidden' in b) cols.is_hidden = String(b.is_hidden) === '1' ? '1' : '0';
  if ('offline_notify_disabled' in b) cols.offline_notify_disabled = String(b.offline_notify_disabled) === '1' ? '1' : '0';
  if ('reset_day' in b) {
    const n = Number(b.reset_day);
    if (!Number.isInteger(n) || n < 0 || n > 31) return bad('invalidResetDay');
    cols.reset_day = n;
  }
  if ('collect_interval' in b) {
    const n = Number(b.collect_interval);
    if (![0, 1, 2, 5, 10].includes(n)) return bad('invalidCollectInterval');
    cols.collect_interval = n;
  }
  if ('report_interval' in b) {
    const n = Number(b.report_interval);
    if (![30, 60, 120, 180].includes(n)) return bad('invalidReportInterval');
    cols.report_interval = n;
  }
  if ('ping_mode' in b) {
    if (!['tcp', 'icmp'].includes(b.ping_mode)) return bad('invalidPingMode');
    cols.ping_mode = b.ping_mode;
  }
  if ('interface' in b) {
    const v = String(b.interface ?? '').trim();
    if (!/^[A-Za-z0-9._,:-]{0,128}$/.test(v)) return bad('invalidInterface');
    cols.interface = v;
  }
  for (const k of ['custom_ct', 'custom_cu', 'custom_cm', 'custom_bd']) {
    if (k in b) {
      const v = String(b[k] ?? '').trim();
      if (!HOST_RE.test(v)) return bad('invalidPingNodeFormat');
      cols[k] = v;
    }
  }
  for (const k of Object.keys(EXTRA_DEFAULTS)) {
    if (!(k in b)) continue;
    if (k === 'rx_correction' || k === 'tx_correction') {
      if (b[k] === null || b[k] === '') { extra[k] = null; continue; }
      const n = Number(b[k]);
      if (!Number.isFinite(n) || n < 0 || n > 1000000) return bad('invalidTrafficCorrection');
      extra[k] = n;
    } else {
      const r = normExtra(k, b[k]);
      if (r.error) return bad(r.error);
      extra[k] = r.value;
    }
  }
  return { cols, extra };
}

async function editServer(env, b) {
  if (!UUID_RE.test(String(b.id || ''))) return fail('invalidServerId', 400);
  const parsed = parseServerFields(b);
  if (parsed.error) return fail(parsed.error, 400);
  const row = await env.DB.prepare('SELECT id, extra FROM servers WHERE id = ?').bind(b.id).first();
  if (!row) return fail('Server not found', 404);
  const sets = Object.keys(parsed.cols).map((k) => `${k} = ?`);
  const vals = Object.values(parsed.cols);
  if (Object.keys(parsed.extra).length) {
    sets.push('extra = ?');
    vals.push(JSON.stringify({ ...safeJson(row.extra), ...parsed.extra }));
  }
  if (sets.length) await env.DB.prepare(`UPDATE servers SET ${sets.join(', ')} WHERE id = ?`).bind(...vals, b.id).run();
  return json({ success: true, message: 'serverUpdated' });
}

async function importServers(env, list) {
  if (!Array.isArray(list) || !list.length) return fail('noServersToImport', 400);
  const imported = []; const skippedIds = [];
  const max = await env.DB.prepare('SELECT COALESCE(MAX(sort_order), -1) AS m FROM servers').first();
  let order = (max?.m ?? -1) + 1;
  for (const item of list) {
    const id = String(item?.id || '');
    const parsed = UUID_RE.test(id) ? parseServerFields({ name: 'Server', ...item }) : { error: 'bad id' };
    const exists = !parsed.error && (await env.DB.prepare('SELECT 1 AS x FROM servers WHERE id = ?').bind(id).first());
    if (parsed.error || exists) { skippedIds.push(id); continue; }
    const cols = { id, created_at: Date.now(), sort_order: order++, ...parsed.cols,
      extra: JSON.stringify(Object.fromEntries(Object.entries(parsed.extra).filter(([, v]) => v !== undefined))) };
    const keys = Object.keys(cols);
    await env.DB.prepare(`INSERT INTO servers (${keys.join(', ')}) VALUES (${keys.map(() => '?').join(', ')})`)
      .bind(...Object.values(cols)).run();
    imported.push(id);
  }
  return json({
    success: true, imported: imported.length, skipped: skippedIds.length, skippedIds,
    message: imported.length ? 'serversImported' : 'noServersImported',
  });
}

// ---------------------------------------------------------------- Cron：离线检测
export async function checkOffline(env) {
  const s = await loadSettings(env);
  const now = Date.now();
  const cutoff = now - thresholdSec(s) * 1000;
  // 单条带条件的 UPDATE ... RETURNING：只有被它切换的行才会触发通知，重复执行也不会重复通知
  const { results } = await env.DB.prepare(
    "UPDATE servers SET status = 'OFFLINE', status_since = ? WHERE status = 'ONLINE' AND last_seen < ? " +
    'RETURNING id, name, last_seen, offline_notify_disabled',
  ).bind(now, cutoff).all();
  if (!notifyEnabled(s)) return (results || []).length;
  for (const r of results || []) {
    if (r.offline_notify_disabled === '1') continue;
    await sendBark(env, s, `${r.name} 离线`,
      `最后上报：${fmtTime(r.last_seen, s.notification_timezone)}\n已超过 ${Math.round(thresholdSec(s) / 60)} 分钟没有上报`);
  }
  return (results || []).length;
}


// ---------------------------------------------------------------- Cron：到期提醒与流量提醒
async function setMark(env, key, value) {
  await env.DB.prepare('INSERT INTO settings (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value')
    .bind(key, value).run();
}
async function clearMarks(env, ids) {
  const stmts = [];
  for (const id of ids) {
    stmts.push(env.DB.prepare('DELETE FROM settings WHERE key = ?').bind('ntf:exp:' + id));
    stmts.push(env.DB.prepare('DELETE FROM settings WHERE key = ?').bind('ntf:traf:' + id));
  }
  if (stmts.length) await env.DB.batch(stmts);
}

// 检查所有节点的到期日与当月流量，必要时发 Bark。每个（节点、到期日或统计周期、档位）只提醒一次，
// 去重标记保存在 settings 表里以 ntf: 开头的行，所以不需要改数据库结构。
// 到期提醒：每天通知时区的「提醒时间」之后开始检查；流量提醒：每 5 分钟检查一次（force 用于测试）。
export async function checkAlerts(env, now = Date.now(), { force = false } = {}) {
  const s = await loadSettings(env);
  if (!notifyEnabled(s) || (!force && new Date(now).getUTCMinutes() % 5 !== 0)) return 0;
  const tz = s.notification_timezone;
  const { results: rows } = await env.DB.prepare('SELECT id, name, extra, reset_day, metrics, last_seen FROM servers').all();
  const { results: markRows } = await env.DB.prepare("SELECT key, value FROM settings WHERE key LIKE 'ntf:%'").all();
  const marks = Object.fromEntries((markRows || []).map((r) => [r.key, r.value]));
  const today = ymdInTz(now, tz);
  const doExpiry = hourInTz(now, tz) >= (parseInt(s.expire_notify_hour, 10) || 0);
  const expStages = (parseIntList(s.expire_notify_days, 0, 365, 6) || []).sort((a, b) => b - a);
  const trafStages = (parseIntList(s.traffic_notify_percents, 1, 100, 4) || []).sort((a, b) => a - b);
  const thresholdMs = thresholdSec(s) * 1000;
  let sent = 0;

  for (const r of rows || []) {
    const extra = { ...EXTRA_DEFAULTS, ...safeJson(r.extra) };

    if (doExpiry && expStages.length && validDate(extra.expire_date)) {
      const days = daysLeft(extra.expire_date, today);
      const stage = expireStage(days, expStages);
      const want = `${extra.expire_date}#${stage}`;
      if (stage !== null && marks['ntf:exp:' + r.id] !== want) {
        const title = days > 0 ? `${r.name} 还有 ${days} 天到期` : days === 0 ? `${r.name} 今天到期` : `${r.name} 已过期 ${-days} 天`;
        const lines = [`到期日：${extra.expire_date}`];
        const price = priceText(extra);
        if (price) lines.push(`续费金额：${price}`);
        lines.push(extra.auto_renewal === '1' ? '已设置自动续费，请确认账户余额充足' : '续费后请在 CFSM Lite 里更新到期日');
        if (await sendBark(env, s, title, lines.join('\n'))) {
          await setMark(env, 'ntf:exp:' + r.id, want);
          sent++;
        }
      }
    }

    const limit = parseTrafficLimit(extra.traffic_limit);
    const online = r.last_seen > 0 && now - r.last_seen <= thresholdMs; // 节点离线时数据是旧的，不据此提醒
    if (limit && trafStages.length && online && r.metrics) {
      const used = trafficUsed(safeJson(r.metrics), extra.traffic_calc_type);
      const pct = (used / limit) * 100;
      const stage = trafficStage(pct, trafStages);
      if (stage !== null) {
        const period = periodStart(now, r.reset_day, tz);
        const [mp, ms] = String(marks['ntf:traf:' + r.id] || '').split('#');
        if (!(mp === period && Number(ms) >= stage)) {
          const reset = r.reset_day > 0 ? `每月 ${r.reset_day} 日重置` : '不自动重置';
          const body = `已用 ${fmtBytes(used)} / ${fmtBytes(limit)}（${CALC_LABEL[extra.traffic_calc_type] || CALC_LABEL.total}）\n${reset}`;
          if (await sendBark(env, s, `${r.name} 本月流量已用 ${Math.floor(pct)}%`, body)) {
            await setMark(env, 'ntf:traf:' + r.id, `${period}#${stage}`);
            sent++;
          }
        }
      }
    }
  }
  return sent;
}

// 每分钟由 Cron 触发：先检测离线，再检查到期与流量；后者出错不影响前者
export async function runCron(env, now = Date.now()) {
  await checkOffline(env);
  try { await checkAlerts(env, now); } catch (e) { console.error('alerts failed:', e?.message || e); }
}

// ---------------------------------------------------------------- /api/ws（WebSocket 实时推送）
// 没有 Durable Object：每个连接自己定时查询 D1，把新上报的节点推送给客户端；
// 另外在收到客户端 ping 时顺带查询一次，所以即使定时器不触发，也不会长时间没有数据。
const WS_POLL_MS = 15000;
const WS_MIN_GAP_MS = 4000;

function wsToken(request, url) {
  const m = /^Bearer\s+(.+)$/i.exec(request.headers.get('authorization') || '');
  if (m) return m[1];
  const c = /(?:^|;\s*)cfsm_auth=([^;]+)/.exec(request.headers.get('cookie') || '');
  if (c) return decodeURIComponent(c[1]);
  return url.searchParams.get('token') || url.searchParams.get('auth_token') || url.searchParams.get('ws_token') || '';
}

// 连接建立后的全部逻辑。sock 只需要有 send、close、addEventListener，所以可以在测试里用假对象代替。
export function createWsSession(sock, env, scope0, opts = {}) {
  const now = opts.now || Date.now;
  const timers = opts.timers || { setInterval, clearInterval };
  let scope = scope0;
  let ids = scope === 'all' ? null : new Set([scope]); // null 表示全部节点
  let wmark = now() - 20000; // 连接时补发最近 20 秒内的上报，避免和首次拉取列表之间漏数据
  const sent = new Map();
  let busy = false, closed = false, lastPoll = 0, failures = 0, timer = null;

  const send = (o) => { try { sock.send(typeof o === 'string' ? o : JSON.stringify(o)); } catch { /* 连接已断开 */ } };
  const stop = () => { closed = true; if (timer !== null) timers.clearInterval(timer); };
  const shut = (code, reason) => { try { sock.close(code, reason); } catch { /* 已关闭 */ } stop(); };

  async function poll(force = false) {
    if (busy || closed || (ids && ids.size === 0)) return;
    if (!force && now() - lastPoll < WS_MIN_GAP_MS) return;
    busy = true;
    lastPoll = now();
    try {
      const { results } = await env.DB.prepare('SELECT id, metrics, last_seen FROM servers WHERE last_seen >= ? AND metrics IS NOT NULL').bind(wmark).all();
      failures = 0;
      let maxSeen = wmark;
      const updates = [];
      for (const r of results || []) {
        if (r.last_seen > maxSeen) maxSeen = r.last_seen;
        if (ids && !ids.has(r.id)) continue;
        if ((sent.get(r.id) || 0) >= r.last_seen) continue;
        sent.set(r.id, r.last_seen);
        updates.push({ serverId: r.id, samples: [{ ts: r.last_seen, data: { ...safeJson(r.metrics), last_updated: r.last_seen } }] });
      }
      wmark = Math.max(wmark, maxSeen - 1000); // 留 1 秒余量，覆盖同一毫秒内的并发写入
      if (updates.length) send({ type: 'batchUpdate', ts: now(), updates });
    } catch (e) {
      // 偶发失败忽略；连续 3 次失败（例如超出免费版单次调用的查询次数）就断开，让客户端重连后重新计数
      failures++;
      if (failures >= 3) shut(1011, 'database unavailable');
    } finally {
      busy = false;
    }
  }

  sock.addEventListener('message', (ev) => {
    if (closed) return;
    let msg = null;
    try { msg = JSON.parse(String(ev.data)); } catch { return; }
    if (!msg || typeof msg !== 'object') return;
    if (msg.type === 'ping') { send('{"type":"pong"}'); poll(); return; }
    if (msg.type !== 'subscribe') return;
    const sc = msg.scope === undefined ? scope : msg.scope;
    const list = msg.ids;
    const okScope = typeof sc === 'string' && (sc === 'all' || ID_RE.test(sc));
    const okIds = list === undefined || (Array.isArray(list) && list.length <= 500 && list.every((x) => typeof x === 'string' && ID_RE.test(x)));
    if (!okScope || !okIds) { shut(1008, 'Invalid subscription'); return; }
    scope = sc;
    ids = sc === 'all' ? (Array.isArray(list) ? new Set(list) : null) : new Set([sc]);
    send({ type: 'subscribed', ts: now(), subscribed: sc, count: ids ? ids.size : 0 });
    wmark = now() - 20000;
    sent.clear();
    poll(true);
  });
  sock.addEventListener('close', stop);
  sock.addEventListener('error', stop);

  send({ type: 'hello', ts: now(), subscribed: scope });
  timer = timers.setInterval(() => { poll(); }, WS_POLL_MS);
  return { poll };
}

async function handleWs(request, env, url) {
  if ((request.headers.get('upgrade') || '').toLowerCase() !== 'websocket') {
    return new Response('Expected WebSocket upgrade request', { status: 426, headers: { 'content-type': 'text/plain' } });
  }
  if (env.IS_PUBLIC !== 'true') {
    const t = wsToken(request, url);
    if (!t || !(await verifyJwt(env, t))) return fail('Unauthorized', 401);
  }
  const scope = url.searchParams.get('subscribe') || 'all';
  if (scope !== 'all' && !ID_RE.test(scope)) return new Response('Invalid subscription scope', { status: 400, headers: { 'content-type': 'text/plain' } });
  if (typeof WebSocketPair === 'undefined') return fail('WebSocket not enabled', 503);
  const [client, server] = Object.values(new WebSocketPair());
  server.accept();
  createWsSession(server, env, scope);
  return new Response(null, { status: 101, webSocket: client });
}

// ---------------------------------------------------------------- 入口
// ---------------------------------------------------------------- Agent 安装脚本（由 Worker 提供，仓库名来自变量 AGENT_REPO）
const REPO_RE = /^[A-Za-z0-9_.-]{1,100}\/[A-Za-z0-9_.-]{1,100}$/;
const INSTALL_SH = String.raw`#!/bin/sh
# CFSM Lite Agent 安装脚本。由 Worker 的 /install.sh 提供（已填入发布仓库），也可以直接保存后执行。
# 用法：
#   sh install.sh -i <节点ID> -s <上报密钥> -u <https://你的域名/update>
#   sh install.sh --uninstall
set -eu

REPO="__REPO__"
BIN=/opt/cfsm-lite/cfsm-lite-agent
ENV_DIR=/opt/cfsm-lite
ENV_FILE=/opt/cfsm-lite/agent.env
UNIT=/etc/systemd/system/cfsm-lite-agent.service

die() { echo "错误: $*" >&2; exit 1; }

ACTION=install
ID=""
SECRET=""
URL=""
TAG=latest
while [ $# -gt 0 ]; do
  case "$1" in
    -i) [ $# -ge 2 ] || die "-i 后面需要节点 ID"; ID="$2"; shift 2 ;;
    -s) [ $# -ge 2 ] || die "-s 后面需要上报密钥"; SECRET="$2"; shift 2 ;;
    -u) [ $# -ge 2 ] || die "-u 后面需要上报地址"; URL="$2"; shift 2 ;;
    -v) [ $# -ge 2 ] || die "-v 后面需要版本号，例如 v0.1.0"; TAG="$2"; shift 2 ;;
    --uninstall) ACTION=uninstall; shift ;;
    *) die "不认识的参数: $1" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "需要 root 权限：请用 root 执行，或在 sh 前面加 sudo"
command -v systemctl >/dev/null 2>&1 || die "只支持使用 systemd 的 Linux 系统"

if [ "$ACTION" = uninstall ]; then
  systemctl disable --now cfsm-lite-agent >/dev/null 2>&1 || true
  rm -f "$UNIT" "$BIN"
  rm -rf "$ENV_DIR" /var/lib/private/cfsm-lite /var/lib/cfsm-lite
  systemctl daemon-reload
  echo "已卸载 cfsm-lite-agent"
  exit 0
fi

command -v curl >/dev/null 2>&1 || die "需要先安装 curl"
[ -n "$ID" ] && [ -n "$SECRET" ] && [ -n "$URL" ] || die "缺少参数：-i 节点ID  -s 上报密钥  -u 上报地址"
case "$URL" in https://*) ;; *) die "上报地址必须以 https:// 开头" ;; esac
case "$ID$SECRET$URL" in *[!A-Za-z0-9._:+/=?@-]*) die "节点 ID、密钥或地址里含有不支持的字符（只允许字母、数字和 . _ : + / = ? @ -）" ;; esac

case "$(uname -m)" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "不支持的 CPU 架构: $(uname -m)（只支持 x86_64 和 arm64）" ;;
esac

if [ "$TAG" = latest ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/$TAG"
fi
NAME="cfsm-lite-agent-linux-$ARCH"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "下载 $NAME ..."
curl -fsSL "$BASE/$NAME" -o "$TMP/$NAME" || die "下载失败: $BASE/$NAME（请确认仓库 $REPO 已发布 Release）"
if command -v sha256sum >/dev/null 2>&1 && curl -fsSL "$BASE/SHA256SUMS" -o "$TMP/SHA256SUMS" 2>/dev/null; then
  (cd "$TMP" && grep " $NAME\$" SHA256SUMS | sha256sum -c - >/dev/null) || die "文件校验失败，可能下载损坏或被篡改，已中止"
  echo "校验通过"
else
  echo "警告: 无法校验文件完整性（缺少 sha256sum 或 SHA256SUMS）" >&2
fi

mkdir -p "$(dirname "$BIN")"
chmod 755 "$(dirname "$BIN")"
install -m 0755 "$TMP/$NAME" "$BIN.new"
mv -f "$BIN.new" "$BIN"

umask 077
mkdir -p "$ENV_DIR"
chmod 755 "$ENV_DIR"
cat > "$ENV_FILE" <<EOT
CFSM_URL=$URL
CFSM_ID=$ID
CFSM_SECRET=$SECRET
CFSM_STATE=/var/lib/cfsm-lite/state.json
EOT
chmod 600 "$ENV_FILE"

cat > "$UNIT" <<EOT
[Unit]
Description=CFSM Lite Agent
After=network-online.target
Wants=network-online.target

[Service]
EnvironmentFile=$ENV_FILE
ExecStart=$BIN
Restart=always
RestartSec=10
DynamicUser=yes
StateDirectory=cfsm-lite
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
CapabilityBoundingSet=

[Install]
WantedBy=multi-user.target
EOT
chmod 644 "$UNIT"

systemctl daemon-reload
systemctl enable cfsm-lite-agent >/dev/null 2>&1
systemctl restart cfsm-lite-agent
sleep 3
if systemctl is-active --quiet cfsm-lite-agent; then
  echo "安装完成。查看运行日志: journalctl -u cfsm-lite-agent -f"
else
  echo "服务没有正常启动，请查看日志: journalctl -u cfsm-lite-agent -n 50" >&2
  exit 1
fi
`;
function installScript(env) {
  const repo = String(env.AGENT_REPO || '').trim();
  if (!REPO_RE.test(repo)) {
    return new Response('Worker 变量 AGENT_REPO 未设置或格式不对（应为 GitHub用户名/仓库名）', { status: 404, headers: { 'content-type': 'text/plain; charset=utf-8' } });
  }
  return new Response(INSTALL_SH.replace('__REPO__', repo), {
    headers: { 'content-type': 'text/plain; charset=utf-8', 'cache-control': 'no-store' },
  });
}

// ---------------------------------------------------------------- 管理页面（单页，内嵌，无外部依赖）
const ADMIN_HTML = String.raw`<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1,viewport-fit=cover">
<meta name="robots" content="noindex,nofollow">
<title>CFSM Lite 管理</title>
<style>
:root{--bg:#f4f5f7;--card:#fff;--fg:#1c2330;--mut:#667085;--line:#e4e7ec;--pri:#2563eb;--ok:#16a34a;--bad:#dc2626;--off:#98a2b3}
@media (prefers-color-scheme:dark){:root{--bg:#0f1318;--card:#181e26;--fg:#e6e9ef;--mut:#8b95a5;--line:#2a323d;--pri:#5b8def;--ok:#34d399;--bad:#f87171;--off:#667085}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI","PingFang SC","Microsoft YaHei",sans-serif;padding:env(safe-area-inset-top) env(safe-area-inset-right) env(safe-area-inset-bottom) env(safe-area-inset-left)}
#app{max-width:760px;margin:0 auto;padding:12px}
h1{font-size:22px;margin:0 0 4px}
.top{display:flex;justify-content:space-between;align-items:center;padding:6px 2px 10px}
.tabs{display:flex;gap:6px;margin-bottom:12px}
.tab{flex:1;padding:9px 0;border:1px solid var(--line);background:var(--card);color:var(--fg);border-radius:10px;font:inherit;cursor:pointer}
.tab.on{background:var(--pri);border-color:var(--pri);color:#fff}
.card{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:12px;margin-bottom:10px}
.row{display:flex;gap:8px;align-items:center;flex-wrap:wrap}
.row>input,.row>select{flex:1;min-width:0}
.between{justify-content:space-between}
.gap{gap:6px}
.btn{padding:8px 14px;border:1px solid var(--line);background:var(--card);color:var(--fg);border-radius:9px;font:inherit;cursor:pointer}
.btn.pri{background:var(--pri);border-color:var(--pri);color:#fff}
.btn.bad{color:var(--bad)}
.btn.sm{padding:4px 10px;font-size:13px}
.btn[disabled]{opacity:.5}
.field{display:block;margin:10px 0}
.field>span{display:block;font-size:13px;color:var(--mut);margin-bottom:3px}
.field small{display:block;margin-top:3px}
input[type=text],input[type=password],input[type=number],input[type=date],select,textarea{width:100%;padding:8px 10px;border:1px solid var(--line);border-radius:9px;background:var(--bg);color:var(--fg);font:inherit}
input[type=checkbox]{width:18px;height:18px;vertical-align:middle;margin-right:6px}
textarea{resize:vertical}
.mono{font:12px/1.4 ui-monospace,Menlo,Consolas,monospace;word-break:break-all}
.mut{color:var(--mut)}
.sm{font-size:13px}
p{margin:6px 0}
.dot{display:inline-block;width:9px;height:9px;border-radius:50%;margin-right:8px;background:var(--off)}
.dot.ok{background:var(--ok)}
.panel{border-top:1px dashed var(--line);margin-top:10px;padding-top:6px}
.slot{margin:8px 0}
.slot .lbl{font-size:13px;color:var(--mut);margin-bottom:2px}
.chk{display:block;margin:8px 0}
details{margin:10px 0}
summary{cursor:pointer;color:var(--mut)}
.scroll{overflow-x:auto}
.tbl{border-collapse:collapse;width:100%;font-size:13px}
.tbl th,.tbl td{padding:6px 8px;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}
.login{max-width:360px;margin:12vh auto 0}
.toast{position:fixed;left:50%;bottom:calc(20px + env(safe-area-inset-bottom));transform:translateX(-50%);background:#222;color:#fff;padding:9px 16px;border-radius:10px;max-width:90vw;z-index:9;font-size:14px}
.toast.bad{background:#b91c1c}
.grid{display:grid;grid-template-columns:repeat(2,1fr);gap:8px;margin:8px 0}
@media (min-width:560px){.grid{grid-template-columns:repeat(3,1fr)}}
.stat{background:var(--bg);border-radius:9px;padding:7px 9px;min-width:0}
.stat .k{font-size:12px;color:var(--mut)}
.stat .v{font-size:13px;word-break:break-word}
.stat .s{font-size:12px;color:var(--mut);word-break:break-word}
.bar{height:5px;border-radius:3px;background:var(--line);margin-top:5px;overflow:hidden}
.bar i{display:block;height:100%;background:var(--ok)}
.bar i.warn{background:#d97706}
.bar i.bad{background:var(--bad)}
.chip{display:inline-block;font-size:12px;border:1px solid var(--line);border-radius:999px;padding:1px 8px;margin:2px 4px 2px 0}
.exp{font-size:13px;margin-top:4px}
.exp.warn{color:var(--bad)}
.off{opacity:.65}
.sec{font-size:13px;font-weight:600;margin:16px 0 2px;padding-top:8px;border-top:1px dashed var(--line)}
[hidden]{display:none!important}
</style>
</head>
<body>
<div id="app"></div>
<div id="toast" class="toast" hidden></div>
<noscript>需要启用 JavaScript。</noscript>
<script>
(function () {
  'use strict';
  var app = document.getElementById('app');
  var toastEl = document.getElementById('toast');
  var token = sessionStorage.getItem('cfsm_t') || '';
  var S = { tab: 'nodes', servers: [], settings: {}, secret: '', ready: false, open: {} };
  var SLOTS = [['ct', '电信'], ['cu', '联通'], ['cm', '移动'], ['bd', 'BGP']];
  var ERR = {
    invalidCredentials: '用户名或密码错误', missingCredentials: '请输入用户名和密码',
    Unauthorized: '登录已过期，请重新登录', invalidServerId: '节点 ID 无效',
    '服务器名称无效': '名称需要 1 到 100 个字符',
    invalidReportInterval: '上报间隔只能是 30、60、120、180 秒',
    invalidCollectInterval: '采集间隔只能是 0、1、2、5、10 秒',
    invalidResetDay: '流量重置日需要是 0 到 31 的整数',
    invalidPingMode: 'Ping 模式只能是 tcp 或 icmp',
    invalidPingNodeFormat: '探测目标格式应为 host 或 host:port，不能含空格',
    invalidNotifyValue: '离线阈值只能是「关闭」或 2 到 30 分钟',
    invalidTimezone: '时区名称无效，例如 Asia/Shanghai',
    tgBotTokenRequired: '请先填写 Bark 地址或设备 Key',
    testNotificationFailed: '测试通知发送失败，请检查 Bark 地址或设备 Key',
    invalidPrice: '续费金额格式不对，例如 30 或 29.99',
    invalidBillingCycle: '付费周期无效', invalidCurrency: '币种无效',
    invalidExpireDate: '到期日格式不对，应为 年-月-日',
    invalidTrafficLimit: '流量上限格式不对，例如 500GB、1TB（不写单位按 GB）',
    invalidExpireDays: '到期提醒天数格式不对，例如 7,3,1,0（0 到 365，最多 6 个）',
    invalidExpireHour: '提醒时间只能是 0 到 23 点',
    invalidTrafficPercents: '流量提醒百分比格式不对，例如 90,95（1 到 100，最多 4 个）',
    noServersToImport: '没有可导入的节点', 'Server not found': '节点不存在', unknownAction: '不支持的操作'
  };

  /* ---------- 小工具 ---------- */
  function add(el, c) {
    if (c === null || c === undefined || c === false) return;
    if (Array.isArray(c)) { c.forEach(function (x) { add(el, x); }); return; }
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  function h(tag, props) {
    var el = document.createElement(tag);
    props = props || {};
    Object.keys(props).forEach(function (k) {
      var v = props[k];
      if (v === undefined || v === null || v === false) return;
      if (k === 'class') el.className = v;
      else if (k.slice(0, 2) === 'on') el.addEventListener(k.slice(2), v);
      else if (k === 'value' || k === 'checked' || k === 'selected' || k === 'disabled') el[k] = v;
      else el.setAttribute(k, v === true ? '' : String(v));
    });
    for (var i = 2; i < arguments.length; i++) add(el, arguments[i]);
    return el;
  }
  function sel(values, current, label) {
    var s = h('select', null, values.map(function (v) {
      return h('option', { value: String(v), selected: String(v) === String(current) }, label(v));
    }));
    s.value = String(current);
    return s;
  }
  function field(label, input, hint) {
    return h('label', { class: 'field' }, h('span', null, label), input, hint ? h('small', { class: 'mut' }, hint) : null);
  }
  function check(input, text) { return h('label', { class: 'chk' }, input, text); }
  var tt;
  function toast(msg, bad) {
    toastEl.textContent = msg;
    toastEl.className = 'toast' + (bad ? ' bad' : '');
    toastEl.hidden = false;
    clearTimeout(tt);
    tt = setTimeout(function () { toastEl.hidden = true; }, 3800);
  }
  function ago(ms) {
    if (!ms) return '从未上报';
    var s = Math.max(0, Math.round((Date.now() - ms) / 1000));
    if (s < 60) return s + ' 秒前';
    if (s < 3600) return Math.floor(s / 60) + ' 分钟前';
    if (s < 86400) return Math.floor(s / 3600) + ' 小时前';
    return Math.floor(s / 86400) + ' 天前';
  }
  function fmtPing(v) { return (v === false || v === undefined) ? '—' : v === null ? '超时' : (Math.round(v * 10) / 10) + ' ms'; }
  function fmtLoss(v) { return (v === false || v === undefined || v === null) ? '—' : v + '%'; }
  function gv(k) { return S.settings[k] || ''; }

  var CYCLES = [['month', '月付'], ['quarter', '季付'], ['half_year', '半年付'], ['year', '年付'], ['two_years', '两年付'], ['three_years', '三年付'], ['four_years', '四年付'], ['five_years', '五年付']];
  var CYCLE_MONTHS = { month: 1, quarter: 3, half_year: 6, year: 12, two_years: 24, three_years: 36, four_years: 48, five_years: 60 };
  var CALCS = [['total', '上下行合计'], ['up', '仅上行'], ['down', '仅下行'], ['max', '上下行取较大值']];
  function labelOf(list, k) { for (var i = 0; i < list.length; i++) if (list[i][0] === k) return list[i][1]; return k; }
  function fmtBytes(n) {
    var u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'], v = Number(n) || 0, i = 0;
    while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
    return (i === 0 ? v.toFixed(0) : v.toFixed(v >= 100 ? 0 : 2)) + ' ' + u[i];
  }
  function fmtMB(mb) { return fmtBytes((Number(mb) || 0) * 1048576); }
  function fmtUptime(bootMs) {
    if (!Number(bootMs)) return '—';
    var s = Math.max(0, Math.floor((Date.now() - Number(bootMs)) / 1000));
    var d = Math.floor(s / 86400), hh = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
    return d > 0 ? d + ' 天 ' + hh + ' 小时' : hh > 0 ? hh + ' 小时 ' + m + ' 分' : m + ' 分';
  }
  function pct(a, b) { return b > 0 ? Math.min(100, Math.round(a * 100 / b)) : 0; }
  function bar(p) { return h('div', { class: 'bar' }, h('i', { class: p >= 90 ? 'bad' : p >= 75 ? 'warn' : '', style: 'width:' + p + '%' })); }
  function stat(label, value, p, sub) {
    return h('div', { class: 'stat' }, h('div', { class: 'k' }, label), h('div', { class: 'v' }, value),
      p === undefined || p === null ? null : bar(p), sub ? h('div', { class: 's' }, sub) : null);
  }
  function pad2(n) { return String(n).padStart(2, '0'); }
  function todayStr() { var d = new Date(); return d.getFullYear() + '-' + pad2(d.getMonth() + 1) + '-' + pad2(d.getDate()); }
  function addMonths(ymd, n) {
    var p = ymd.split('-').map(Number), y = p[0], m = p[1] - 1 + n, d = p[2];
    y += Math.floor(m / 12);
    m = ((m % 12) + 12) % 12;
    var dim = new Date(Date.UTC(y, m + 1, 0)).getUTCDate();
    return y + '-' + pad2(m + 1) + '-' + pad2(Math.min(d, dim));
  }
  function priceLine(s) {
    if (s.price === '' || s.price === undefined) return '';
    if (Number(s.price) <= 0) return '免费';
    return s.currency + s.price + ' / ' + labelOf(CYCLES, s.billing_cycle);
  }

  async function api(body) {
    var headers = { 'content-type': 'application/json' };
    if (token) headers.authorization = 'Bearer ' + token;
    var r = await fetch('/admin/api', { method: 'POST', headers: headers, body: JSON.stringify(body) });
    var j = {};
    try { j = await r.json(); } catch (e) { j = {}; }
    if (r.status === 401 && body.action !== 'login') { logout(); throw new Error(ERR.Unauthorized); }
    if (!r.ok) throw new Error(ERR[j.error] || j.error || ('请求失败（' + r.status + '）'));
    return j;
  }
  async function load() {
    var r = await Promise.all([api({ action: 'list' }), api({ action: 'get_settings' })]);
    S.servers = r[0].servers;
    S.settings = r[1].settings;
    S.secret = r[1].api_secret;
    S.ready = !!r[1].agent_install_ready;
    render();
  }
  function logout() {
    token = '';
    sessionStorage.removeItem('cfsm_t');
    S.servers = [];
    render();
  }
  async function saveSettings(obj, okMsg) {
    try { await api({ action: 'save_settings', settings: obj }); await load(); toast(okMsg || '已保存'); }
    catch (e) { toast(e.message, true); }
  }

  /* ---------- 登录 ---------- */
  function viewLogin() {
    var u = h('input', { type: 'text', autocomplete: 'username', value: 'admin' });
    var p = h('input', { type: 'password', autocomplete: 'current-password' });
    var btn = h('button', { class: 'btn pri', type: 'button' }, '登录');
    async function go() {
      btn.disabled = true;
      try {
        var j = await api({ action: 'login', username: u.value.trim(), password: p.value });
        token = j.token;
        sessionStorage.setItem('cfsm_t', token);
        toastEl.hidden = true; // 登录成功后收起之前的错误提示
        await load();
      } catch (e) { toast(e.message, true); btn.disabled = false; }
    }
    btn.addEventListener('click', go);
    p.addEventListener('keydown', function (e) { if (e.key === 'Enter') go(); });
    return h('div', { class: 'login card' }, h('h1', null, 'CFSM Lite'), h('p', { class: 'mut' }, '管理后台'),
      field('用户名', u), field('密码', p), btn);
  }

  /* ---------- 节点 ---------- */
  function installCmd(s) {
    return 'curl -fsSL ' + location.origin + '/install.sh | sh -s -- -i ' + s.id + ' -s ' + S.secret + ' -u ' + location.origin + '/update';
  }
  function installPanel(s) {
    var ta = h('textarea', { readonly: true, rows: '5', class: 'mono' }, installCmd(s));
    var copy = h('button', { class: 'btn pri sm', type: 'button', onclick: async function () {
      try { await navigator.clipboard.writeText(ta.value); toast('已复制'); }
      catch (e) { ta.select(); toast('已选中，请手动复制', true); }
    } }, '复制命令');
    return h('div', { class: 'panel' },
      S.ready ? null : h('p', { class: 'sm', style: 'color:var(--bad)' }, '还不能用：请先在 Cloudflare 控制台给这个 Worker 添加变量 AGENT_REPO（格式：GitHub用户名/仓库名），再刷新本页。'),
      h('p', { class: 'mut sm' }, '在要监控的服务器上以 root 执行这条命令（非 root 用户在 sh 前加 sudo）。命令里含有上报密钥，请不要发给别人。'), ta, h('div', { class: 'row' }, copy));
  }
  function editPanel(s) {
    var f = {};
    f.name = h('input', { type: 'text', value: s.name, maxlength: '100' });
    f.group = h('input', { type: 'text', value: s.server_group, maxlength: '64' });
    // 续费与到期
    f.price = h('input', { type: 'text', value: s.price, placeholder: '例如 30，免费填 0' });
    f.currency = sel(['¥', '$', '€', '£', '₽', '₣', '₹', '₫', '฿'], s.currency, function (v) { return v; });
    f.cycle = sel(CYCLES.map(function (c) { return c[0]; }), s.billing_cycle, function (v) { return labelOf(CYCLES, v); });
    f.expire = h('input', { type: 'date', value: s.expire_date });
    f.auto = h('input', { type: 'checkbox', checked: s.auto_renewal === '1' });
    var renew = h('button', { class: 'btn sm', type: 'button', onclick: function () {
      var base = f.expire.value && f.expire.value >= todayStr() ? f.expire.value : todayStr();
      f.expire.value = addMonths(base, CYCLE_MONTHS[f.cycle.value] || 1);
      toast('到期日已顺延到 ' + f.expire.value + '，点「保存」后生效');
    } }, '续费一期');
    // 流量
    f.limit = h('input', { type: 'text', value: s.traffic_limit, placeholder: '例如 500GB、1TB，留空表示不限' });
    f.calc = sel(CALCS.map(function (c) { return c[0]; }), s.traffic_calc_type, function (v) { return labelOf(CALCS, v); });
    f.reset = h('input', { type: 'number', min: '0', max: '31', value: String(s.reset_day) });
    f.iface = h('input', { type: 'text', value: s.interface, placeholder: '留空自动汇总，多个网卡用英文逗号分隔' });
    // 采集与通知
    f.report = sel([30, 60, 120, 180], s.report_interval, function (v) { return v + ' 秒'; });
    f.collect = sel([0, 1, 2, 5, 10], s.collect_interval, function (v) { return v === 0 ? '不额外采样' : v + ' 秒'; });
    f.hidden = h('input', { type: 'checkbox', checked: s.is_hidden === '1' });
    f.mute = h('input', { type: 'checkbox', checked: s.offline_notify_disabled === '1' });
    SLOTS.forEach(function (p) {
      f['p_' + p[0]] = h('input', { type: 'text', value: s['custom_' + p[0]] || '', placeholder: '留空使用全局：' + (gv('custom_' + p[0]) || '内置默认') });
    });
    f.tags = h('input', { type: 'text', value: s.tags, placeholder: '英文逗号分隔' });
    f.note = h('textarea', { rows: '2', maxlength: '500' }, s.note || '');
    var save = h('button', { class: 'btn pri', type: 'button', onclick: async function () {
      var body = {
        action: 'edit', id: s.id, name: f.name.value.trim(), server_group: f.group.value.trim() || 'Default',
        price: f.price.value.trim(), currency: f.currency.value, billing_cycle: f.cycle.value, expire_date: f.expire.value,
        auto_renewal: f.auto.checked ? '1' : '0',
        traffic_limit: f.limit.value.trim(), traffic_calc_type: f.calc.value, reset_day: Number(f.reset.value || 0), interface: f.iface.value.trim(),
        report_interval: Number(f.report.value), collect_interval: Number(f.collect.value),
        is_hidden: f.hidden.checked ? '1' : '0', offline_notify_disabled: f.mute.checked ? '1' : '0',
        tags: f.tags.value.trim(), note: f.note.value.trim()
      };
      SLOTS.forEach(function (p) { body['custom_' + p[0]] = f['p_' + p[0]].value.trim(); });
      try { await api(body); S.open[s.id] = null; await load(); toast('已保存，Agent 会在下次上报时自动应用'); }
      catch (e) { toast(e.message, true); }
    } }, '保存');
    return h('div', { class: 'panel' },
      field('名称', f.name), field('分组', f.group),
      h('div', { class: 'sec' }, '续费与到期'),
      field('续费金额', f.price, '每个付费周期的金额'), field('币种', f.currency), field('付费周期', f.cycle),
      field('到期日', f.expire, '到期前会按「通知」页设置的天数发 Bark 提醒'),
      h('div', { class: 'row' }, renew),
      check(f.auto, '已设置自动续费（仅作标记，提醒里会注明）'),
      h('div', { class: 'sec' }, '流量'),
      field('每月流量上限', f.limit, '不写单位按 GB 算（按 1024 换算）；用到「通知」页设置的百分比时发 Bark 提醒'),
      field('流量计算方式', f.calc, '按厂商的计费方式选择'),
      field('每月流量重置日', f.reset, '1 到 31；0 表示不重置；大于当月天数时取当月最后一天'),
      field('统计网卡', f.iface),
      h('div', { class: 'sec' }, '采集与通知'),
      field('上报间隔', f.report, '越短越实时，请求量也越大'), field('本机采集间隔', f.collect),
      check(f.hidden, '在 Monito 等客户端中隐藏此节点'),
      check(f.mute, '此节点离线或恢复时不发送通知'),
      h('details', null, h('summary', null, '此节点专用的 Ping 目标（可选）'),
        SLOTS.map(function (p) { return field(gv('custom_' + p[0] + '_name') || p[1], f['p_' + p[0]]); })),
      h('details', null, h('summary', null, '标签与备注（可选）'), field('标签', f.tags), field('备注', f.note)),
      h('div', { class: 'row' }, save));
  }
  function toggle(id, m) { S.open[id] = S.open[id] === m ? null : m; render(); }
  async function del(s) {
    if (!confirm('确定删除「' + s.name + '」？该节点上的 Agent 将无法继续上报。')) return;
    try { await api({ action: 'delete', id: s.id }); await load(); toast('已删除'); }
    catch (e) { toast(e.message, true); }
  }
  function nodeCard(s) {
    var online = s.is_online;
    var meta = [s.server_group];
    if (s.region) meta.push(s.region);
    if (s.os) meta.push(s.os + (s.arch ? ' ' + s.arch : ''));
    meta.push(online ? '已在线 ' + fmtUptime(s.boot_time) : '离线 · ' + ago(s.last_updated));
    if (s.agent_version) meta.push(s.agent_version);
    if (s.is_hidden === '1') meta.push('已隐藏');
    var ramP = pct(s.ram_used, s.ram_total), diskP = pct(s.disk_used, s.disk_total);
    var limited = s.lite_traffic_limit_bytes > 0;
    var tp = limited ? Math.min(100, Math.round(s.lite_traffic_percent)) : null;
    var trafSub = limited ? '已用 ' + fmtBytes(s.lite_traffic_used_bytes) + ' / ' + fmtBytes(s.lite_traffic_limit_bytes) + '（' + s.lite_traffic_percent + '%，' + labelOf(CALCS, s.traffic_calc_type) + '）' : '未设置流量上限';
    var grid = h('div', { class: 'grid' },
      stat('CPU', (Math.round(s.cpu * 10) / 10) + '%', Math.min(100, Math.round(s.cpu)), '负载 ' + s.load_avg),
      stat('内存', fmtMB(s.ram_used) + ' / ' + fmtMB(s.ram_total), ramP, ramP + '%'),
      stat('磁盘', fmtMB(s.disk_used) + ' / ' + fmtMB(s.disk_total), diskP, diskP + '%'),
      stat('网速', '↓ ' + fmtBytes(s.net_in_speed) + '/s', null, '↑ ' + fmtBytes(s.net_out_speed) + '/s'),
      stat('当月流量', '↓ ' + fmtBytes(s.net_rx_monthly) + '  ↑ ' + fmtBytes(s.net_tx_monthly), tp, trafSub),
      stat('累计流量', '↓ ' + fmtBytes(s.net_rx), null, '↑ ' + fmtBytes(s.net_tx)));
    var chips = h('div', null, SLOTS.map(function (p) {
      return h('span', { class: 'chip' }, (gv('custom_' + p[0] + '_name') || p[1]) + ' ' + fmtPing(s['ping_' + p[0]]) + ' / ' + fmtLoss(s['loss_' + p[0]]));
    }));
    var exp = [];
    var dl = s.lite_expire_days_left;
    if (s.expire_date) exp.push('到期 ' + s.expire_date + (dl === null ? '' : dl >= 0 ? '（剩 ' + dl + ' 天）' : '（已过期 ' + (-dl) + ' 天）'));
    if (priceLine(s)) exp.push('续费 ' + priceLine(s));
    var card = h('div', { class: 'card' + (online ? '' : ' off') },
      h('div', { class: 'row between' },
        h('div', null, h('span', { class: 'dot' + (online ? ' ok' : '') }), h('b', null, s.name)),
        h('div', { class: 'row gap' },
          h('button', { class: 'btn sm', type: 'button', onclick: function () { toggle(s.id, 'install'); } }, '安装命令'),
          h('button', { class: 'btn sm', type: 'button', onclick: function () { toggle(s.id, 'edit'); } }, '编辑'),
          h('button', { class: 'btn sm bad', type: 'button', onclick: function () { del(s); } }, '删除'))),
      h('p', { class: 'mut sm' }, meta.join(' · ')),
      grid, chips,
      exp.length ? h('div', { class: 'exp' + (dl !== null && dl <= 7 ? ' warn' : '') }, exp.join(' · ')) : null);
    if (S.open[s.id] === 'install') card.append(installPanel(s));
    if (S.open[s.id] === 'edit') card.append(editPanel(s));
    return card;
  }
  function viewNodes() {
    var name = h('input', { type: 'text', placeholder: '新节点名称', maxlength: '100' });
    var addBtn = h('button', { class: 'btn pri', type: 'button', onclick: async function () {
      var n = name.value.trim();
      if (!n) { toast('请输入节点名称', true); return; }
      try { var j = await api({ action: 'add', name: n }); S.open[j.id] = 'install'; await load(); toast('已添加，请复制下方的安装命令'); }
      catch (e) { toast(e.message, true); }
    } }, '添加节点');
    var online = S.servers.filter(function (x) { return x.is_online; }).length;
    return h('div', null,
      h('div', { class: 'card' }, h('div', { class: 'row' }, name, addBtn),
        h('p', { class: 'mut sm' }, '共 ' + S.servers.length + ' 台，在线 ' + online + ' 台')),
      S.servers.length ? S.servers.map(nodeCard) : h('p', { class: 'mut' }, '还没有节点：先添加一个，再把安装命令粘贴到服务器上执行。'));
  }

  /* ---------- Ping 与丢包 ---------- */
  function viewPing() {
    var f = {};
    f.mode = sel(['tcp', 'icmp'], gv('ping_mode') || 'tcp', function (v) { return v === 'tcp' ? 'TCP（推荐，无需特殊权限）' : 'ICMP（仅原版 Agent 支持，Lite Agent 会自动改用 TCP）'; });
    var rows = SLOTS.map(function (p) {
      f['n_' + p[0]] = h('input', { type: 'text', value: gv('custom_' + p[0] + '_name') || p[1], maxlength: '16' });
      f['t_' + p[0]] = h('input', { type: 'text', value: gv('custom_' + p[0]), placeholder: 'host 或 host:port，留空使用内置默认' });
      return h('div', { class: 'slot' }, h('div', { class: 'lbl' }, '名称与探测目标'), h('div', { class: 'row' }, f['n_' + p[0]], f['t_' + p[0]]));
    });
    var save = h('button', { class: 'btn pri', type: 'button', onclick: function () {
      var o = { ping_mode: f.mode.value };
      SLOTS.forEach(function (p) {
        o['custom_' + p[0] + '_name'] = f['n_' + p[0]].value.trim() || p[1];
        o['custom_' + p[0]] = f['t_' + p[0]].value.trim();
      });
      saveSettings(o, '已保存，Agent 会在下次上报（约 1 分钟内）自动应用');
    } }, '保存');
    var tbl = h('table', { class: 'tbl' },
      h('thead', null, h('tr', null, h('th', null, '节点'), SLOTS.map(function (p) { return h('th', null, gv('custom_' + p[0] + '_name') || p[1]); }))),
      h('tbody', null, S.servers.map(function (s) {
        return h('tr', null, h('td', null, s.name), SLOTS.map(function (p) {
          return h('td', null, s.is_online ? fmtPing(s['ping_' + p[0]]) + ' / ' + fmtLoss(s['loss_' + p[0]]) : '离线');
        }));
      })));
    return h('div', null,
      h('div', { class: 'card' },
        h('p', { class: 'mut sm' }, '延迟和丢包都由每台服务器上的 Agent 对下面 4 个目标发探测得出，丢包率与延迟来自同一轮探测，没有单独的丢包开关。这里的设置对所有节点生效，个别节点要用不同目标，可在「节点 → 编辑」里单独设置。'),
        field('探测方式', f.mode), rows, h('div', { class: 'row' }, save)),
      h('div', { class: 'card' }, h('b', null, '最新读数（延迟 / 丢包）'),
        h('p', { class: 'mut sm' }, '「—」表示未配置或未取样，「超时」表示这一轮全部探测失败。'),
        h('div', { class: 'scroll' }, tbl)));
  }

  /* ---------- 通知 ---------- */
  function viewNotify() {
    var st = S.settings, f = {};
    f.bark = h('input', { type: 'password', autocomplete: 'off',
      placeholder: st.tg_bot_token_configured ? '已配置（留空则保持不变）' : 'https://api.day.app/你的设备Key，或直接填设备 Key' });
    var opts = ['0'];
    for (var i = 2; i <= 30; i++) opts.push(String(i));
    f.notify = sel(opts, st.tg_notify || '3', function (v) { return v === '0' ? '关闭全部通知' : '离线超过 ' + v + ' 分钟后通知'; });
    f.tz = h('input', { type: 'text', value: st.notification_timezone || 'Asia/Shanghai' });
    var hours = [];
    for (var j = 0; j < 24; j++) hours.push(String(j));
    f.expDays = h('input', { type: 'text', value: st.expire_notify_days === undefined ? '7,3,1,0' : st.expire_notify_days, placeholder: '例如 7,3,1,0，留空表示不提醒' });
    f.expHour = sel(hours, st.expire_notify_hour || '9', function (v) { return v + ' 点之后'; });
    f.trafPct = h('input', { type: 'text', value: st.traffic_notify_percents === undefined ? '90,95' : st.traffic_notify_percents, placeholder: '例如 90,95，留空表示不提醒' });
    var save = h('button', { class: 'btn pri', type: 'button', onclick: function () {
      var o = {
        tg_notify: f.notify.value, notification_timezone: f.tz.value.trim(),
        expire_notify_days: f.expDays.value.trim(), expire_notify_hour: f.expHour.value, traffic_notify_percents: f.trafPct.value.trim()
      };
      if (f.bark.value.trim()) o.tg_bot_token = f.bark.value.trim();
      saveSettings(o, '通知设置已保存');
    } }, '保存');
    var test = h('button', { class: 'btn', type: 'button', onclick: async function () {
      try { await api({ action: 'send_test_notification', tg_bot_token: f.bark.value.trim() }); toast('测试通知已发送，请查看手机'); }
      catch (e) { toast(e.message, true); }
    } }, '发送测试通知');
    return h('div', { class: 'card' },
      h('p', { class: 'mut sm' }, '节点离线、恢复、即将到期、流量用量过高时通过 Bark 推送。Bark 地址只能写入，不会再显示出来。'),
      field('Bark 地址或设备 Key', f.bark, st.tg_bot_token_configured ? '当前状态：已配置' : '当前状态：未配置'),
      field('离线通知', f.notify, '离线判断和通知使用同一个时间，也是客户端显示在线/离线的依据；选「关闭全部通知」会同时关闭下面的到期和流量提醒'),
      field('通知时间的时区', f.tz),
      h('div', { class: 'sec' }, '到期提醒'),
      field('到期前几天提醒', f.expDays, '用英文逗号分隔，0 表示到期当天；每个节点每个到期日在每个档位只提醒一次。续费后在节点里更新到期日即可重新开始计算'),
      field('每天几点之后发送', f.expHour, '按上面的时区；到了这个时间之后的第一次检查就会发送'),
      h('div', { class: 'sec' }, '流量提醒'),
      field('当月流量用到百分之几时提醒', f.trafPct, '例如 90,95，每个统计周期每个档位只提醒一次；流量上限和重置日在每个节点的编辑里设置'),
      h('div', { class: 'row' }, save, test));
  }

  /* ---------- 系统 ---------- */
  function viewSystem() {
    var st = S.settings;
    var title = h('input', { type: 'text', value: st.site_title || 'CFSM Lite', maxlength: '64' });
    var out = h('textarea', { rows: '6', class: 'mono', placeholder: '导出的节点配置会显示在这里；要导入时把导出的内容粘贴到这里' });
    var exp = h('button', { class: 'btn', type: 'button', onclick: async function () {
      try { var j = await api({ action: 'export_servers' }); out.value = JSON.stringify(j.servers, null, 2); toast('已导出，请复制保存'); }
      catch (e) { toast(e.message, true); }
    } }, '导出节点配置');
    var imp = h('button', { class: 'btn', type: 'button', onclick: async function () {
      var list;
      try { list = JSON.parse(out.value); if (list && !Array.isArray(list)) list = list.servers; } catch (e) { toast('内容不是有效的 JSON', true); return; }
      try { var j = await api({ action: 'import_servers', servers: list }); await load(); toast('导入 ' + j.imported + ' 个，跳过 ' + j.skipped + ' 个'); }
      catch (e) { toast(e.message, true); }
    } }, '导入节点配置');
    var info = [
      ['上报地址', location.origin + '/update'],
      ['数据读取权限', st.is_public === 'true' ? '公开：任何人都能读取监控数据' : '私有：需要管理员登录令牌'],
      ['管理员用户名', st.username || 'admin']
    ];
    return h('div', null,
      h('div', { class: 'card' }, field('站点标题', title),
        h('div', { class: 'row' }, h('button', { class: 'btn pri', type: 'button', onclick: function () { saveSettings({ site_title: title.value.trim() || 'CFSM Lite' }); } }, '保存'))),
      h('div', { class: 'card' }, h('b', null, '当前配置'),
        info.map(function (r) { return h('p', { class: 'sm' }, h('span', { class: 'mut' }, r[0] + '：'), r[1]); }),
        h('p', { class: 'mut sm' }, '管理员密码、上报密钥（API_SECRET）和访问模式不能在这里修改，请到 Cloudflare 控制台里该 Worker 的「变量和机密」中修改。')),
      h('div', { class: 'card' }, h('b', null, '备份与迁移'),
        h('p', { class: 'mut sm' }, '只包含节点配置，不含监控数据。节点 ID 不变，导入后 Agent 不用重装。'),
        out, h('div', { class: 'row' }, exp, imp)));
  }

  /* ---------- 外壳 ---------- */
  function render() {
    if (!token) { app.replaceChildren(viewLogin()); return; }
    var tabs = [['nodes', '节点'], ['ping', 'Ping'], ['notify', '通知'], ['system', '系统']];
    var nav = h('nav', { class: 'tabs' }, tabs.map(function (t) {
      return h('button', { class: 'tab' + (S.tab === t[0] ? ' on' : ''), type: 'button', onclick: function () { S.tab = t[0]; render(); } }, t[1]);
    }));
    var body = { nodes: viewNodes, ping: viewPing, notify: viewNotify, system: viewSystem }[S.tab]();
    app.replaceChildren(
      h('header', { class: 'top' }, h('b', null, (S.settings.site_title || 'CFSM Lite') + ' 管理'),
        h('button', { class: 'btn sm', type: 'button', onclick: logout }, '退出')),
      nav, body);
  }

  if (token) { load().catch(function (e) { toast(e.message, true); render(); }); } else { render(); }
})();
</script>
</body>
</html>
`;
function adminPage() {
  return new Response(ADMIN_HTML, {
    headers: {
      'content-type': 'text/html; charset=utf-8',
      'cache-control': 'no-store',
      'content-security-policy': "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
      'x-frame-options': 'DENY',
      'x-content-type-options': 'nosniff',
      'referrer-policy': 'no-referrer',
    },
  });
}

export default {
  async fetch(request, env, ctx) {
    if (!env.API_SECRET) return fail('API_SECRET is required', 400);
    const url = new URL(request.url);
    const path = url.pathname.replace(/\/+$/, '') || '/';
    try {
      if (request.method === 'OPTIONS') return new Response(null, { status: 204 });
      if (path === '/update') {
        if (request.method === 'POST') return await handleUpdate(request, env, ctx);
        // Lite 不提供 WebSocket；Agent 安装时指定 -connection_mode=http，不会走到这里
        return fail('WebSocket not enabled', 503);
      }
      if (path === '/api/ws' && request.method === 'GET') return await handleWs(request, env, url);
      if (path === '/admin/api') return await handleAdmin(request, env);
      if (path === '/admin' && request.method === 'GET') return adminPage();
      if (path === '/install.sh' && request.method === 'GET') return installScript(env);
      if (path.startsWith('/api/') && request.method === 'GET') return await handlePublic(request, env, url, path);
      if (path === '/') return new Response('CFSM Lite is running.', { headers: { 'content-type': 'text/plain' } });
      return fail('Not Found', 404);
    } catch (e) {
      console.error('unhandled error:', e?.message || e);
      return fail('Internal Server Error', 500);
    }
  },
  async scheduled(_event, env, ctx) {
    ctx.waitUntil(runCron(env));
  },
};
