const test = require('node:test');
const assert = require('node:assert/strict');
const { load } = require('./load-typescript.cjs');

const f = load('src/components/modules/key/lib/key-filters.ts');
const g = load('src/components/modules/key/lib/key-guide.ts');
const NOW = 1_800_000_000;
const DAY = 86400;

const key = (over) => ({ id: 1, name: 'k', api_key: 'sk-octopus-AbCdEfGhIjKlMnOpHm4q', user_id: 2, enabled: true, ...over });

test('状态和后端鉴权一样：停用 > 过期 > 用完 > 能用', () => {
    assert.equal(f.keyStatus(key({ enabled: false, expire_at: NOW - 1, max_cost: 1 }), 5, NOW), 'off');
    assert.equal(f.keyStatus(key({ expire_at: NOW - 1, max_cost: 1 }), 5, NOW), 'expired');
    assert.equal(f.keyStatus(key({ max_cost: 1 }), 1.01, NOW), 'used_up');
    assert.equal(f.keyStatus(key({ max_cost: 1 }), 1, NOW), 'ok', '正好等于上限还没超');
    assert.equal(f.keyStatus(key({ expire_at: 0, max_cost: 0 }), 999, NOW), 'ok', '0 = 永久 / 不限');
});

test('遮住 Key：列表只露末 4 位，代码块露前缀；都不是完整 Key', () => {
    const k = 'sk-octopus-AbCdEfGhIjKlMnOpHm4q';
    assert.equal(f.maskKey(k), 'sk-…Hm4q');
    assert.equal(f.maskKeyLong(k), 'sk-octopus-AbCd••••Hm4q');
    assert.ok(!f.maskKey(k).includes('AbCd'));
    assert.equal(f.maskKey(''), '');
});

test('协议：空 = 全部，旧字段名和 openai 也认', () => {
    assert.deepEqual(f.keyFamilies(key({})), ['openai-compatible', 'anthropic', 'gemini', 'video']);
    assert.deepEqual(f.keyFamilies(key({ endpoint_families: [] })), ['openai-compatible', 'anthropic', 'gemini', 'video']);
    assert.deepEqual(f.keyFamilies(key({ endpoint_scopes: 'openai,video' })), ['openai-compatible', 'video']);
    assert.equal(f.isAllFamilies(['video', 'gemini', 'anthropic', 'openai-compatible']), true);
});

test('模型：空 = 全部模型（不限），有值 = 只限这些', () => {
    assert.deepEqual(f.keyModels(key({})), []);
    assert.deepEqual(f.keyModels(key({ supported_models: 'a, b,,c' })), ['a', 'b', 'c']);
});

test('筛选 + 计数：页签数字按其他筛选算，不随当前页签变', () => {
    const keys = [
        key({ id: 1, name: '笔记本', user_id: 2 }),
        key({ id: 2, name: '服务器', user_id: 2, enabled: false }),
        key({ id: 3, name: 'CI', user_id: 3, expire_at: NOW - DAY }),
        key({ id: 4, name: '试用', user_id: 3, max_cost: 1, endpoint_families: ['video'] }),
    ];
    const spent = (id) => (id === 4 ? 2 : 0);
    const owner = (id) => (id === 2 ? 'chen.wei' : 'xu.na');
    const all = { ...f.EMPTY_KEY_FILTERS };
    assert.deepEqual(f.keyStatusCounts(keys, all, spent, NOW, owner), { all: 4, ok: 1, off: 1, expired: 1, used_up: 1 });
    assert.deepEqual(f.filterKeys(keys, { ...all, status: 'used_up' }, spent, NOW, owner).map((k) => k.id), [4]);
    assert.deepEqual(f.filterKeys(keys, { ...all, search: 'chen' }, spent, NOW, owner).map((k) => k.id), [1, 2]);
    assert.deepEqual(f.filterKeys(keys, { ...all, search: 'hm4q' }, spent, NOW, owner).length, 4, '按末 4 位搜');
    assert.deepEqual(f.filterKeys(keys, { ...all, search: '#3' }, spent, NOW, owner).map((k) => k.id), [3]);
    assert.deepEqual(f.filterKeys(keys, { ...all, owners: [3] }, spent, NOW, owner).map((k) => k.id), [3, 4]);
    assert.deepEqual(f.filterKeys(keys, { ...all, families: ['video'] }, spent, NOW, owner).map((k) => k.id), [1, 2, 3, 4]);
    assert.deepEqual(f.filterKeys(keys, { ...all, families: ['gemini'] }, spent, NOW, owner).map((k) => k.id), [1, 2, 3]);
    const c = f.keyStatusCounts(keys, { ...all, status: 'off', owners: [3] }, spent, NOW, owner);
    assert.equal(c.all, 2);
    assert.equal(c.off, 0);
});

