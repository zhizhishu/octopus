// Run from web/: node --test tests/log-auto-refresh.test.cjs
// 日志页自动刷新（关闭 / 实时 / 5·10·30 秒）：
//   - 选项解析 + 本地记忆的 key
//   - 暂停判定（后台 > 弹窗 > 选字 > 已下滚）
//   - 新日志缓冲去重、合并插入第 1 页顶部
//   - 三种语言都有文案；实时请求面板可折叠且记住状态
/* eslint-disable @typescript-eslint/no-require-imports -- CommonJS test harness, same as the other tests/*.test.cjs */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const ar = load('src/lib/log-auto-refresh.ts');

test('auto refresh: choices, parsing and default off', () => {
    assert.deepEqual([...ar.LOG_AUTO_REFRESH_CHOICES], ['off', 'live', '5s', '10s', '30s']);
    for (const v of ['off', 'live', '5s', '10s', '30s']) assert.equal(ar.parseLogAutoRefresh(v), v);
    assert.equal(ar.parseLogAutoRefresh(' 10s '), '10s');
    for (const v of [null, undefined, '', '3s', 'LIVE', 'true', 5]) assert.equal(ar.parseLogAutoRefresh(v), 'off', String(v));
    assert.equal(ar.LOG_AUTO_REFRESH_STORAGE_KEY, 'octopus.log.autoRefresh');
    assert.equal(ar.LIVE_ACTIVITY_COLLAPSED_STORAGE_KEY, 'octopus.log.liveActivityCollapsed');
});

test('auto refresh: flush cadence and polling fallback', () => {
    assert.equal(ar.logAutoRefreshFlushMs('off'), null);
    assert.equal(ar.logAutoRefreshFlushMs('live'), 0);
    assert.equal(ar.logAutoRefreshFlushMs('5s'), 5000);
    assert.equal(ar.logAutoRefreshFlushMs('10s'), 10000);
    assert.equal(ar.logAutoRefreshFlushMs('30s'), 30000);
    assert.equal(ar.logAutoRefreshPollMs('off'), null);
    // 实时模式断流时也不每秒扫库：最少 5 秒。
    assert.equal(ar.logAutoRefreshPollMs('live'), 5000);
    assert.equal(ar.logAutoRefreshPollMs('30s'), 30000);
});

test('auto refresh: pause reasons and their priority', () => {
    const base = { hidden: false, dialogOpen: false, selecting: false, scrollTop: 0 };
    assert.equal(ar.logAutoRefreshPauseReason(base), null);
    assert.equal(ar.logAutoRefreshPauseReason({ ...base, scrollTop: ar.LOG_AUTO_REFRESH_TOP_SLACK_PX }), null);
    assert.equal(ar.logAutoRefreshPauseReason({ ...base, scrollTop: ar.LOG_AUTO_REFRESH_TOP_SLACK_PX + 1 }), 'scrolled');
    assert.equal(ar.logAutoRefreshPauseReason({ ...base, selecting: true, scrollTop: 400 }), 'selecting');
    assert.equal(ar.logAutoRefreshPauseReason({ ...base, dialogOpen: true, selecting: true }), 'detail');
    assert.equal(ar.logAutoRefreshPauseReason({ hidden: true, dialogOpen: true, selecting: true, scrollTop: 999 }), 'hidden');
});

test('auto refresh: buffer dedupes, keeps newest first and caps size', () => {
    let buf = [];
    buf = ar.bufferLog(buf, { id: 5 });
    buf = ar.bufferLog(buf, { id: 7 });
    buf = ar.bufferLog(buf, { id: 6 });
    buf = ar.bufferLog(buf, { id: 7 });
    assert.deepEqual(buf.map((x) => x.id), [7, 6, 5]);
    const frozen = Object.freeze([{ id: 1 }]);
    assert.deepEqual(ar.bufferLog(frozen, { id: 2 }).map((x) => x.id), [2, 1]);
    let big = [];
    for (let i = 1; i <= 5; i++) big = ar.bufferLog(big, { id: i }, 3);
    assert.deepEqual(big.map((x) => x.id), [5, 4, 3]);
});

test('auto refresh: batch goes on top of page 1, existing rows skipped', () => {
    const pages = [[{ id: 10 }, { id: 9 }], [{ id: 8 }]];
    const r = ar.prependLogBatch(pages, [{ id: 12 }, { id: 10 }, { id: 11 }]);
    assert.deepEqual(r.inserted, [12, 11]);
    assert.deepEqual(r.pages.map((p) => p.map((x) => x.id)), [[12, 11, 10, 9], [8]]);
    // 入参不被改动
    assert.deepEqual(pages[0].map((x) => x.id), [10, 9]);
    const none = ar.prependLogBatch(pages, [{ id: 9 }]);
    assert.deepEqual(none.inserted, []);
    assert.deepEqual(none.pages.map((p) => p.map((x) => x.id)), [[10, 9], [8]]);
    assert.deepEqual(ar.prependLogBatch([], [{ id: 1 }]).pages, [[{ id: 1 }]]);
});

test('auto refresh: all three locales carry the labels', () => {
    const keys = ['label', 'off', 'live', 's5', 's10', 's30', 'running', 'runningLive', 'reconnecting', 'paused',
        'pausedHidden', 'pausedDetail', 'pausedSelecting', 'pausedScrolled', 'newCount', 'showNew', 'onlyFirstPage', 'hint'];
    for (const lang of ['zh_hans', 'zh_hant', 'en']) {
        const json = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${lang}.json`), 'utf8'));
        for (const k of keys) assert.ok(json.log.autoRefresh[k], `${lang} log.autoRefresh.${k}`);
        assert.match(json.log.autoRefresh.newCount, /\{count\}/, lang);
        for (const k of ['collapse', 'expand', 'collapsedRescue', 'collapsedRetrying']) assert.ok(json.log.live[k], `${lang} log.live.${k}`);
    }
});

test('auto refresh: page wiring reuses the existing log SSE and the panel remembers collapse', () => {
    const page = readFileSync(path.join(__dirname, '..', 'src', 'components', 'modules', 'log', 'index.tsx'), 'utf8');
    // 自动刷新走 useLogs 的 live（同一条 /api/v1/log/stream），不另起轮询器。
    assert.match(page, /live: isLiveMode && currentPage === 1/);
    assert.match(page, /livePaused: autoRefresh\.pauseReason !== null/);
    assert.doesNotMatch(page, /setInterval\(/);
    assert.match(page, /LIVE_ACTIVITY_COLLAPSED_STORAGE_KEY/);
    assert.match(page, /aria-expanded=\{!collapsed\}/);
    const hook = readFileSync(path.join(__dirname, '..', 'src', 'api', 'endpoints', 'log.ts'), 'utf8');
    assert.match(hook, /\/api\/v1\/log\/stream\?/);
    assert.match(hook, /bufferLog\(pendingRef\.current, log\)/);
});
