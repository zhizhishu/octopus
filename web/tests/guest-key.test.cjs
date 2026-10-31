const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const g = load('src/components/modules/key/lib/guest-key.ts');
const NOW = 1_800_000_000;
const H = 3600;
const D = 86400;
const key = (over) => ({ id: 1, name: '客人', api_key: 'sk-octopus-AbCdHm4q', enabled: true, expire_at: NOW + D, supported_models: 'gpt-4o', ...over });

test('状态顺序和后端鉴权一样：作废 > 停用 > 过期 > 没设上限 > 用完 > 能用', () => {
    assert.equal(g.guestStatus(key({ revoked_at: NOW - 1, enabled: false, expire_at: NOW - 1 }), 0, NOW), 'revoked');
    assert.equal(g.guestStatus(key({ enabled: false, expire_at: NOW - 1 }), 0, NOW), 'off');
    assert.equal(g.guestStatus(key({ expire_at: NOW - 1, max_cost: 1 }), 5, NOW), 'expired');
    assert.equal(g.guestStatus(key({ max_cost: 0 }), 0, NOW), 'no_cap');
    assert.equal(g.guestStatus(key({}), 0, NOW), 'no_cap');
    assert.equal(g.guestStatus(key({ max_cost: 1 }), 1.01, NOW), 'used_up');
    // 与后端一致：花到上限（等于）就拒绝。
    assert.equal(g.guestStatus(key({ max_cost: 1 }), 1, NOW), 'used_up');
    assert.equal(g.guestStatus(key({ max_cost: 1 }), 0.99, NOW), 'ok');
});

test('有效期预设：从现在起算；自定义不给值', () => {
    assert.equal(g.presetExpireAt('1h', NOW), NOW + H);
    assert.equal(g.presetExpireAt('30d', NOW), NOW + 30 * D);
    assert.equal(g.presetExpireAt('custom', NOW), null);
    assert.ok(g.GUEST_EXPIRE_PRESETS.every((p) => p.sec <= g.GUEST_MAX_DAYS * D), '预设不能超过最长期限');
});

test('剩余时长按最大单位显示：几小时的访客 Key 不显示「0 天」', () => {
    assert.deepEqual(g.timeLeft(NOW + 3 * H + 10, NOW), { unit: 'hours', n: 3, past: false });
    assert.deepEqual(g.timeLeft(NOW + 2 * D, NOW), { unit: 'days', n: 2, past: false });
    assert.deepEqual(g.timeLeft(NOW + 20, NOW), { unit: 'minutes', n: 1, past: false });
    assert.deepEqual(g.timeLeft(NOW - 2 * H, NOW), { unit: 'hours', n: 2, past: true });
});

test('保存前检查和后端同口径：必须有名称、模型、未来且不超过 365 天的有效期、大于 0 的花费上限', () => {
    const ok = { name: 'a', note: '', models: ['m'], expireAt: NOW + H, cap: '5', enabled: true };
    assert.equal(g.validateGuestDraft(ok, NOW), null);
    assert.equal(g.validateGuestDraft({ ...ok, name: ' ' }, NOW), 'needName');
    assert.equal(g.validateGuestDraft({ ...ok, models: [' '] }, NOW), 'needModel');
    assert.equal(g.validateGuestDraft({ ...ok, expireAt: 0 }, NOW), 'needExpire');
    assert.equal(g.validateGuestDraft({ ...ok, expireAt: NOW - 1 }, NOW), 'pastExpire');
    assert.equal(g.validateGuestDraft({ ...ok, expireAt: NOW + 366 * D }, NOW), 'tooFar');
    assert.equal(g.validateGuestDraft({ ...ok, cap: 'x' }, NOW), 'badCap');
    assert.equal(g.validateGuestDraft({ ...ok, cap: '' }, NOW), 'needCap');
    assert.equal(g.validateGuestDraft({ ...ok, cap: ' ' }, NOW), 'needCap');
    assert.equal(g.validateGuestDraft({ ...ok, cap: '0' }, NOW), 'badCap');
    assert.equal(g.validateGuestDraft({ ...ok, cap: '0.5' }, NOW), null);
    // 已过期的 Key 不改有效期、只改别的：放行（后端同样只拦「新设的」过去时间）。
    assert.equal(g.validateGuestDraft({ ...ok, expireAt: NOW - D }, NOW, false), null);
});

test('搜索：名称 / 备注 / 模型 / Key 末 4 位', () => {
    const keys = [key({ id: 1, name: '张三', note: '展会' }), key({ id: 2, name: 'B', supported_models: 'claude-x,gemini-y', api_key: 'sk-octopus-zzzzWXYZ' })];
    assert.deepEqual(g.guestSearch(keys, '展会').map((k) => k.id), [1]);
    assert.deepEqual(g.guestSearch(keys, 'gemini').map((k) => k.id), [2]);
    assert.deepEqual(g.guestSearch(keys, 'wxyz').map((k) => k.id), [2]);
    assert.equal(g.guestSearch(keys, '').length, 2);
});

test('三种语言都有访客 Key 的文字（key 一样）', () => {
    const flat = (o, p = '') => Object.entries(o).flatMap(([k, v]) => (v && typeof v === 'object' ? flat(v, `${p}${k}.`) : [`${p}${k}`])).sort();
    const read = (f) => JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'public', 'locale', f), 'utf8')).keys;
    const zh = read('zh_hans.json');
    for (const f of ['en.json', 'zh_hant.json']) {
        const other = read(f);
        assert.deepEqual(flat(other.guest), flat(zh.guest), f);
        assert.deepEqual(flat(other.sections), flat(zh.sections), f);
    }
    assert.equal(zh.sections.guest, '访客');
});

test('访客页签只给管理员：普通用户的 Key 页不渲染它', () => {
    const src = fs.readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'key', 'index.tsx'), 'utf8');
    assert.match(src, /isAdmin && section === 'guest' \? <GuestKeys \/>/);
    assert.match(src, /\{isAdmin && \(\s*<div className="px-1" data-key-sections>/);
});
