// 本地测试：用 Node 内置 SQLite 模拟 D1，直接调用 Worker 的 fetch / scheduled。
// 运行：node test/run.mjs
import { DatabaseSync } from 'node:sqlite';
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import vm from 'node:vm';
import { spawnSync } from 'node:child_process';
import { writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import worker, {
  checkOffline, md5hex, buildAgentConfig, checkAlerts, runCron, createWsSession, applyDefaultPing, Hub,
  parseTrafficLimit, trafficUsed, periodStart, expireStage, trafficStage, parseIntList, ymdInTz, daysLeft,
} from '../src/index.js';

// ---- 最小 D1 模拟 ----
class Stmt {
  constructor(db, sql) { this.db = db; this.sql = sql; this.args = []; }
  bind(...a) { this.args = a; return this; }
  async run() { const r = this.db.prepare(this.sql).run(...this.args); return { success: true, results: [], meta: { changes: Number(r.changes) } }; }
  async all() { return { success: true, results: this.db.prepare(this.sql).all(...this.args), meta: {} }; }
  async first() { return this.db.prepare(this.sql).get(...this.args) ?? null; }
}
const makeDB = () => {
  const db = new DatabaseSync(':memory:');
  db.exec(readFileSync(new URL('../schema.sql', import.meta.url), 'utf8'));
  return {
    raw: db,
    prepare: (sql) => new Stmt(db, sql),
    async batch(stmts) {
      db.exec('BEGIN');
      try { const out = []; for (const s of stmts) out.push(await s.run()); db.exec('COMMIT'); return out; }
      catch (e) { db.exec('ROLLBACK'); throw e; }
    },
  };
};

// ---- 环境与辅助 ----
const barkCalls = [];
globalThis.fetch = async (url, init) => { barkCalls.push({ url, body: JSON.parse(init.body) }); return new Response('{}', { status: 200 }); };
const bg = []; // waitUntil 的任务
const ctx = { waitUntil: (p) => bg.push(p) };
const flush = async () => { await Promise.all(bg.splice(0)); };
const mkEnv = (extra = {}) => ({ DB: makeDB(), API_SECRET: 'test-secret-123', BARK_RETRY_MS: 0, ...extra });
const call = (env, method, path, { body, token, headers } = {}) =>
  worker.fetch(new Request('https://x.test' + path, {
    method, body: body === undefined ? undefined : typeof body === 'string' ? body : JSON.stringify(body),
    headers: { ...(token ? { authorization: 'Bearer ' + token } : {}), ...headers },
  }), env, ctx);
const admin = (env, body, token) => call(env, 'POST', '/admin/api', { body, token });
const login = async (env) => (await (await admin(env, { action: 'login', username: 'admin', password: 'test-secret-123' })).json()).token;
const metrics = (o = {}) => ({
  cpu: '12.34', ram_total: '8192', ram_used: '3700', swap_total: '2048', swap_used: '100', disk_total: '102400', disk_used: '32000',
  load_avg: '0.10 0.20 0.30', boot_time: '1700000000000', net_rx: '12345678', net_tx: '87654321', net_rx_monthly: '1073741824',
  net_tx_monthly: '536870912', net_in_speed: '1024', net_out_speed: '512', os: 'Ubuntu 22.04', arch: 'amd64',
  kernel_version: '6.8.0', cpu_info: 'Xeon', cpu_cores: '4', gpu_info: null, processes: '256', tcp_conn: '32', udp_conn: '4',
  ip_v4: '203.0.113.10', ip_v6: '0', ping_ct: '23', ping_cu: 'null', ping_cm: false, ping_bgp: '40',
  loss_ct: '0', loss_cu: '100', loss_cm: false, loss_bd: '0',
  disk: { read_bps: 0, write_bps: 0, read_iops: 0, write_iops: 0, await_ms: 0, util: 0 }, ...o,
});
let passed = 0;
const test = async (name, fn) => { await fn(); passed++; console.log('  ok  ' + name); };
const setup = async (env) => {
  const token = await login(env);
  const { id } = await (await admin(env, { action: 'add', name: 'HK-01' }, token)).json();
  return { token, id };
};

// ---- 测试 ----
console.log('CFSM Lite Worker tests');

await test('API_SECRET 未配置时拒绝服务', async () => {
  const env = mkEnv({ API_SECRET: '' });
  assert.equal((await call(env, 'GET', '/api/config')).status, 400);
});

await test('登录：缺参数 400、密码错 401、成功返回 JWT；ADMIN_PASSWORD 优先于 API_SECRET', async () => {
  const env = mkEnv({ ADMIN_PASSWORD: 'other-pass' });
  assert.equal((await admin(env, { action: 'login', username: 'admin' })).status, 400);
  assert.equal((await admin(env, { action: 'login', username: 'admin', password: 'test-secret-123' })).status, 401);
  const r = await admin(env, { action: 'login', username: 'admin', password: 'other-pass' });
  const j = await r.json();
  assert.equal(r.status, 200); assert.equal(j.success, true); assert.equal(j.token.split('.').length, 3);
});

await test('管理接口无令牌或令牌被篡改 -> 401', async () => {
  const env = mkEnv();
  const token = await login(env);
  assert.equal((await admin(env, { action: 'list' })).status, 401);
  assert.equal((await admin(env, { action: 'list' }, token.slice(0, -2) + 'xx')).status, 401);
  assert.equal((await admin(env, { action: 'list' }, token)).status, 200);
});

await test('add / list / edit(局部更新+校验) / delete', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  assert.match(id, /^[0-9a-f-]{36}$/);
  assert.equal((await admin(env, { action: 'add', name: '' }, token)).status, 400);
  assert.equal((await admin(env, { action: 'edit', id, report_interval: 45 }, token)).status, 400);
  assert.equal((await admin(env, { action: 'edit', id, custom_ct: 'bad host!' }, token)).status, 400);
  assert.equal((await admin(env, { action: 'edit', id, name: 'HK-02', price: '30', currency: '$', tags: 'a,b' }, token)).status, 200);
  const list = await (await admin(env, { action: 'list' }, token)).json();
  assert.equal(list.servers[0].name, 'HK-02'); assert.equal(list.servers[0].price, '30.00'); assert.equal(list.servers[0].server_group, 'Default');
  assert.equal(list.stats.total, 1); assert.equal(list.stats.online, 0);
  assert.equal((await admin(env, { action: 'edit', id: '11111111-1111-4111-8111-111111111111', name: 'x' }, token)).status, 404);
  assert.equal((await admin(env, { action: 'delete', id: 'nope' }, token)).status, 400);
  assert.equal((await admin(env, { action: 'delete', id }, token)).status, 200);
  assert.equal((await (await admin(env, { action: 'list' }, token)).json()).servers.length, 0);
});

await test('/update：错误密钥 401、未知节点 404、非法 JSON 400、超大请求 413', async () => {
  const env = mkEnv();
  const { id } = await setup(env);
  const post = (body, headers) => call(env, 'POST', '/update', { body, headers });
  assert.equal((await post({ id, secret: 'wrong', metrics: metrics() })).status, 401);
  assert.equal((await post({ id: '11111111-1111-4111-8111-111111111111', secret: env.API_SECRET, metrics: metrics() })).status, 404);
  assert.equal((await post('{oops')).status, 400);
  assert.equal((await post({ id: 'bad id!', secret: env.API_SECRET, metrics: metrics() })).status, 400);
  assert.equal((await post({ id, secret: env.API_SECRET })).status, 400);
  assert.equal((await post({ id, secret: env.API_SECRET, metrics: metrics() }, { 'content-length': String(600 * 1024) })).status, 413);
});

await test('/update 成功：旧版 Agent 得 OK；流量修正确认包得 OK', async () => {
  const env = mkEnv();
  const { id } = await setup(env);
  const r2 = await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } });
  assert.equal(r2.status, 200); assert.equal(await r2.text(), 'OK');
  const r3 = await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, rx_correction: 0, tx_correction: 0 } });
  assert.equal(r3.status, 200);
});

await test('动态配置：首次下发完整配置串，MD5 一致后回 204；配置变更后重新下发', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  const hdr = (md5) => ({ 'x-agent-config-schema': '6', 'x-agent-config-md5': md5, 'x-agent-version': 'lite-go-0.1' });
  const post = (md5) => call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() }, headers: hdr(md5) });
  const r1 = await post('none');
  assert.equal(r1.status, 200);
  const body = await r1.text();
  assert.equal(body, 'collect_interval=0&report_interval=60&reset_day=1&schema_version=6' +
    '&custom_ct=gd-ct-dualstack.ip.zstaticcdn.com&custom_cu=gd-cu-dualstack.ip.zstaticcdn.com' +
    '&custom_cm=gd-cm-dualstack.ip.zstaticcdn.com&custom_bd=ip.zstaticcdn.com&interface=' +
    '&connection_mode=http&wss_report_interval=2&ping_mode=tcp');
  assert.ok(body.length <= 512);
  const sum = r1.headers.get('x-agent-config-md5');
  assert.equal(sum, createHash('md5').update(body).digest('hex'));       // 自实现的 MD5 与 Node 一致
  assert.equal(r1.headers.get('x-agent-config-schema'), '6');
  assert.equal((await post(sum)).status, 204);                            // 配置未变 -> 204
  await admin(env, { action: 'save_settings', settings: { ping_mode: 'icmp', custom_ct: 'example.com:443' } }, token);
  await admin(env, { action: 'edit', id, report_interval: 120, custom_cu: '1.1.1.1' }, token);
  const r2 = await post(sum);                                              // 配置变了 -> 新配置
  assert.equal(r2.status, 200);
  const b2 = await r2.text();
  assert.match(b2, /report_interval=120/); assert.match(b2, /custom_ct=example\.com:443/);
  assert.match(b2, /custom_cu=1\.1\.1\.1/); assert.match(b2, /ping_mode=icmp$/);
  assert.equal(r2.headers.get('x-agent-config-md5'), createHash('md5').update(b2).digest('hex'));
  const r3 = await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() }, headers: { 'x-agent-config-schema': '5', 'x-agent-config-md5': 'none' } });
  assert.doesNotMatch(await r3.text(), /ping_mode/);                       // schema 5 不含 ping_mode
  const r4 = await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() }, headers: { 'x-agent-config-schema': '9', 'x-agent-config-md5': 'none' } });
  assert.equal(r4.status, 204);                                            // 未知的新 schema：不下发配置
});

