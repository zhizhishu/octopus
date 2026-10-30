const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

// 仪表盘的纯计算（model.ts）：时间段、上一段、合计口径、涨跌、趋势点。
const m = load('src/components/modules/dashboard/model.ts', {});

test('presets are anchored on the stats-timezone today, inclusive', () => {
    const today = '2026-10-30';
    assert.deepEqual(m.presetRange('today', today), { from: today, to: today });
    assert.deepEqual(m.presetRange('yesterday', today), { from: '2026-10-29', to: '2026-10-29' });
    assert.deepEqual(m.presetRange('7d', today), { from: '2026-10-24', to: today });
    assert.deepEqual(m.presetRange('30d', today), { from: '2026-10-01', to: today });
    assert.deepEqual(m.presetRange('month', today), { from: '2026-10-01', to: today });
    assert.equal(m.rangeDays(m.presetRange('7d', today)), 7);
    assert.equal(m.rangeDays(m.presetRange('30d', today)), 30);
    // 跨年、跨闰日。
    assert.deepEqual(m.presetRange('7d', '2028-03-02'), { from: '2028-02-25', to: '2028-03-02' });
    assert.deepEqual(m.presetRange('yesterday', '2027-01-01'), { from: '2026-12-31', to: '2026-12-31' });
});

test('previous period is the same length right before', () => {
    assert.deepEqual(m.previousRange({ from: '2026-10-24', to: '2026-10-30' }), { from: '2026-10-17', to: '2026-10-23' });
    assert.deepEqual(m.previousRange({ from: '2026-10-30', to: '2026-10-30' }), { from: '2026-10-29', to: '2026-10-29' });
    assert.deepEqual(m.previousRange({ from: '2026-10-01', to: '2026-10-30' }), { from: '2026-09-01', to: '2026-09-30' });
});

test('custom range validation mirrors the backend bounds', () => {
    const today = '2026-10-30';
    assert.equal(m.validateRange({ from: '2026-08-02', to: today }, today), null); // 正好 90 天
    assert.equal(m.validateRange({ from: '2026-08-01', to: today }, today), 'tooLong');
    assert.equal(m.validateRange({ from: today, to: '2026-10-31' }, today), 'future');
    assert.equal(m.validateRange({ from: '2026-10-30', to: '2026-10-29' }, today), 'order');
    assert.equal(m.validateRange({ from: '2026-02-30', to: today }, today), 'format');
    assert.equal(m.validateRange({ from: '', to: today }, today), 'format');
    const quick = m.quickRanges(today);
    assert.deepEqual(quick.lastMonth, { from: '2026-09-01', to: '2026-09-30' });
    assert.equal(m.rangeDays(quick.last90), 90);
    for (const r of Object.values(quick)) assert.equal(m.validateRange(r, today), null);
    assert.equal(m.matchPreset({ from: '2026-10-24', to: today }, today), '7d');
    assert.equal(m.matchPreset({ from: '2026-10-23', to: today }, today), 'custom');
});

test('hourly view only for a single day', () => {
    assert.equal(m.effectiveGranularity({ from: '2026-10-30', to: '2026-10-30' }, 'hour'), 'hour');
    assert.equal(m.effectiveGranularity({ from: '2026-10-29', to: '2026-10-30' }, 'hour'), 'day');
    assert.equal(m.effectiveGranularity({ from: '2026-10-30', to: '2026-10-30' }, 'day'), 'day');
});

const row = (date, o = {}) => ({
    date, input_token: 0, output_token: 0, input_cost: 0, output_cost: 0, wait_time: 0,
    request_success: 0, request_failed: 0, cache_hit_token: 0, cache_write_token: 0, cache_input_token: 0, ...o,
});