test('已用 / 上限：80% 起算快用完，超过算用完；0 = 不限', () => {
    assert.equal(f.spendState(5, 0).limited, false);
    assert.equal(f.spendState(79, 100).level, 'ok');
    assert.equal(f.spendState(80, 100).level, 'near');
    assert.equal(f.spendState(101, 100).level, 'over');
    assert.equal(f.spendState(101, 100).pct, 100);
    assert.equal(f.spendState(30, 100).left, 70);
});

test('有效期：永久 / 还剩 N 天 / 过期 N 天；快捷按钮能认回来', () => {
    assert.deepEqual(f.expiryState(0, NOW), { kind: 'forever' });
    assert.deepEqual(f.expiryState(NOW + 3 * DAY, NOW), { kind: 'left', days: 3, soon: true });
    assert.deepEqual(f.expiryState(NOW + 40 * DAY, NOW), { kind: 'left', days: 40, soon: false });
    assert.deepEqual(f.expiryState(NOW - 2 * DAY, NOW), { kind: 'past', days: 2 });
    const plus30 = f.expireAfterDays(30, NOW);
    assert.ok(plus30 > NOW + 29 * DAY && plus30 <= NOW + 31 * DAY);
    assert.equal(f.matchExpirePreset(plus30, [1, 30, 90], NOW), 30);
    assert.equal(f.matchExpirePreset(0, [1, 30, 90], NOW), 0);
    assert.equal(f.matchExpirePreset(plus30 + 60, [1, 30, 90], NOW), null);
    assert.equal(f.localInputToUnix(f.unixToLocalInput(plus30)), plus30);
    assert.equal(f.normalizeMoneyInput('$1,0.5.0'), '10.50');
});

test('接入说明：各客户端的片段', () => {
    const base = { baseUrl: 'https://octo.example', apiKey: 'sk-FULL', models: [], shell: 'unix' };
    const claude = g.buildGuide('claude', { ...base, plan: 'svip' });
    assert.match(claude[0].code, /export ANTHROPIC_BASE_URL="https:\/\/octo\.example"/);
    assert.match(claude[0].code, /export ANTHROPIC_AUTH_TOKEN="sk-FULL"/);
    assert.match(claude[0].code, /ANTHROPIC_CUSTOM_HEADERS="X-Octopus-Plan: svip"/);
    assert.equal(JSON.parse(claude[1].code).env.ANTHROPIC_AUTH_TOKEN, 'sk-FULL');
    const ps = g.buildGuide('gemini', { ...base, shell: 'powershell' });
    assert.match(ps[0].code, /^\$env:GOOGLE_GEMINI_BASE_URL="https:\/\/octo\.example"$/m);
    assert.match(ps[0].code, /^\$env:GEMINI_API_KEY="sk-FULL"$/m);
    const codex = g.buildGuide('codex', { ...base, models: ['claude-x', 'gpt-5.1-codex'] });
    assert.match(codex[0].code, /base_url = "https:\/\/octo\.example\/v1"/);
    assert.match(codex[0].code, /wire_api = "responses"/);
    assert.match(codex[0].code, /model = "gpt-5.1-codex"/, '限定模型时挑名字对得上的');
    assert.match(codex[1].code, /OCTOPUS_API_KEY="sk-FULL"/);
    assert.doesNotMatch(codex[0].code, /sk-FULL/, 'Key 不写进 config.toml，走环境变量');
    const video = g.buildGuide('video', base);
    assert.equal(video.length, 2);
    assert.match(video[1].code, /\/v1\/videos\/<id>/);
    assert.equal(g.pickModel([], 'claude'), 'claude-sonnet-4.5');
    assert.equal(g.pickModel(['only-this'], 'gemini'), 'only-this');
});