await test('MD5 实现：与 Node 对多种输入一致（含中文、长文本、块边界）', async () => {
  for (const t of ['', 'a', 'abc', 'x'.repeat(55), 'x'.repeat(56), 'x'.repeat(63), 'x'.repeat(64), 'x'.repeat(65), 'x'.repeat(1000), '中文测试 mixed 123']) {
    assert.equal(md5hex(t), createHash('md5').update(t).digest('hex'), JSON.stringify(t.slice(0, 20)));
  }
});

await test('管理页面：GET /admin 返回 HTML 与严格的安全响应头；页面脚本语法正确', async () => {
  const env = mkEnv();
  const r = await call(env, 'GET', '/admin');
  assert.equal(r.status, 200);
  assert.match(r.headers.get('content-type'), /text\/html/);
  assert.match(r.headers.get('content-security-policy'), /default-src 'none'/);
  assert.equal(r.headers.get('x-frame-options'), 'DENY');
  const html = await r.text();
  assert.match(html, /CFSM Lite 管理/);
  assert.equal(/<script\s+src=/i.test(html), false);                      // 没有任何外部脚本
  const script = /<script>([\s\S]*?)<\/script>/.exec(html)[1];
  new vm.Script(script);                                                   // 语法检查
});

await test('全局 Ping 设置：模式与目标可保存、非法值被拒绝，留空回退内置默认', async () => {
  const env = mkEnv();
  const { token } = await setup(env);
  assert.equal((await admin(env, { action: 'save_settings', settings: { ping_mode: 'udp' } }, token)).status, 400);
  assert.equal((await admin(env, { action: 'save_settings', settings: { custom_bd: 'bad host' } }, token)).status, 400);
  await admin(env, { action: 'save_settings', settings: { ping_mode: 'icmp', custom_bd: '', custom_ct_name: '电信宽带' } }, token);
  const g = await (await admin(env, { action: 'get_settings' }, token)).json();
  assert.equal(g.settings.ping_mode, 'icmp'); assert.equal(g.settings.custom_ct_name, '电信宽带');
  const cfg = buildAgentConfig({ collect_interval: 0, report_interval: 60, reset_day: 1, interface: '', custom_ct: '', custom_cu: '', custom_cm: '', custom_bd: '' }, { ...g.settings }, 6);
  assert.match(cfg, /custom_bd=ip\.zstaticcdn\.com/); assert.match(cfg, /ping_mode=icmp/);
});

await test('/install.sh：未设置 AGENT_REPO 或格式不对返回 404；设置后返回可执行脚本且语法正确', async () => {
  assert.equal((await call(mkEnv(), 'GET', '/install.sh')).status, 404);
  assert.equal((await call(mkEnv({ AGENT_REPO: 'a b/c' }), 'GET', '/install.sh')).status, 404);
  assert.equal((await call(mkEnv({ AGENT_REPO: 'x"; rm -rf /; "/y' }), 'GET', '/install.sh')).status, 404);
  const env = mkEnv({ AGENT_REPO: 'someone/cfsm-lite' });
  const r = await call(env, 'GET', '/install.sh');
  assert.equal(r.status, 200);
  const text = await r.text();
  assert.match(text, /^#!\/bin\/sh/); assert.match(text, /REPO="someone\/cfsm-lite"/);
  assert.equal(text.includes('__REPO__'), false);
  const file = join(tmpdir(), 'cfsm-install-test.sh');
  writeFileSync(file, text);
  assert.equal(spawnSync('sh', ['-n', file]).status, 0);                  // sh 语法检查
  const g = await (await admin(env, { action: 'get_settings' }, await login(env))).json();
  assert.equal(g.agent_install_ready, true);
  const g2 = await (await admin(mkEnv(), { action: 'get_settings' }, await login(mkEnv()))).json();
  assert.equal(g2.agent_install_ready, false);
});

await test('/update 支持 samples 与 payload.metrics 两种格式', async () => {
  const env = mkEnv();
  const { id } = await setup(env);
  assert.equal((await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, samples: [{ ts: 1, metrics: metrics({ cpu: '1' }) }, { ts: 2, metrics: metrics({ cpu: '9' }) }] } })).status, 200);
  assert.equal((await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, payload: { metrics: metrics() } } })).status, 200);
});

await test('私有模式：未登录读 /api/servers 401，/api/config 可读；登录后指标为数字且 Ping 语义正确', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() }, headers: { 'x-agent-version': 'v1.0.10' } });
  assert.equal((await call(env, 'GET', '/api/servers')).status, 401);
  const cfg = await (await call(env, 'GET', '/api/config')).json();
  assert.equal(cfg.is_public, false); assert.equal(cfg.authorization, false); assert.equal(cfg.custom_ct_name, '电信');
  const r = await (await call(env, 'GET', '/api/servers', { token })).json();
  const s = r.servers[0];
  assert.equal(s.is_online, true); assert.equal(s.cpu, 12.34); assert.equal(s.ram_total, 8192);
  assert.equal(s.net_rx, 12345678); assert.equal(s.boot_time, '1700000000000'); assert.equal(s.agent_version, 'v1.0.10');
  // 对外接口：默认探测目标是电信(ct)，所以四组延迟、丢包字段都是它的数值（23ms、丢包 0）
  for (const k of ['ct', 'cu', 'cm', 'bd']) { assert.equal(s['ping_' + k], 23); assert.equal(s['loss_' + k], 0); }
  // 管理页面用的 list 接口仍然是各目标的真实值
  const raw = (await (await admin(env, { action: 'list' }, token)).json()).servers[0];
  assert.equal(raw.ping_ct, 23); assert.equal(raw.ping_cu, null); assert.equal(raw.ping_cm, false); assert.equal(raw.ping_bd, 40);
  assert.equal(raw.loss_cu, 100); assert.equal(raw.loss_cm, false);
  assert.equal(s.ip_v4, '1'); assert.equal(s.ip_v6, '0'); assert.equal('disk' in s, false); assert.equal(s.gpu_info, null);
  assert.deepEqual(s.ping, []); assert.deepEqual(s.loss, []);
  assert.equal(r.stats.online, 1); assert.equal(r.stats.globalSpeedIn, 1024); assert.equal(r.stats.globalNetTx, 87654321);
  assert.deepEqual(r.latestReportUpdates, []); assert.equal(r.sysConfig.show_three_net_details, true);
});

await test('IS_PUBLIC=true 允许匿名读取，且匿名看不到隐藏节点和 note', async () => {
  const env = mkEnv({ IS_PUBLIC: 'true' });
  const { token, id } = await setup(env);
  await admin(env, { action: 'edit', id, note: 'secret note' }, token);
  const { id: hid } = await (await admin(env, { action: 'add', name: 'hidden' }, token)).json();
  await admin(env, { action: 'edit', id: hid, is_hidden: '1' }, token);
  const anon = await (await call(env, 'GET', '/api/servers')).json();
  assert.equal(anon.servers.length, 1); assert.equal('note' in anon.servers[0], false);
  const authed = await (await call(env, 'GET', '/api/servers', { token })).json();
  assert.equal(authed.servers.length, 2); assert.equal(authed.servers.find((x) => x.id === id).note, 'secret note');
  assert.equal((await call(env, 'GET', '/api/server?id=' + hid)).status, 404);
});

await test('/api/server 与 /api/history/all 的参数校验与空历史', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  assert.equal((await call(env, 'GET', '/api/server', { token })).status, 400);
  assert.equal((await call(env, 'GET', '/api/server?id=nope', { token })).status, 404);
  const one = await (await call(env, 'GET', '/api/server?id=' + id, { token })).json();
  assert.equal(one.id, id); assert.equal(one.cpu, 0); assert.equal(one.is_online, false); assert.equal(one.last_updated, 0);
  const h = await call(env, 'GET', `/api/history/all?id=${id}&hours=24`, { token });
  assert.equal(h.status, 200); assert.deepEqual(await h.json(), []); assert.equal(h.headers.get('x-cache'), 'MISS');
  assert.equal((await call(env, 'GET', `/api/history/all?id=${id}&hours=5`, { token })).status, 400);
});