test('daily series fills gaps, uses the live today row, and sums like the home page', () => {
    const daily = [
        row('20261028', { request_success: 9, request_failed: 1, input_cost: 1, output_cost: 0.5, input_token: 100, output_token: 20, cache_hit_token: 30, cache_input_token: 100, wait_time: 5000 }),
        row('20261030', { request_success: 1, input_cost: 99 }), // 库里「今天」那行是旧的
    ];
    const todayRow = row('20261030', { request_success: 4, input_cost: 2, input_token: 50, output_token: 5, wait_time: 2000 });
    const series = m.dailySeries({ from: '2026-10-28', to: '2026-10-30' }, daily, '2026-10-30', todayRow);
    assert.equal(series.length, 3);
    assert.equal(series[1].success, 0);
    assert.equal(series[2].cost, 2);
    const s = m.summarize(series);
    assert.equal(s.requests, 14);
    assert.equal(s.success, 13);
    assert.equal(s.cost, 3.5);
    assert.equal(s.tokens, 175); // 120 + 55
    assert.equal(s.output, 25);
    assert.equal(s.cacheHit, 30);
    assert.equal(s.cacheRate, 30 / 150); // 分母：100 + 50
    assert.equal(s.avgMs, 7000 / 14);
    const empty = m.summarize([]);
    assert.equal(empty.successRate, null);
    assert.equal(empty.cacheRate, null);
    assert.equal(empty.avgMs, null);
});

test('log usage uses total_cost; token total counts cache the same way as home', () => {
    const u = m.fromLog({ request_success: 1, request_failed: 1, input_token: 10, output_token: 5, cache_hit_token: 80, cache_write_token: 20, cache_input_token: 0, total_cost: 0.25, wait_time: 100 });
    assert.equal(u.cost, 0.25);
    assert.equal(m.cacheRateBase(u), 100);
    assert.equal(m.totalTokens(u), 105);
});

test('change vs previous period; nothing before means no comparison', () => {
    assert.equal(m.pctChange(112, 100), 0.12);
    assert.equal(m.pctChange(96, 100), -0.04);
    assert.equal(m.pctChange(5, 0), null);
    assert.equal(m.pctChange(0, 0), null);
});

test('trend points: previous period dashed, today cut at the current hour', () => {
    const cur = [m.fromLog({ request_success: 2, request_failed: 0, input_token: 0, output_token: 0, cache_hit_token: 0, cache_write_token: 0, cache_input_token: 0, total_cost: 1, wait_time: 0 }),
        m.fromLog({ request_success: 1, request_failed: 1, input_token: 0, output_token: 0, cache_hit_token: 0, cache_write_token: 0, cache_input_token: 0, total_cost: 2, wait_time: 0 }),
        m.fromLog(null)];
    const prev = [m.fromLog(null), m.fromLog(null), m.fromLog({ request_success: 3, request_failed: 1, input_token: 0, output_token: 0, cache_hit_token: 0, cache_write_token: 0, cache_input_token: 0, total_cost: 4, wait_time: 0 })];
    const pts = m.buildTrend(['0:00', '1:00', '2:00'], cur, prev, 'cost', 1);
    assert.deepEqual(pts.map((p) => p.current), [1, 2, null]);
    assert.deepEqual(pts.map((p) => p.previous), [0, 0, 4]);
    const ok = m.buildTrend(['a', 'b', 'c'], cur, prev, 'success');
    assert.deepEqual(ok.map((p) => p.current), [100, 50, null]);
    assert.deepEqual(ok.map((p) => p.previous), [null, null, 75]);
    assert.equal(m.currentHourIndex([100, 200, 300], 250), 1);
    assert.equal(m.currentHourIndex([100, 200, 300], 50), -1);
});

test('share slices: top items plus one "others", shares sum to 1', () => {
    const slices = m.shareSlices(
        [{ key: 'a', name: 'a', total_cost: 6 }, { key: 'b', name: 'b', total_cost: 3 }],
        { total_cost: 1 }, 4, 10,
    );
    assert.equal(slices.length, 3);
    assert.equal(slices[2].isOthers, true);
    assert.equal(slices[2].othersCount, 4);
    assert.ok(Math.abs(slices.reduce((s, x) => s + x.share, 0) - 1) < 1e-12);
    assert.equal(m.shareSlices([], undefined, 0, 0).length, 0);
});

test('cleaned-logs notice only when the range starts before the oldest log', () => {
    assert.equal(m.logsCleanedBefore(1000, 2000), true);
    assert.equal(m.logsCleanedBefore(2000, 1000), false);
    assert.equal(m.logsCleanedBefore(1000, 1000), false); // 最早一条正好在开头：没缺

    assert.equal(m.logsCleanedBefore(1000, 0), false);
    assert.equal(m.logsCleanedBefore(undefined, 5), false);
});
