// Run from web/: node --test tests/log-severity-count.test.cjs
//
// 锁两件事：
//   1. 前端分桶和 Go TrimSpace 在空白字符上一致（空格、tab、换行、NBSP），
//      不退回「只剥空格」的 SQL TRIM。
//   2. 空列表的全时段计数不轮询；平时的徽章计数放宽到 60 秒。
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

const { classifyRelayLogSeverity, logSeverityCountQueryOptions, LOG_SEVERITY_COUNT_POLL_MS } = load('src/api/endpoints/log-severity.ts');

const cases = [
    { name: 'first attempt', log: { total_attempts: 1 }, want: 'success' },
    { name: 'zero attempts', log: { total_attempts: 0 }, want: 'success' },
    { name: 'retry', log: { total_attempts: 2 }, want: 'warn' },
    { name: 'error text', log: { error: 'boom', total_attempts: 1 }, want: 'error' },
    { name: 'padded error', log: { error: '  boom  ', total_attempts: 1 }, want: 'error' },
    { name: 'spaces only', log: { error: '   ', total_attempts: 1 }, want: 'success' },
    { name: 'spaces only with retries', log: { error: '   ', total_attempts: 4 }, want: 'warn' },
    { name: 'blank code with retries', log: { error_code: '  ', total_attempts: 2 }, want: 'warn' },
    { name: 'tab and newline', log: { error: '\t\n', total_attempts: 1 }, want: 'success' },
    { name: 'cr lf vt ff', log: { error: '\r\n\v\f', total_attempts: 1 }, want: 'success' },
    { name: 'nbsp', log: { error: '\u00a0', total_attempts: 1 }, want: 'success' },
    { name: 'tab-only code', log: { error_code: '\t', total_attempts: 4 }, want: 'warn' },
    { name: 'status 400 beats retries', log: { error_status: 400, total_attempts: 9 }, want: 'error' },
    { name: 'status 399', log: { error_status: 399, total_attempts: 1 }, want: 'success' },
    { name: 'missing attempts field falls back to attempts length', log: { attempts: [{}, {}] }, want: 'warn' },
    { name: 'zero attempts does not fall back to attempts length', log: { total_attempts: 0, attempts: [{}, {}] }, want: 'success' },
];

for (const c of cases) {
    test(`classify: ${c.name}`, () => {
        assert.equal(classifyRelayLogSeverity(c.log), c.want);
    });
}

test('severity count poll is 60s, and the one-shot query does not poll', () => {
    assert.equal(LOG_SEVERITY_COUNT_POLL_MS, 60_000);
    const polling = logSeverityCountQueryOptions(true);
    assert.equal(polling.refetchInterval, 60_000);
    assert.equal(polling.refetchIntervalInBackground, false);
    assert.equal(polling.refetchOnMount, 'always');

    const once = logSeverityCountQueryOptions(false);
    assert.equal(once.refetchInterval, false);
    assert.equal(once.refetchOnMount, false);
    assert.equal(once.refetchOnWindowFocus, false);
    assert.equal(once.refetchOnReconnect, false);
    assert.equal(once.staleTime, Number.POSITIVE_INFINITY);
});

test('empty-list all-time count opts out of polling', () => {
    const src = readFileSync(path.join(__dirname, '../src/components/modules/log/index.tsx'), 'utf8');
    const hook = src.indexOf('const { data: allTimeCounts');
    assert.notEqual(hook, -1, 'allTimeCounts query was removed');
    const block = src.slice(hook, src.indexOf('});', hook));
    assert.match(block, /poll:\s*false/);
    assert.match(block, /enabled:\s*logPanel === 'requests' && listIsEmpty/);
    assert.doesNotMatch(src, /refetchInterval:\s*15000/);
});