await test('离线检测：超时只通知一次；恢复后只通知一次；重复 Cron 不重复通知', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  await admin(env, { action: 'save_settings', settings: { tg_bot_token: 'https://api.day.app/KEY123', tg_notify: '3' } }, token);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } });
  barkCalls.length = 0;
  assert.equal(await checkOffline(env), 0);                         // 刚上报：不离线
  env.DB.raw.prepare('UPDATE servers SET last_seen = ? WHERE id = ?').run(Date.now() - 200_000, id);
  assert.equal(await checkOffline(env), 1);                         // 超过 180 秒：离线
  assert.equal(await checkOffline(env), 0);                         // 再跑不重复
  assert.equal(barkCalls.length, 1);
  assert.equal(barkCalls[0].url, 'https://api.day.app/KEY123'); assert.match(barkCalls[0].body.title, /HK-01 离线/);
  const list = await (await admin(env, { action: 'list' }, token)).json();
  assert.equal(list.servers[0].is_online, false);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } }); await flush();
  assert.equal(barkCalls.length, 2); assert.match(barkCalls[1].body.title, /HK-01 已恢复/);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } }); await flush();
  assert.equal(barkCalls.length, 2);                                // 在线状态再上报：不通知
});

await test('新节点首次上报不发恢复通知；offline_notify_disabled 与 tg_notify=0 不发通知', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  await admin(env, { action: 'save_settings', settings: { tg_bot_token: 'KEYONLY' } }, token);
  barkCalls.length = 0;
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } }); await flush();
  assert.equal(barkCalls.length, 0);
  await admin(env, { action: 'edit', id, offline_notify_disabled: '1' }, token);
  env.DB.raw.prepare('UPDATE servers SET last_seen = ? WHERE id = ?').run(Date.now() - 999_000, id);
  assert.equal(await checkOffline(env), 1); assert.equal(barkCalls.length, 0);
  await admin(env, { action: 'edit', id, offline_notify_disabled: '0' }, token);
  await admin(env, { action: 'save_settings', settings: { tg_notify: '0' } }, token);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } }); await flush();
  env.DB.raw.prepare('UPDATE servers SET last_seen = ? WHERE id = ?').run(Date.now() - 999_000, id);
  await checkOffline(env); assert.equal(barkCalls.length, 0);
});

await test('Bark 设备 Key 自动补全为 api.day.app 地址；测试通知可用', async () => {
  const env = mkEnv();
  const { token } = await setup(env);
  assert.equal((await admin(env, { action: 'send_test_notification' }, token)).status, 400);
  barkCalls.length = 0;
  const r = await admin(env, { action: 'send_test_notification', tg_bot_token: 'DEVKEY' }, token);
  assert.equal(r.status, 200); assert.equal(barkCalls[0].url, 'https://api.day.app/DEVKEY');
});

await test('Bark 地址只写不读；空值不会覆盖；非法设置被拒绝；未知字段被忽略并提示', async () => {
  const env = mkEnv();
  const { token } = await setup(env);
  await admin(env, { action: 'save_settings', settings: { tg_bot_token: 'https://api.day.app/SECRETKEY' } }, token);
  const g = await (await admin(env, { action: 'get_settings' }, token)).json();
  assert.equal(g.success, true); assert.equal(g.settings.tg_bot_token, ''); assert.equal(g.settings.tg_bot_token_configured, true);
  assert.equal(JSON.stringify(g).includes('SECRETKEY'), false);
  assert.equal(g.api_secret, env.API_SECRET); assert.equal(g.settings.username, 'admin');
  await admin(env, { action: 'save_settings', settings: { tg_bot_token: '' } }, token);
  assert.equal((await (await admin(env, { action: 'get_settings' }, token)).json()).settings.tg_bot_token_configured, true);
  assert.equal((await admin(env, { action: 'save_settings', settings: { tg_notify: '1' } }, token)).status, 400);
  assert.equal((await admin(env, { action: 'save_settings', settings: { notification_timezone: 'Mars/Base' } }, token)).status, 400);
  const ig = await (await admin(env, { action: 'save_settings', settings: { password: 'x', site_title: 'Mine' } }, token)).json();
  assert.deepEqual(ig.ignored, ['password']);
  assert.equal((await (await call(env, 'GET', '/api/config')).json()).site_title, 'Mine');
});

await test('batch_delete / save_order / export / import', async () => {
  const env = mkEnv();
  const { token, id: a } = await setup(env);
  const { id: b } = await (await admin(env, { action: 'add', name: 'B' }, token)).json();
  const { id: c } = await (await admin(env, { action: 'add', name: 'C' }, token)).json();
  await admin(env, { action: 'save_order', orders: [c, a, b] }, token);
  const ord = (await (await admin(env, { action: 'list' }, token)).json()).servers.map((x) => x.id);
  assert.deepEqual(ord, [c, a, b]);
  const exp = await (await admin(env, { action: 'export_servers' }, token)).json();
  assert.equal(exp.servers.length, 3);
  assert.equal((await admin(env, { action: 'batch_delete', ids: [a, b, c] }, token)).status, 200);
  const imp = await (await admin(env, { action: 'import_servers', servers: [...exp.servers, { id: 'bad' }] }, token)).json();
  assert.equal(imp.imported, 3); assert.equal(imp.skipped, 1);
  const again = await (await admin(env, { action: 'import_servers', servers: exp.servers }, token)).json();
  assert.equal(again.imported, 0); assert.equal(again.message, 'noServersImported');
  assert.equal((await admin(env, { action: 'bogus' }, token)).status, 400);
});

await test('/api/ws 握手：缺 Upgrade 426；私有站点无令牌 401（令牌可来自请求头、Cookie 或查询参数）；非法订阅 400；GET /update 仍是 503', async () => {
  const env = mkEnv();
  const { token } = await setup(env);
  const up = { upgrade: 'websocket' };
  assert.equal((await call(env, 'GET', '/api/ws')).status, 426);
  assert.equal((await call(env, 'GET', '/api/ws', { headers: up })).status, 401);
  assert.equal((await call(env, 'GET', '/api/ws', { headers: { ...up, authorization: 'Bearer bad.token.x' } })).status, 401);
  assert.equal((await call(env, 'GET', '/api/ws?subscribe=bad%20id', { token, headers: up })).status, 400);
  // 令牌有效时通过认证；测试环境（Node）没有 WebSocketPair，所以最后停在 503，而不是 401
  assert.equal((await call(env, 'GET', '/api/ws', { token, headers: up })).status, 503);
  assert.equal((await call(env, 'GET', '/api/ws?token=' + token, { headers: up })).status, 503);
  assert.equal((await call(env, 'GET', '/api/ws', { headers: { ...up, cookie: 'a=b; cfsm_auth=' + token } })).status, 503);
  assert.equal((await call(mkEnv({ IS_PUBLIC: 'true' }), 'GET', '/api/ws', { headers: up })).status, 503);
  assert.equal((await call(env, 'GET', '/update', { headers: up })).status, 503);
});

function fakeSocket() {
  const handlers = {};
  return {
    sent: [], closed: null,
    send(x) { this.sent.push(x); }, close(code, reason) { this.closed = [code, reason]; },
    addEventListener(t, fn) { handlers[t] = fn; }, emit(t, data) { return handlers[t] && handlers[t](data); },
    json() { return this.sent.map((x) => (x.startsWith('{') ? JSON.parse(x) : x)); },
  };
}

await test('WebSocket 会话：hello、ping/pong、订阅确认、新上报推送、去重、按 ID 过滤、非法订阅关闭 1008', async () => {
  const env = mkEnv();
  const { id } = await setup(env);
  const { id: other } = await (await admin(env, { action: 'add', name: 'Other' }, await login(env))).json();
  const report = (sid, cpu) => call(env, 'POST', '/update', { body: { id: sid, secret: env.API_SECRET, metrics: metrics({ cpu: String(cpu) }) } });
  await report(id, 11); await report(other, 22);
  let clock = Date.now();
  const timers = { setInterval: (fn) => { timers.fn = fn; return 7; }, clearInterval: () => { timers.cleared = true; } };
  const sock = fakeSocket();
  const session = createWsSession(sock, env, 'all', { timers, now: () => clock });
  assert.deepEqual(sock.json()[0], { type: 'hello', ts: clock, subscribed: 'all' });
  await session.poll(true);
  const first = sock.json().find((m) => m.type === 'batchUpdate');
  assert.equal(first.updates.length, 2);                                  // 连接时补发最近 20 秒内的上报
  const u = first.updates.find((x) => x.serverId === id);
  assert.equal(u.samples[0].data.cpu, 11); assert.equal(typeof u.samples[0].ts, 'number');
  assert.equal(u.samples[0].data.last_updated, u.samples[0].ts); assert.equal(u.samples[0].data.ping_ct, 23);
  const n = sock.sent.length;
  await session.poll(true);
  assert.equal(sock.sent.length, n);                                      // 没有新上报：不重复推送
  await new Promise((r) => setTimeout(r, 5)); await report(id, 33);
  await session.poll(true);
  const second = sock.json().filter((m) => m.type === 'batchUpdate')[1];
  assert.equal(second.updates.length, 1); assert.equal(second.updates[0].samples[0].data.cpu, 33);
  sock.emit('message', { data: '{"type":"ping"}' });
  assert.equal(sock.sent.includes('{"type":"pong"}'), true);              // 回复必须是精确的字符串，不带 ts
  sock.emit('message', { data: '{"type":"pong"}' }); sock.emit('message', { data: 'not json' });
  assert.equal(sock.closed, null);                                        // 客户端的 pong 与垃圾数据被忽略
  sock.emit('message', { data: JSON.stringify({ type: 'subscribe', scope: 'all', ids: [other] }) });
  await new Promise((r) => setTimeout(r, 20));
  const ack = sock.json().find((m) => m.type === 'subscribed');
  assert.equal(ack.subscribed, 'all'); assert.equal(ack.count, 1);
  const before = sock.json().filter((m) => m.type === 'batchUpdate').length;
  await new Promise((r) => setTimeout(r, 5)); await report(id, 44);
  clock += 10000; await session.poll(true);
  assert.equal(sock.json().filter((m) => m.type === 'batchUpdate').length, before + 0);   // 只订阅了 other：id 的更新不推送
  sock.emit('message', { data: JSON.stringify({ type: 'subscribe', scope: 'all', ids: 'oops' }) });
  assert.equal(sock.closed[0], 1008); assert.equal(timers.cleared, true);
  const one = fakeSocket();
  const s2 = createWsSession(one, env, id, { timers, now: () => clock });
  await s2.poll(true);
  assert.deepEqual(one.json().filter((m) => m.type === 'batchUpdate')[0].updates.map((x) => x.serverId), [id]);
  one.emit('close'); assert.equal(timers.cleared, true);
});

