// Run from web/: node --test tests/log-debug-mode.test.cjs
// 日志模式（普通 / 调试）：
//   - setting/log-mode.ts 与后端 op/log_mode.go 同一套读法与到期计算
//   - log/debug-info.ts 调试详情的展示规则（优先/轮询的名字、毫秒、换家链色调）
//   - 三种语言都有设置页文案
/* eslint-disable @typescript-eslint/no-require-imports -- CommonJS test harness, same as the other tests/*.test.cjs */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const mode = load('src/components/modules/setting/log-mode.ts');
const debug = load('src/components/modules/log/debug-info.ts');

test('log mode: only "debug" means debug; everything else is normal', () => {
    assert.equal(mode.parseLogMode('debug'), 'debug');
    assert.equal(mode.parseLogMode(' debug '), 'debug');
    for (const v of ['normal', '', undefined, 'DEBUG', 'true', 'verbose']) assert.equal(mode.parseLogMode(v), 'normal', String(v));
});

test('log mode: auto-off hours default to 24 and stay within 0..168', () => {
    assert.equal(mode.parseAutoOffHours('6'), 6);
    assert.equal(mode.parseAutoOffHours('0'), 0);
    assert.equal(mode.parseAutoOffHours('168'), 168);
    for (const v of ['169', '-1', '1.5', 'abc', '', undefined]) assert.equal(mode.parseAutoOffHours(v), 24, String(v));
    assert.deepEqual([...mode.LOG_DEBUG_AUTO_OFF_CHOICES], [1, 6, 24, 72, 168, 0]);
    assert.equal(mode.autoOffChoiceKey(1), '1h');
    assert.equal(mode.autoOffChoiceKey(24), '24h');
    assert.equal(mode.autoOffChoiceKey(72), '3d');
    assert.equal(mode.autoOffChoiceKey(168), '7d');
    assert.equal(mode.autoOffChoiceKey(0), 'never');
});

test('log mode: expiry = since + hours, null when either is 0 (same rule as the backend)', () => {
    assert.equal(mode.logDebugExpiresAtMs('1800000000', 24), (1800000000 + 24 * 3600) * 1000);
    assert.equal(mode.logDebugExpiresAtMs('0', 24), null);
    assert.equal(mode.logDebugExpiresAtMs('1800000000', 0), null);
    assert.equal(mode.logDebugExpiresAtMs(undefined, 24), null);
    const now = 1_000_000;
    assert.deepEqual(mode.remainingParts(now + 90 * 60000, now), { hours: 1, minutes: 30 });
    assert.deepEqual(mode.remainingParts(now + 1000, now), { hours: 0, minutes: 1 }, 'never shows 0 minutes');
});

test('debug info: route mode names match the literal 优先 / 轮询 wording', () => {
    assert.equal(debug.debugRouteModeLabel('fill_first'), '优先');
    assert.equal(debug.debugRouteModeLabel('spread'), '轮询');
    assert.equal(debug.debugRouteStageLabel('primary'), '正常选路');
    assert.equal(debug.debugRouteStageLabel('fallback'), '回退池');
});

test('debug info: ms formatting, -1 means not measured', () => {
    assert.equal(debug.debugMs(-1), '—');
    assert.equal(debug.debugMs(undefined), '—');
    assert.equal(debug.debugMs(340), '340 ms');
    assert.equal(debug.debugMs(1530), '1.53 s');
    assert.equal(debug.debugMs(12500), '12.5 s');
});

test('debug info: call tone and presence', () => {
    assert.equal(debug.debugCallTone(200), 'ok');
    assert.equal(debug.debugCallTone(503), 'warn');
    assert.equal(debug.debugCallTone(429), 'warn');
    assert.equal(debug.debugCallTone(401), 'error');
    assert.equal(debug.debugCallTone(undefined, 'dial tcp: refused'), 'error');
    assert.equal(debug.hasDebugInfo(undefined), false);
    assert.equal(debug.hasDebugInfo({ timing: {} }), false);
    assert.equal(debug.hasDebugInfo({ timing: {}, calls: [{ channel_id: 1 }] }), true);
    assert.deepEqual(debug.splitRouteTrace('2 candidates | #0 ch=1 pri=1 | #1 ch=2 pri=2'), ['2 candidates', '#0 ch=1 pri=1', '#1 ch=2 pri=2']);
    const rows = debug.debugTimingRows({ timing: { total_ms: 900, before_send_ms: 3, header_wait_ms: 400, first_token_ms: -1, generation_ms: -1, upstream_calls: 2 } });
    assert.deepEqual(rows.map((r) => r.value), ['900 ms', '3 ms', '400 ms', '—', '—', '2 次']);
});

test('locale: log mode strings exist in all three languages', () => {
    for (const lang of ['zh_hans', 'zh_hant', 'en']) {
        const m = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${lang}.json`), 'utf8')).setting.log.mode;
        for (const k of ['label', 'normal', 'debug', 'normalHint', 'debugHint', 'autoOff', 'offInHours', 'offInMinutes', 'neverOff', 'switchedDebug', 'switchedNormal']) {
            assert.ok(m?.[k], `${lang}: log.mode.${k}`);
        }
        for (const c of mode.LOG_DEBUG_AUTO_OFF_CHOICES) {
            assert.ok(m.choice?.[mode.autoOffChoiceKey(c)], `${lang}: log.mode.choice.${mode.autoOffChoiceKey(c)}`);
        }
    }
    const zh = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', 'zh_hans.json'), 'utf8')).setting.log.mode;
    assert.equal(zh.normal, '普通');
    assert.equal(zh.debug, '调试');
});