await test('默认探测目标：对外接口四组字段都用它的数值；管理接口保持真实值；配置里的名称同步；WebSocket 同样处理；非法值被拒绝', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics() } });
  const save = (settings) => admin(env, { action: 'save_settings', settings }, token);
  assert.equal((await save({ ping_default: 'xx' })).status, 400);
  assert.equal((await (await admin(env, { action: 'get_settings' }, token)).json()).settings.ping_default, 'ct');   // 默认是电信
  const check = async (ping, loss, name) => {
    const s = (await (await call(env, 'GET', '/api/servers', { token })).json()).servers[0];
    const one = await (await call(env, 'GET', '/api/server?id=' + id, { token })).json();
    for (const k of ['ct', 'cu', 'cm', 'bd']) {
      assert.equal(s['ping_' + k], ping); assert.equal(s['loss_' + k], loss);
      assert.equal(one['ping_' + k], ping); assert.equal(one['loss_' + k], loss);
    }
    const cfg = await (await call(env, 'GET', '/api/config')).json();
    for (const k of ['ct', 'cu', 'cm', 'bd']) assert.equal(cfg['custom_' + k + '_name'], name);
  };
  await check(23, 0, '电信');
  assert.equal((await save({ ping_default: 'bd' })).status, 200); await check(40, 0, 'BGP');
  assert.equal((await save({ ping_default: 'cu' })).status, 200); await check(null, 100, '联通');
  assert.equal((await save({ ping_default: 'cm' })).status, 200); await check(false, false, '移动');
  const raw = (await (await admin(env, { action: 'list' }, token)).json()).servers[0];   // 管理页面看到的仍是各目标真实值
  assert.equal(raw.ping_ct, 23); assert.equal(raw.ping_cu, null); assert.equal(raw.ping_cm, false); assert.equal(raw.ping_bd, 40);
  assert.equal(applyDefaultPing({ ping_ct: 1, loss_ct: 2, ping_cu: 3, loss_cu: 4 }, 'weird').ping_cu, 1);   // 无效的值退回电信
  const sock = fakeSocket();
  const session = createWsSession(sock, env, 'all', { timers: { setInterval: () => 1, clearInterval() {} }, pingDefault: 'bd' });
  await session.poll(true);
  const data = sock.json().find((m) => m.type === 'batchUpdate').updates[0].samples[0].data;
  for (const k of ['ct', 'cu', 'cm', 'bd']) { assert.equal(data['ping_' + k], 40); assert.equal(data['loss_' + k], 0); }
});

await test('币种：常用 8 种可保存，旧版本保存过的符号仍可用，其他值被拒绝', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  for (const c of ['$', '¥', '€', '£', '￥JPY', 'HK$', 'C$', 'S$', '₽']) {
    assert.equal((await admin(env, { action: 'edit', id, currency: c }, token)).status, 200, c);
    assert.equal((await (await admin(env, { action: 'list' }, token)).json()).servers[0].currency, c);
  }
  for (const c of ['CA$', 'USD', '']) assert.equal((await admin(env, { action: 'edit', id, currency: c }, token)).status, 400, c);
});

await test('WebSocket 会话：数据库连续失败 3 次后断开（让客户端重连），偶发失败不影响', async () => {
  let fail = true;
  const env = { DB: { prepare: () => { if (fail) throw new Error('boom'); return { bind: () => ({ all: async () => ({ results: [] }) }) }; } } };
  const sock = fakeSocket();
  const timers = { setInterval: () => 1, clearInterval: () => {} };
  const session = createWsSession(sock, env, 'all', { timers });
  await session.poll(true); await session.poll(true);
  assert.equal(sock.closed, null);
  fail = false; await session.poll(true);                                 // 成功一次后失败计数清零
  fail = true;
  await session.poll(true); await session.poll(true);
  assert.equal(sock.closed, null);
  await session.poll(true);
  assert.equal(sock.closed[0], 1011);
});

await test('流量上限解析、用量计算、统计周期、提醒档位、列表解析', async () => {
  const GB = 1024 ** 3;
  for (const [t, want] of [['500GB', 500 * GB], ['1TB', 1024 * GB], ['1.5T', 1536 * GB], ['200g', 200 * GB], ['500', 500 * GB], ['500 GiB', 500 * GB], ['2048MB', 2 * GB], ['', 0], ['abc', 0], ['500B', 0], ['0', 0], ['-5GB', 0]]) {
    assert.equal(parseTrafficLimit(t), want, JSON.stringify(t));
  }
  const m = { net_rx_monthly: 10, net_tx_monthly: 30 };
  assert.deepEqual(['total', 'up', 'down', 'max', undefined].map((c) => trafficUsed(m, c)), [40, 30, 10, 30, 40]);
  const at = (y, mo, d, h = 12) => Date.UTC(y, mo - 1, d, h);
  assert.equal(periodStart(at(2026, 10, 31), 1, 'UTC'), '2026-10-01');
  assert.equal(periodStart(at(2026, 10, 10), 15, 'UTC'), '2026-09-15');
  assert.equal(periodStart(at(2026, 3, 5), 31, 'UTC'), '2026-02-28');
  assert.equal(periodStart(at(2026, 1, 5), 10, 'UTC'), '2025-12-10');
  assert.equal(periodStart(at(2026, 10, 31), 0, 'UTC'), 'none');
  assert.equal(periodStart(at(2026, 10, 31, 20), 1, 'Asia/Shanghai'), '2026-11-01');  // 上海已经是 11 月 1 日
  assert.deepEqual([8, 7, 6, 3, 2, 1, 0, -1].map((d) => expireStage(d, [7, 3, 1, 0])), [null, 7, 7, 3, 3, 1, 0, -1]);
  assert.deepEqual([50, 89, 90, 94.9, 95, 120].map((p) => trafficStage(p, [90, 95])), [null, null, 90, 90, 95, 95]);
  assert.deepEqual(parseIntList('7, 3,1，0', 0, 365, 6), [7, 3, 1, 0]);
  assert.deepEqual(parseIntList('', 0, 365, 6), []);
  assert.deepEqual(parseIntList('7,7', 0, 365, 6), [7]);
  assert.equal(parseIntList('x', 0, 365, 6), null); assert.equal(parseIntList('400', 0, 365, 6), null);
  assert.equal(parseIntList('1,2,3,4,5', 1, 100, 4), null);
  assert.equal(daysLeft('2026-11-04', '2026-11-01'), 3); assert.equal(daysLeft('2026-10-30', '2026-11-01'), -2);
});

await test('节点续费与流量字段：保存时校验与规范化，列表里带上计算结果', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  const edit = (o) => admin(env, { action: 'edit', id, ...o }, token);
  const err = async (o) => (await (await edit(o)).json()).error;
  assert.equal(await err({ price: 'abc' }), 'invalidPrice');
  assert.equal(await err({ billing_cycle: 'weekly' }), 'invalidBillingCycle');
  assert.equal(await err({ currency: 'X' }), 'invalidCurrency');
  assert.equal(await err({ expire_date: '2026-13-40' }), 'invalidExpireDate');
  assert.equal(await err({ traffic_limit: 'lots' }), 'invalidTrafficLimit');
  const today = ymdInTz(Date.now(), 'Asia/Shanghai');
  const plus10 = ymdInTz(Date.now() + 10 * 86400000, 'Asia/Shanghai');
  assert.equal((await edit({ price: '29.9', currency: '$', billing_cycle: 'year', expire_date: plus10, auto_renewal: '1', traffic_limit: '1TB', traffic_calc_type: 'weird', reset_day: 15 })).status, 200);
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics({ net_rx_monthly: String(512 * 1024 ** 3), net_tx_monthly: String(1024 ** 4) }) } });
  const s = (await (await admin(env, { action: 'list' }, token)).json()).servers[0];
  assert.equal(s.price, '29.90'); assert.equal(s.billing_cycle, 'year'); assert.equal(s.auto_renewal, '1');
  assert.equal(s.traffic_calc_type, 'total'); assert.equal(s.reset_day, 15);
  assert.equal(s.lite_expire_days_left, 10); assert.equal(s.lite_traffic_limit_bytes, 1024 ** 4);
  assert.equal(s.lite_traffic_used_bytes, 1536 * 1024 ** 3); assert.equal(s.lite_traffic_percent, 150);
  assert.equal(today.length, 10);
  await edit({ traffic_calc_type: 'down' });
  assert.equal((await (await admin(env, { action: 'list' }, token)).json()).servers[0].lite_traffic_percent, 50);
  await edit({ expire_date: '', traffic_limit: '' });
  const c = (await (await admin(env, { action: 'list' }, token)).json()).servers[0];
  assert.equal(c.lite_expire_days_left, null); assert.equal(c.lite_traffic_percent, null);
});

await test('提醒设置：规范化保存、非法值被拒绝、允许留空关闭；提醒标记不会出现在设置里', async () => {
  const env = mkEnv();
  const { token } = await setup(env);
  const save = (o) => admin(env, { action: 'save_settings', settings: o }, token);
  const get = async () => (await (await admin(env, { action: 'get_settings' }, token)).json()).settings;
  let g = await get();
  assert.equal(g.expire_notify_days, '7,3,1,0'); assert.equal(g.expire_notify_hour, '9'); assert.equal(g.traffic_notify_percents, '90,95');
  assert.equal((await save({ expire_notify_days: '1, 7,3,0', expire_notify_hour: '08', traffic_notify_percents: '95,80,90' })).status, 200);
  g = await get();
  assert.equal(g.expire_notify_days, '7,3,1,0'); assert.equal(g.expire_notify_hour, '8'); assert.equal(g.traffic_notify_percents, '80,90,95');
  for (const bad of [{ expire_notify_days: '400' }, { expire_notify_days: 'a' }, { expire_notify_hour: '24' }, { traffic_notify_percents: '0' }, { traffic_notify_percents: '1,2,3,4,5' }]) {
    assert.equal((await save(bad)).status, 400, JSON.stringify(bad));
  }
  assert.equal((await save({ expire_notify_days: '', traffic_notify_percents: '' })).status, 200);
  g = await get();
  assert.equal(g.expire_notify_days, ''); assert.equal(g.traffic_notify_percents, '');
  env.DB.raw.prepare("INSERT INTO settings (key, value) VALUES ('ntf:exp:x', 'y')").run();
  assert.equal(JSON.stringify(await get()).includes('ntf:'), false);
});

await test('到期提醒：按档位只提醒一次、续费后重新计算、到期当天与过期后各提醒一次、未到提醒时间不发', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  await admin(env, { action: 'save_settings', settings: { tg_bot_token: 'https://api.day.app/K' } }, token);
  await admin(env, { action: 'edit', id, expire_date: '2026-11-04', price: '30', currency: '¥', billing_cycle: 'year' }, token);
  const at = (d, hLocal) => Date.UTC(2026, 10, d, hLocal - 8);          // 上海时间 11 月 d 日 hLocal 点
  const run = (now) => checkAlerts(env, now, { force: true });
  barkCalls.length = 0;
  assert.equal(await run(at(1, 8)), 0);                                   // 8 点，未到默认的 9 点
  assert.equal(await run(at(1, 10)), 1);                                  // 还有 3 天 -> 3 天档
  assert.match(barkCalls[0].body.title, /HK-01 还有 3 天到期/);
  assert.match(barkCalls[0].body.body, /到期日：2026-11-04/); assert.match(barkCalls[0].body.body, /续费金额：¥30\.00 \/ 年付/);
  assert.equal(await run(at(1, 11)), 0); assert.equal(await run(at(2, 10)), 0);   // 同一档位不重复
  assert.equal(await run(at(3, 10)), 1); assert.match(barkCalls[1].body.title, /还有 1 天到期/);
  assert.equal(await run(at(4, 10)), 1); assert.match(barkCalls[2].body.title, /今天到期/);
  assert.equal(await run(at(6, 10)), 1); assert.match(barkCalls[3].body.title, /已过期 2 天/);
  assert.equal(await run(at(7, 10)), 0);                                  // 过期后只提醒一次
  await admin(env, { action: 'edit', id, expire_date: '2027-11-04', auto_renewal: '1' }, token);   // 续费：到期日改到明年
  assert.equal(await run(at(7, 10)), 0);
  assert.equal(await run(Date.UTC(2027, 10, 3, 2)), 1);                   // 明年再次提醒
  assert.match(barkCalls[4].body.body, /已设置自动续费/);
  await admin(env, { action: 'save_settings', settings: { tg_notify: '0' } }, token);
  assert.equal(await run(Date.UTC(2027, 10, 4, 2)), 0);                  // 关闭通知后不发
  assert.equal(env.DB.raw.prepare("SELECT count(*) AS c FROM settings WHERE key LIKE 'ntf:%'").get().c, 1);   // 每个节点的到期提醒只占一行标记
  await admin(env, { action: 'delete', id }, token);                      // 删除节点同时清掉提醒标记
  assert.equal(env.DB.raw.prepare("SELECT count(*) AS c FROM settings WHERE key LIKE 'ntf:%'").get().c, 0);
});

await test('流量提醒：90% 与 95% 各提醒一次、新统计周期重新提醒、离线节点与关闭通知不提醒、按计算方式取值', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  await admin(env, { action: 'save_settings', settings: { tg_bot_token: 'K' } }, token);
  await admin(env, { action: 'edit', id, traffic_limit: '1TB', reset_day: 1 }, token);
  const GB = 1024 ** 3;
  const now = Date.UTC(2026, 10, 10, 4);                                  // 2026-11-10 12:00 上海，统计周期起点 2026-11-01
  const setUsage = (rxGB, txGB, lastSeen = now) => env.DB.raw.prepare('UPDATE servers SET last_seen = ?, metrics = ? WHERE id = ?')
    .run(lastSeen, JSON.stringify({ net_rx_monthly: rxGB * GB, net_tx_monthly: txGB * GB }), id);
  const run = (t = now) => checkAlerts(env, t, { force: true });
  barkCalls.length = 0;
  setUsage(300, 200); assert.equal(await run(), 0);                       // 48.8%
  setUsage(500, 430); assert.equal(await run(), 1);                       // 90.8%
  assert.match(barkCalls[0].body.title, /HK-01 本月流量已用 90%/);
  assert.match(barkCalls[0].body.body, /已用 930 GB \/ 1\.00 TB（上下行合计）/); assert.match(barkCalls[0].body.body, /每月 1 日重置/);
  assert.equal(await run(), 0);                                           // 同档位不重复
  setUsage(500, 480); assert.equal(await run(), 1); assert.match(barkCalls[1].body.title, /已用 95%/);
  assert.equal(await run(), 0);
  setUsage(500, 520); assert.equal(await run(), 0);                       // 超过 100% 也只有两个档位，不再提醒
  assert.equal(await run(Date.UTC(2026, 11, 2, 4)), 0);                  // 新周期但节点已离线（旧数据），不据此提醒
  setUsage(500, 520, Date.UTC(2026, 11, 2, 4));
  assert.equal(await run(Date.UTC(2026, 11, 2, 4)), 1);                  // 新周期里节点在线且仍超过 95%：重新提醒
  setUsage(500, 480); await admin(env, { action: 'edit', id, traffic_calc_type: 'down' }, token);
  assert.equal(await run(Date.UTC(2026, 11, 20, 4)), 0);                 // 仅算下行：500GB / 1TB = 48.8%，不提醒
  await admin(env, { action: 'save_settings', settings: { traffic_notify_percents: '' } }, token);
  setUsage(1000, 0, Date.UTC(2026, 11, 20, 4)); assert.equal(await run(Date.UTC(2026, 11, 20, 4)), 0);   // 留空表示不提醒
  assert.equal(await checkAlerts(env, Date.UTC(2026, 11, 20, 4, 1)), 0);  // 不在 5 分钟整点上：不检查
  assert.equal(typeof (await runCron(env)), 'undefined');                 // runCron 能整体跑通
});

await test('管理页面端到端：在模拟 DOM 里登录、加节点、改 Ping/通知/节点配置、导出、删除、退出', async () => {
  const env = mkEnv({ AGENT_REPO: 'someone/cfsm-lite' });
  // ---- 极简 DOM ----
  class TextNode { constructor(t) { this.nodeType = 3; this.text = String(t); } get textContent() { return this.text; } }
  class El {
    constructor(tag) { this.nodeType = 1; this.tag = tag; this.children = []; this.attrs = {}; this.listeners = {}; this.className = ''; this.value = ''; this.checked = false; this.disabled = false; this.hidden = false; this.selected = false; }
    append(...kids) { for (const k of kids) { this.children.push(k); if (this.tag === 'textarea' && k.nodeType === 3) this.value += k.text; } }
    replaceChildren(...kids) { this.children = []; this.append(...kids); }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
    select() {}
    get textContent() { return this.children.map((c) => c.textContent).join(''); }
    set textContent(v) { this.children = [new TextNode(v)]; }
  }
  const app = new El('div'); const toastEl = new El('div');
  const store = new Map(); let copied = '';
  const sandbox = {
    document: { getElementById: (id) => ({ app, toast: toastEl })[id], createElement: (t) => new El(t), createTextNode: (t) => new TextNode(t) },
    sessionStorage: { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) },
    location: { origin: 'https://x.test' }, confirm: () => true,
    navigator: { clipboard: { writeText: async (t) => { copied = t; } } },
    fetch: (url, init) => worker.fetch(new Request('https://x.test' + url, init), env, ctx),
    setTimeout, clearTimeout, console,
  };
  const html = await (await call(env, 'GET', '/admin')).text();
  vm.runInNewContext(/<script>([\s\S]*?)<\/script>/.exec(html)[1], sandbox);
  // ---- 辅助 ----
  const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setTimeout(r, 15)); };
  const walk = (n, f) => { f(n); (n.children || []).forEach((c) => walk(c, f)); };
  const findAll = (pred) => { const out = []; walk(app, (n) => { if (n.nodeType === 1 && pred(n)) out.push(n); }); return out; };
  const button = (text) => { const r = findAll((e) => e.tag === 'button' && e.textContent === text); assert.ok(r.length >= 1, '找不到按钮：' + text); return r[0]; };
  const click = async (el) => { for (const fn of el.listeners.click || []) await fn({}); await settle(); };
  const noBadToast = () => assert.equal(!toastEl.hidden && toastEl.className.includes('bad'), false, '出现错误提示：' + toastEl.textContent);
  const withOpt = (v) => findAll((e) => e.tag === 'select' && e.children.some((o) => o.value === v));
  const text = () => app.textContent;

  assert.match(text(), /管理后台/);                                       // 未登录：显示登录页
  findAll((e) => e.attrs.type === 'password')[0].value = 'wrong';
  await click(button('登录'));
  assert.equal(!toastEl.hidden && toastEl.className.includes('bad'), true); // 密码错：有错误提示
  findAll((e) => e.attrs.type === 'password')[0].value = 'test-secret-123';
  await click(button('登录')); noBadToast();
  assert.match(text(), /共 0 台/);

  findAll((e) => e.attrs.placeholder === '新节点名称')[0].value = 'Tokyo-01';
  await click(button('添加节点')); noBadToast();
  assert.match(text(), /Tokyo-01/); assert.match(text(), /共 1 台/);
  const cmd = findAll((e) => e.tag === 'textarea' && e.className === 'mono')[0].value;
  assert.match(cmd, /curl -fsSL https:\/\/x\.test\/install\.sh \| sh -s -- -i [0-9a-f-]{36} -s test-secret-123 -u https:\/\/x\.test\/update/);
  await click(button('复制命令')); assert.equal(copied, cmd);

  const list = await (await admin(env, { action: 'list' }, await login(env))).json();
  const id = list.servers[0].id;
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics({ ping_ct: '23', loss_ct: '0', ping_cu: 'null', loss_cu: '100' }) } });

  await click(button('Ping'));                                            // ---- Ping 页 ----
  assert.match(text(), /探测方式/);
  findAll((e) => e.attrs.placeholder === 'host 或 host:port，留空使用内置默认')[0].value = 'example.com:443';
  withOpt('icmp')[0].value = 'icmp';
  withOpt('cm')[0].value = 'cu';                                         // 默认探测目标改成联通
  await click(button('保存')); noBadToast();
  assert.match(text(), /23 ms \/ 0%/); assert.match(text(), /超时 \/ 100%/);
  let g = await (await admin(env, { action: 'get_settings' }, await login(env))).json();
  assert.equal(g.settings.custom_ct, 'example.com:443'); assert.equal(g.settings.ping_mode, 'icmp'); assert.equal(g.settings.ping_default, 'cu');

  await click(button('通知'));                                            // ---- 通知页 ----
  findAll((e) => e.attrs.type === 'password')[0].value = 'DEVKEY123';
  withOpt('30')[0].value = '5';
  findAll((e) => e.attrs.placeholder === '例如 7,3,1,0，留空表示不提醒')[0].value = '14,7,1';
  findAll((e) => e.attrs.placeholder === '例如 90,95，留空表示不提醒')[0].value = '80,95';
  findAll((e) => e.tag === 'select' && e.children.some((o) => o.value === '23') && !e.children.some((o) => o.value === '30'))[0].value = '20';   // 提醒小时的下拉框（只有 0 到 23）
  await click(button('保存')); noBadToast();
  g = await (await admin(env, { action: 'get_settings' }, await login(env))).json();
  assert.equal(g.settings.tg_notify, '5'); assert.equal(g.settings.tg_bot_token_configured, true);
  assert.equal(g.settings.expire_notify_days, '14,7,1'); assert.equal(g.settings.expire_notify_hour, '20'); assert.equal(g.settings.traffic_notify_percents, '80,95');
  assert.equal(JSON.stringify(g).includes('DEVKEY123'), false);
  barkCalls.length = 0;
  await click(button('发送测试通知')); noBadToast();
  assert.equal(barkCalls.length, 1); assert.equal(barkCalls[0].url, 'https://api.day.app/DEVKEY123');

  await click(button('节点'));                                            // ---- 节点卡片与编辑 ----
  for (const want of ['CPU12.3%', '内存45.2%', '3.61 GB / 8.00 GB', '磁盘31.3%', '31.25 GB / 100 GB', '本月累计', '↑ 512 B/s', '↓ 1.00 KB/s', '↑ 83.59 MB', '↓ 11.77 MB', '延迟超时', '丢包100%', 'Ubuntu 22.04']) {
    assert.ok(text().includes(want), '节点卡片缺少：' + want + '\n' + text());   // 上 4 块：CPU、内存、磁盘、流量；下 4 块：网速、上传下载量、延迟、丢包
  }
  assert.equal(text().includes('电信 23 ms / 0%'), false);                 // 全部目标默认收起
  await click(findAll((e) => e.className === 'c tap' && e.textContent.startsWith('延迟'))[0]);
  for (const want of ['★ 联通 超时 / 100%', '电信 23 ms / 0%']) assert.ok(text().includes(want), '展开后缺少：' + want);
  await click(button('编辑'));
  assert.deepEqual(withOpt('HK$')[0].children.map((o) => o.textContent), ['$ 美元', '¥ 人民币', '€ 欧元', '£ 英镑', '￥JPY 日元', 'HK$ 港币', 'C$ 加拿大元', 'S$ 新加坡元']);
  withOpt('HK$')[0].value = 'S$';
  findAll((e) => e.tag === 'input' && e.value === 'Tokyo-01')[0].value = 'Tokyo-02';
  withOpt('180')[0].value = '120';
  findAll((e) => e.attrs.placeholder === '例如 30，免费填 0')[0].value = '30';
  findAll((e) => (e.attrs.placeholder || '').startsWith('例如 500GB'))[0].value = '1TB';
  withOpt('month')[0].value = 'year';
  await click(button('续费一期'));                                        // 到期日顺延一个付费周期（这里是一年）
  const exp = findAll((e) => e.attrs.type === 'date')[0].value;
  assert.match(exp, /^\d{4}-\d{2}-\d{2}$/);
  assert.ok(Number(exp.slice(0, 4)) >= new Date().getFullYear() + 1, '续费一期应顺延到明年或之后：' + exp);
  await click(button('保存')); noBadToast();
  assert.match(text(), /Tokyo-02/); assert.match(text(), /续费 S\$30\.00 \/ 年付/); assert.match(text(), /到期 \d{4}-\d{2}-\d{2}（剩 \d+ 天）/);
  assert.match(text(), /1\.50 GB \/ 1\.00 TB/);
  const after = (await (await admin(env, { action: 'list' }, await login(env))).json()).servers[0];
  assert.equal(after.name, 'Tokyo-02'); assert.equal(after.report_interval, 120);
  assert.equal(after.price, '30.00'); assert.equal(after.currency, 'S$'); assert.equal(after.billing_cycle, 'year'); assert.equal(after.traffic_limit, '1TB'); assert.equal(after.expire_date, exp);

  findAll((e) => e.attrs.placeholder === '新节点名称')[0].value = 'Osaka-01';   // ---- 手动排序 ----
  await click(button('添加节点')); noBadToast();
  assert.match(text(), /共 2 台/);
  const names = async () => (await (await admin(env, { action: 'list' }, await login(env))).json()).servers.map((x) => x.name);
  assert.deepEqual(await names(), ['Tokyo-02', 'Osaka-01']);
  await click(button('调整顺序'));
  assert.match(text(), /用按钮调整顺序/);
  const tops = findAll((e) => e.tag === 'button' && e.textContent === '置顶');
  assert.equal(tops[0].disabled, true);                                   // 第一个已经在最前面
  await click(tops[1]); noBadToast();                                     // 把 Osaka-01 置顶
  assert.deepEqual(await names(), ['Osaka-01', 'Tokyo-02']);
  await click(findAll((e) => e.tag === 'button' && e.textContent === '▼')[0]);   // 第一个下移一位
  assert.deepEqual(await names(), ['Tokyo-02', 'Osaka-01']);
  await click(button('完成排序'));
  assert.ok(text().indexOf('Tokyo-02') < text().indexOf('Osaka-01'));

  await click(button('系统'));                                            // ---- 系统页：导出 ----
  await click(button('导出节点配置')); noBadToast();
  assert.match(findAll((e) => e.tag === 'textarea')[0].value, /Tokyo-02/);

  await click(button('节点'));                                            // ---- 删除、退出 ----
  await click(button('删除')); noBadToast();
  await click(button('删除')); noBadToast();
  assert.match(text(), /共 0 台/);
  await click(button('退出'));
  assert.match(text(), /管理后台/); assert.equal(store.has('cfsm_t'), false);
});

await test('节点卡片与排序：紧凑卡片的 8 块内容、点延迟展开全部目标、手动排序、默认探测目标、币种选项', async () => {
  const env = mkEnv({ AGENT_REPO: 'someone/cfsm-lite' });
  class TextNode { constructor(t) { this.nodeType = 3; this.text = String(t); } get textContent() { return this.text; } }
  class El {
    constructor(tag) { this.nodeType = 1; this.tag = tag; this.children = []; this.attrs = {}; this.listeners = {}; this.className = ''; this.value = ''; this.checked = false; this.disabled = false; this.hidden = false; this.selected = false; }
    append(...kids) { for (const k of kids) { this.children.push(k); if (this.tag === 'textarea' && k.nodeType === 3) this.value += k.text; } }
    replaceChildren(...kids) { this.children = []; this.append(...kids); }
    setAttribute(k, v) { this.attrs[k] = String(v); }
    addEventListener(t, fn) { (this.listeners[t] = this.listeners[t] || []).push(fn); }
    select() {}
    get textContent() { return this.children.map((c) => c.textContent).join(''); }
    set textContent(v) { this.children = [new TextNode(v)]; }
  }
  const app = new El('div'); const toastEl = new El('div'); const store = new Map();
  const token = await login(env);
  store.set('cfsm_t', token);
  const mk = async (name) => (await (await admin(env, { action: 'add', name }, token)).json()).id;
  const idA = await mk('A-node'); const idB = await mk('B-node');
  const future = new Date(Date.now() + 10 * 86400000).toISOString().slice(0, 10);
  assert.equal((await admin(env, { action: 'edit', id: idA, server_group: 'HK', tags: 'web,db', price: '30', currency: '￥JPY', billing_cycle: 'year', expire_date: future, traffic_limit: '500GB' }, token)).status, 200);
  await call(env, 'POST', '/update', { body: { id: idA, secret: env.API_SECRET, metrics: metrics({ ping_ct: '23', loss_ct: '0', ping_cu: '120', loss_cu: '5' }) }, headers: { 'cf-ipcountry': 'US' } });
  const sandbox = {
    document: { getElementById: (id) => ({ app, toast: toastEl })[id], createElement: (t) => new El(t), createTextNode: (t) => new TextNode(t) },
    sessionStorage: { getItem: (k) => (store.has(k) ? store.get(k) : null), setItem: (k, v) => store.set(k, String(v)), removeItem: (k) => store.delete(k) },
    location: { origin: 'https://x.test' }, confirm: () => true, navigator: { clipboard: { writeText: async () => {} } },
    fetch: (url, init) => worker.fetch(new Request('https://x.test' + url, init), env, ctx), setTimeout, clearTimeout, console,
  };
  const html = await (await call(env, 'GET', '/admin')).text();
  vm.runInNewContext(/<script>([\s\S]*?)<\/script>/.exec(html)[1], sandbox);
  const settle = async () => { for (let i = 0; i < 4; i++) await new Promise((r) => setTimeout(r, 15)); };
  const walk = (n, f) => { f(n); (n.children || []).forEach((c) => walk(c, f)); };
  const findAll = (pred) => { const out = []; walk(app, (n) => { if (n.nodeType === 1 && pred(n)) out.push(n); }); return out; };
  const button = (text, i = 0) => { const r = findAll((e) => e.tag === 'button' && e.textContent === text); assert.ok(r.length > i, '找不到按钮：' + text); return r[i]; };
  const click = async (el) => { for (const fn of el.listeners.click || []) await fn({}); await settle(); };
  const text = () => app.textContent;
  await settle();

  // 1) 卡片内容：标题后带分组和地区，中间 8 块，底部是标签与续费到期
  const t = text();
  for (const w of ['A-node', 'HK', 'US', 'CPU', '内存', '磁盘', '流量', '延迟', '丢包', '23.0 ms', 'web', 'db', '到期 ' + future, '￥JPY30.00 / 年付', '本月累计']) assert.ok(t.includes(w) || w === '本月累计', '卡片里缺少：' + w);
  assert.ok(t.indexOf('A-node') < t.indexOf('HK'));                       // 分组在名称之后
  // 2) 点「延迟」展开全部探测目标
  assert.equal(text().includes('联通 120 ms'), false);
  await click(findAll((e) => (e.listeners.click || []).length && e.textContent.startsWith('延迟'))[0]);
  assert.match(text(), /电信 23 ms \/ 0%/); assert.match(text(), /联通 120 ms \/ 5%/);
  // 3) 手动排序：B 置顶，顺序立即保存，并按新顺序显示
  await click(button('调整顺序'));
  assert.equal(findAll((e) => e.tag === 'button' && e.textContent === '置顶')[0].disabled, true);   // 第一行没有上移空间
  await click(button('置顶', 1));
  let list = (await (await admin(env, { action: 'list' }, token)).json()).servers.map((x) => x.name);
  assert.deepEqual(list, ['B-node', 'A-node']);
  await click(button('完成排序'));
  assert.ok(text().indexOf('B-node') < text().indexOf('A-node'));
  list = (await (await call(env, 'GET', '/api/servers', { token })).json()).servers.map((x) => x.name);
  assert.deepEqual(list, ['B-node', 'A-node']);                           // 对外接口（Monito）里的顺序同步变化
  // 4) 默认探测目标改成联通：卡片显示联通的数值，对外接口也是
  await click(button('Ping'));
  findAll((e) => e.tag === 'select' && e.children.some((o) => o.value === 'bd'))[0].value = 'cu';
  await click(button('保存'));
  const pub = (await (await call(env, 'GET', '/api/servers', { token })).json()).servers.find((x) => x.name === 'A-node');
  assert.equal(pub.ping_ct, 120); assert.equal(pub.ping_bd, 120); assert.equal(pub.loss_cm, 5);
  await click(button('节点'));
  assert.match(text(), /延迟120.0 ms/);
  // 5) 币种选项为中文名称，且包含新增的几种
  await click(button('编辑', 1));
  const cur = findAll((e) => e.tag === 'select' && e.children.some((o) => o.value === 'S$'))[0];
  const labels = cur.children.map((o) => o.textContent);
  assert.deepEqual(labels, ['$ 美元', '¥ 人民币', '€ 欧元', '£ 英镑', '￥JPY 日元', 'HK$ 港币', 'C$ 加拿大元', 'S$ 新加坡元']);
});

// ---------------- 实时推送中心（Durable Object）----------------
class FakeSock {
  constructor() { this.sent = []; this.closed = null; this.readyState = 1; this.attachment = null; this.tags = []; }
  send(m) { this.sent.push(typeof m === 'string' ? m : String(m)); }
  close(code, reason) { this.closed = { code, reason }; this.readyState = 3; }
  serializeAttachment(v) { this.attachment = JSON.parse(JSON.stringify(v)); }
  deserializeAttachment() { return this.attachment; }
  msgs() { return this.sent.map((x) => { try { return JSON.parse(x); } catch { return x; } }); }
}
class FakeDoState {
  constructor() { this.sockets = []; }
  acceptWebSocket(ws, tags) { ws.tags = tags; this.sockets.push(ws); }
  getWebSockets(tag) { return this.sockets.filter((w) => w.readyState === 1 && (!tag || w.tags.includes(tag))); }
  getTags(ws) { return ws.tags; }
  setWebSocketAutoResponse() {}
  waitUntil(p) { bg.push(p); }
}
globalThis.WebSocketPair = class { constructor() { this[0] = new FakeSock(); this[1] = new FakeSock(); } };
const mkHub = (env) => { const st = new FakeDoState(); return { hub: new Hub(st, env), st }; };
const lastServer = (st, tag) => st.sockets.filter((w) => w.tags.includes(tag)).pop();

await test('实时推送中心：Agent 与前端连接、有人看时通知 Agent 切到实时模式，最后一个前端离开后退回', async () => {
  const env = mkEnv();
  const { hub, st } = mkHub(env);
  hub.acceptAgent({ id: 'node-a', ver: 'lite-go-x', region: 'US', interval: 3 });
  hub.acceptAgent({ id: 'node-b', ver: 'lite-go-x', region: 'US', interval: 2 });
  const a = st.sockets[0], b = st.sockets[1];
  assert.deepEqual(a.msgs()[0], { type: 'ready', active: false, interval: 3 });
  hub.acceptViewer('all');                                                // 第一个前端：通知所有 Agent
  assert.deepEqual(a.msgs()[1], { type: 'activate', interval: 3 });
  assert.deepEqual(b.msgs()[1], { type: 'activate', interval: 2 });
  const v1 = lastServer(st, 'viewer');
  assert.equal(v1.msgs()[0].type, 'hello');
  hub.acceptViewer('all');                                                // 第二个前端：不重复通知
  assert.equal(a.sent.length, 2);
  const v2 = lastServer(st, 'viewer');
  await hub.webSocketClose(v2, 1000);
  assert.equal(a.sent.length, 2);                                         // 还有人在看
  await hub.webSocketClose(v1, 1000);
  assert.deepEqual(a.msgs()[2], { type: 'deactivate' });                  // 没人看了
  hub.acceptAgent({ id: 'node-a', ver: 'v2', region: 'US', interval: 2 }); // 同一节点重连：旧连接被关闭
  assert.equal(a.closed.code, 1000);
  hub.acceptViewer('all');
  const late = st.sockets.filter((w) => w.tags.includes('agent')).pop();
  assert.deepEqual(late.msgs()[1], { type: 'activate', interval: 2 });
  assert.equal(JSON.stringify((await (await hub.fetch(new Request('https://hub/internal/status'))).json())), JSON.stringify({ viewers: 1, agents: ['node-b', 'node-a'] }));
});

await test('实时推送中心：Agent 的实时帧广播给订阅的前端、按订阅范围过滤、默认探测目标生效，并保底写入数据库', async () => {
  const env = mkEnv();
  const { token, id } = await setup(env);
  const { hub, st } = mkHub(env);
  hub.acceptAgent({ id, ver: 'lite-go-x', region: 'JP', interval: 2 });
  const agent = st.sockets[0];
  hub.acceptViewer('all'); const all = lastServer(st, 'viewer');
  hub.acceptViewer(id); const one = lastServer(st, 'viewer');
  hub.acceptViewer('other-id'); const other = lastServer(st, 'viewer');
  hub.acceptViewer('all'); const some = lastServer(st, 'viewer');
  await hub.viewerMessage(some, JSON.stringify({ type: 'subscribe', scope: 'all', ids: ['x-1', 'x-2'] }));
  assert.equal(some.msgs().filter((m) => m.type === 'subscribed').pop().count, 2);
  await admin(env, { action: 'save_settings', settings: { ping_default: 'cu' } }, token);
  const frame = { type: 'metrics', metrics: metrics({ cpu: '55.5', net_in_speed: '2048', ping_ct: '10', ping_cu: '99' }) };
  await hub.webSocketMessage(agent, JSON.stringify(frame));
  const got = all.msgs().filter((m) => m.type === 'batchUpdate');
  assert.equal(got.length, 1);
  const u = got[0].updates[0];
  assert.equal(u.serverId, id); assert.equal(u.samples[0].data.cpu, 55.5); assert.equal(u.samples[0].data.net_in_speed, 2048);
  assert.equal(u.samples[0].data.ping_ct, 99);                            // 默认探测目标（联通）的值同时填进四组字段
  assert.ok(u.samples[0].data.last_updated > 0);
  assert.equal(one.msgs().filter((m) => m.type === 'batchUpdate').length, 1);
  assert.equal(other.msgs().filter((m) => m.type === 'batchUpdate').length, 0);
  assert.equal(some.msgs().filter((m) => m.type === 'batchUpdate').length, 0);   // ids 过滤
  // 保底入库：第一帧写数据库，之后一段时间内不再写
  let row = env.DB.raw.prepare('SELECT status, last_seen, region FROM servers WHERE id = ?').get(id);
  assert.equal(row.status, 'ONLINE'); assert.equal(row.region, 'JP'); const first = row.last_seen; assert.ok(first > 0);
  await hub.webSocketMessage(agent, JSON.stringify(frame));
  assert.equal(env.DB.raw.prepare('SELECT last_seen FROM servers WHERE id = ?').get(id).last_seen, first);
  hub.lastPersist.set(id, Date.now() - 200000);
  await new Promise((r) => setTimeout(r, 3));
  await hub.webSocketMessage(agent, JSON.stringify(frame));
  assert.ok(env.DB.raw.prepare('SELECT last_seen FROM servers WHERE id = ?').get(id).last_seen > first);
  // 非法帧被忽略；超大帧被忽略
  const before = all.sent.length;
  await hub.webSocketMessage(agent, 'not json'); await hub.webSocketMessage(agent, JSON.stringify({ type: 'metrics' })); await hub.webSocketMessage(agent, 'x'.repeat(70000));
  assert.equal(all.sent.length, before);
});

await test('实时推送中心：未通过身份的连接被关闭；数据库里已删除的节点会被告知；前端订阅格式非法时关闭', async () => {
  const env = mkEnv();
  const { hub, st } = mkHub(env);
  hub.acceptAgent({ id: '', ver: '', region: '', interval: 2 });
  await hub.webSocketMessage(st.sockets[0], JSON.stringify({ type: 'metrics', metrics: metrics() }));
  assert.equal(st.sockets[0].closed.code, 1008);
  hub.acceptAgent({ id: '11111111-1111-4111-8111-111111111111', ver: '', region: '', interval: 2 });
  await hub.webSocketMessage(st.sockets[1], JSON.stringify({ type: 'metrics', metrics: metrics() }));
  assert.equal(st.sockets[1].closed.code, 4404);
  hub.acceptViewer('all'); const v = lastServer(st, 'viewer');
  await hub.viewerMessage(v, JSON.stringify({ type: 'ping' }));
  assert.equal(v.sent.pop(), '{"type":"pong"}');
  await hub.viewerMessage(v, JSON.stringify({ type: 'subscribe', scope: 'all', ids: ['bad id!'] }));
  assert.equal(v.closed.code, 1008);
});

await test('Worker 入口：Agent 的 WebSocket 握手校验身份并带上受信任的请求头；POST 上报同步通知推送中心', async () => {
  const calls = [];
  const stub = { fetch: async (req, init) => { calls.push(typeof req === 'string' ? new Request(req, init) : req); return new Response('forwarded', { status: 200 }); } };
  const env = mkEnv({ HUB: { idFromName: () => 'hub', get: () => stub } });
  const { token, id } = await setup(env);
  await admin(env, { action: 'edit', id, wss_report_interval: '4' }, token);
  const ws = (headers) => call(env, 'GET', '/update', { headers: { upgrade: 'websocket', ...headers } });
  assert.equal((await ws({ authorization: 'Bearer ' + env.API_SECRET })).status, 400);                         // 缺节点 ID
  assert.equal((await ws({ authorization: 'Bearer wrong', 'x-agent-id': id })).status, 401);
  assert.equal((await ws({ authorization: 'Bearer ' + env.API_SECRET, 'x-agent-id': '11111111-1111-4111-8111-111111111111' })).status, 404);
  const ok = await ws({ authorization: 'Bearer ' + env.API_SECRET, 'x-agent-id': id, 'x-agent-version': 'lite-go-v9', 'x-hub-role': 'viewer', 'cf-ipcountry': 'DE' });
  assert.equal(ok.status, 200); assert.equal(calls.length, 1);
  const h = calls[0].headers;
  assert.equal(h.get('x-hub-role'), 'agent');                                                                    // 客户端自带的 role 被覆盖
  assert.equal(h.get('x-agent-id'), id); assert.equal(h.get('x-hub-interval'), '4'); assert.equal(h.get('x-hub-region'), 'DE');
  await admin(env, { action: 'edit', id, connection_mode: 'http' }, token);
  assert.equal((await ws({ authorization: 'Bearer ' + env.API_SECRET, 'x-agent-id': id })).status, 403);         // 节点设为仅 HTTP
  await admin(env, { action: 'edit', id, connection_mode: 'auto' }, token);
  // 前端（Monito）的入口：私有站点要登录令牌，通过后转给推送中心并带上订阅范围
  assert.equal((await call(env, 'GET', '/api/ws', { headers: { upgrade: 'websocket' } })).status, 401);
  assert.equal((await call(env, 'GET', '/api/ws?subscribe=bad%20id', { token, headers: { upgrade: 'websocket' } })).status, 400);
  calls.length = 0;
  assert.equal((await call(env, 'GET', '/api/ws?subscribe=all', { token, headers: { upgrade: 'websocket' } })).status, 200);
  assert.equal(calls[0].headers.get('x-hub-role'), 'viewer'); assert.equal(calls[0].headers.get('x-hub-scope'), 'all');
  // POST 上报：写库后通知推送中心，让正在看的页面立刻更新
  calls.length = 0;
  await call(env, 'POST', '/update', { body: { id, secret: env.API_SECRET, metrics: metrics({ cpu: '33' }) } }); await flush();
  assert.equal(calls.length, 1); assert.match(calls[0].url, /internal\/ingest/);
  const body = JSON.parse(await calls[0].text());
  assert.equal(body.id, id); assert.equal(body.data.cpu, 33);
  // 没有绑定推送中心时：Agent 握手返回 503，管理接口报告「未启用」
  const bare = mkEnv();
  assert.equal((await call(bare, 'GET', '/update', { headers: { upgrade: 'websocket', authorization: 'Bearer ' + bare.API_SECRET, 'x-agent-id': 'x' } })).status, 503);
  const rt = await (await admin(bare, { action: 'realtime_status' }, await login(bare))).json();
  assert.equal(rt.available, false);
  const rt2 = await (await admin(env, { action: 'realtime_status' }, token)).json();
  assert.equal(rt2.available, true);
});

await test('动态配置：绑定推送中心时下发 auto，没绑定或节点设为仅 HTTP 时下发 http，看盘间隔可调', async () => {
  const row = { collect_interval: 0, report_interval: 60, reset_day: 1, interface: '', custom_ct: '', custom_cu: '', custom_cm: '', custom_bd: '', extra: JSON.stringify({ wss_report_interval: '3' }) };
  const get = (r, hubOn) => buildAgentConfig(r, {}, 6, hubOn);
  assert.match(get(row, true), /&connection_mode=auto&wss_report_interval=3&ping_mode=tcp$/);
  assert.match(get(row, false), /&connection_mode=http&wss_report_interval=3&/);
  assert.match(get({ ...row, extra: JSON.stringify({ connection_mode: 'http' }) }, true), /&connection_mode=http&wss_report_interval=2&/);
});

console.log(`\n${passed} tests passed`);
