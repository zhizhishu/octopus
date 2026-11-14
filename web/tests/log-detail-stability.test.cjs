// Run on Linux: node --test tests/log-detail-stability.test.cjs
// Exercise the production state machine, not a duplicate fake implementation.
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');
const auto = load('src/lib/log-auto-refresh.ts');
const live = load('src/lib/log-live-buffer.ts');
const source = (p) => readFileSync(path.join(__dirname, '..', p), 'utf8');

test('detail: freeze is synchronous, and closing without explicit resume remains frozen', () => {
    let state = live.INITIAL_LIVE_BUFFER_STATE;
    let pending = [];
    const opened = live.freezeLiveBuffer({ state, buffer: pending });
    state = opened.state;
    pending = opened.buffer;
    assert.equal(state.frozen, true);
    assert.deepEqual(opened.insert, []);
    const received = live.receiveLiveLog({ state, buffer: pending, log: { id: 10 }, live: true, flushMs: 0, paused: false });
    assert.deepEqual(received.insert, []);
    assert.deepEqual(received.buffer.map((r) => r.id), [10]);
    const closed = live.tickLiveBuffer({ state: received.state, buffer: received.buffer, live: true, paused: false });
    assert.deepEqual(closed.insert, [], 'closing cannot flush an already-frozen backlog');
    assert.equal(closed.state.frozen, true);
});

test('detail: 300 SSE events keep only 200, report overflow, and never flush while frozen', () => {
    let state = live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state;
    let pending = [];
    for (let id = 1; id <= 300; id++) {
        const step = live.receiveLiveLog({ state, buffer: pending, log: { id }, live: true, flushMs: 0, paused: false });
        state = step.state;
        pending = step.buffer;
        assert.deepEqual(step.insert, []);
    }
    assert.equal(pending.length, auto.LOG_AUTO_REFRESH_BUFFER_MAX);
    assert.equal(pending[0].id, 300);
    assert.equal(pending[pending.length - 1].id, 101);
    assert.equal(state.overflowed, true);
    assert.equal(live.pendingUnreadLabel(pending.length, state.overflowed), '200+');
    const dedup = live.receiveLiveLog({ state, buffer: pending, log: { id: 300 }, live: true, flushMs: 0, paused: false });
    assert.equal(dedup.buffer.length, 200);
    assert.equal(live.pendingWouldDropOldest(pending, { id: 300 }), false);
});

test('detail: a failed GET retains the freeze and pending data, successful retry merges once and unfreezes', () => {
    let state = live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state;
    let pending = [];
    const first = live.receiveLiveLog({ state, buffer: pending, log: { id: 18 }, live: true, flushMs: 0, paused: false });
    state = first.state; pending = first.buffer;
    const started = live.beginResume({ state, buffer: pending, key: 'page1' });
    assert.ok(started);
    state = started.state;
    assert.equal(live.beginResume({ state, buffer: pending, key: 'page1' }), null, 'in-flight GET is single-flight');
    // SSE arriving after GET starts still belongs to the same merge.
    const later = live.receiveLiveLog({ state, buffer: pending, log: { id: 19 }, live: true, flushMs: 0, paused: false });
    state = later.state; pending = later.buffer;
    const failed = live.settleResume({ state, buffer: pending, ok: false, currentKey: 'page1' });
    state = failed.state; pending = failed.buffer;
    assert.equal(state.frozen, true);
    assert.equal(state.resuming, false);
    assert.deepEqual(pending.map((x) => x.id), [19, 18]);
    const retry = live.beginResume({ state, buffer: pending, key: 'page1' });
    assert.ok(retry);
    const success = live.settleResume({ state: retry.state, buffer: retry.buffer, ok: true, currentKey: 'page1' });
    assert.equal(success.state.frozen, false);
    assert.deepEqual(success.buffer, []);
    assert.deepEqual(success.insert.map((x) => x.id), [19, 18]);
    assert.equal(success.state.overflowed, false);
});

test('detail: switching filters invalidates an in-flight resume', () => {
    const started = live.beginResume({
        state: live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state,
        buffer: [{ id: 12 }], key: 'old'
    });
    const result = live.settleResume({ state: started.state, buffer: started.buffer, ok: true, currentKey: 'new' });
    assert.deepEqual(result.insert, []);
    assert.deepEqual(result.buffer, []);
    assert.equal(result.state.frozen, false);
});

// 真实竞态：用户点完「N 条新日志」又去点开一条（恢复 GET 还在飞）。
// 这一次的 GET 落地后绝不能解冻、更不能把积压插到用户正在读的列表背后；
// 它只能把 busy 收掉，闩和缓冲留给下一次显式恢复。
test('detail: opening a detail during a slow resume cancels that resume (no late unfreeze, no backlog insert)', () => {
    let state = live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state;
    const first = live.receiveLiveLog({ state, buffer: [], log: { id: 5 }, live: true, flushMs: 0, paused: false });
    state = first.state;
    const started = live.beginResume({ state, buffer: first.buffer, key: 'page1' });
    assert.ok(started);
    state = started.state;
    // GET 期间 SSE 继续收（先进缓冲，不插列表）。
    const later = live.receiveLiveLog({ state, buffer: started.buffer, log: { id: 6 }, live: true, flushMs: 0, paused: false });
    state = later.state;
    assert.deepEqual(later.insert, []);
    // 用户在这段慢 GET 里点开了详情：闩压过在飞的恢复。
    const refrozen = live.freezeLiveBuffer({ state, buffer: later.buffer });
    state = refrozen.state;
    const pending = refrozen.buffer;
    assert.equal(state.frozen, true);
    assert.equal(state.resuming, true, 'the GET really is still in flight, so busy stays honest');
    // 慢 GET 现在成功了。
    const settled = live.settleResume({ state, buffer: pending, ok: true, currentKey: 'page1' });
    assert.equal(settled.state.frozen, true, 'a late success must not unfreeze behind the open dialog');
    assert.equal(settled.state.resuming, false);
    assert.deepEqual(settled.insert, [], 'and must not push the backlog into the list');
    assert.deepEqual(settled.buffer.map((x) => x.id), [6, 5], 'backlog is kept for the next explicit resume');
    // 关掉详情后用户再点一次，缓冲仍在、可以真恢复。
    const retry = live.beginResume({ state: settled.state, buffer: settled.buffer, key: 'page1' });
    assert.ok(retry, 'explicit resume is still available afterwards');
    const done = live.settleResume({ state: retry.state, buffer: retry.buffer, ok: true, currentKey: 'page1' });
    assert.equal(done.state.frozen, false);
    assert.deepEqual(done.insert.map((x) => x.id), [6, 5]);
});

// 恢复成功只提交一次：GET 与积压重叠时按 id 去重，而且后续 tick 不会重插。
test('detail: a successful resume commits once — overlap with the GET is deduped, later ticks insert nothing', () => {
    let state = live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state;
    let pending = [];
    for (const id of [18, 19]) {
        const step = live.receiveLiveLog({ state, buffer: pending, log: { id }, live: true, flushMs: 0, paused: false });
        state = step.state;
        pending = step.buffer;
    }
    const started = live.beginResume({ state, buffer: pending, key: 'page1' });
    const settled = live.settleResume({ state: started.state, buffer: started.buffer, ok: true, currentKey: 'page1' });
    // 后端落库比 SSE 慢一步：GET 已经含有 18（也可能含 19）。
    const pages = [[{ id: 18 }, { id: 17 }], [{ id: 16 }]];
    const merged = auto.prependLogBatch(pages, settled.insert);
    assert.deepEqual(merged.inserted, [19], 'only the id the GET did not have is inserted');
    // 解冻之后的 tick：缓冲已清空，不会再插一次。
    const after = live.tickLiveBuffer({ state: settled.state, buffer: settled.buffer, live: true, paused: false });
    assert.deepEqual(after.insert, [], 'no second commit of the same batch');
    assert.deepEqual(after.buffer, []);
});

// 慢 GET 期间定时器照样在跑：闩没解开就一拍都不能插。
test('detail: timer ticks during a slow resume never insert', () => {    const started = live.beginResume({
        state: live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state,
        buffer: [{ id: 3 }, { id: 2 }], key: 'page1'
    });
    assert.ok(started);
    for (let i = 0; i < 5; i++) {
        const tick = live.tickLiveBuffer({ state: started.state, buffer: started.buffer, live: true, paused: false });
        assert.deepEqual(tick.insert, []);
        assert.equal(tick.buffer.length, 2, 'backlog is untouched by ticks');
    }
});

// 从「已往下翻」的暂停点恢复（列表本来没冻结）：GET 期间必须也冻住。
// 否则滚回顶部后暂停原因消失，SSE 会先插行，而 refetch 落地会整份替换缓存——
// 刚插进去、还没落库的那几行就这梓没了。
test('detail: resuming from a scrolled pause freezes for the whole GET', () => {
    let state = live.INITIAL_LIVE_BUFFER_STATE;
    let pending = [];
    for (const id of [8, 9]) {
        const step = live.receiveLiveLog({ state, buffer: pending, log: { id }, live: true, flushMs: 0, paused: true });
        state = step.state;
        pending = step.buffer;
        assert.deepEqual(step.insert, []);
    }
    assert.equal(state.frozen, false, 'a scrolled pause alone is not a freeze');
    const started = live.beginResume({ state, buffer: pending, key: 'page1' });
    assert.ok(started);
    assert.equal(started.state.frozen, true, 'the recovery GET freezes even when the list was not frozen');
    const arriving = live.receiveLiveLog({
        state: started.state, buffer: started.buffer, log: { id: 10 }, live: true, flushMs: 0, paused: false
    });
    assert.deepEqual(arriving.insert, [], 'rows arriving during the GET must not be inserted before it lands');
    const flushed = live.tickLiveBuffer({ state: arriving.state, buffer: arriving.buffer, live: true, paused: false });
    assert.deepEqual(flushed.insert, [], 'the timer cannot sneak them in either');
    const settled = live.settleResume({ state: arriving.state, buffer: arriving.buffer, ok: true, currentKey: 'page1' });
    assert.equal(settled.state.frozen, false);
    assert.deepEqual(settled.insert.map((x) => x.id), [10, 9, 8], 'everything buffered during the GET merges after it commits');
});

// 失败一次后溢出标记与「200+」不变，重试成功才清。
test('detail: overflow survives a failed resume and the retry clears it', () => {
    let state = live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state;
    let pending = [];
    for (let id = 1; id <= 205; id++) {
        const step = live.receiveLiveLog({ state, buffer: pending, log: { id }, live: true, flushMs: 0, paused: false });
        state = step.state;
        pending = step.buffer;
    }
    assert.equal(state.overflowed, true);
    const started = live.beginResume({ state, buffer: pending, key: 'page1' });
    const failed = live.settleResume({ state: started.state, buffer: started.buffer, ok: false, currentKey: 'page1' });
    assert.equal(failed.state.overflowed, true, 'a failed GET cannot pretend the backlog is complete');
    assert.equal(failed.buffer.length, auto.LOG_AUTO_REFRESH_BUFFER_MAX);
    assert.equal(live.pendingUnreadLabel(failed.buffer.length, failed.state.overflowed), '200+');
    assert.equal(failed.state.frozen, true);
    const retry = live.beginResume({ state: failed.state, buffer: failed.buffer, key: 'page1' });
    const ok = live.settleResume({ state: retry.state, buffer: retry.buffer, ok: true, currentKey: 'page1' });
    assert.equal(ok.state.overflowed, false);
    assert.equal(ok.insert.length, auto.LOG_AUTO_REFRESH_BUFFER_MAX);
});

// 已经没有恢复在飞（reset 已跑过/被作废过）时，迟到的 settle 不能动任何东西。
test('detail: a settle with no resume in flight changes nothing', () => {
    const state = live.freezeLiveBuffer({ state: live.INITIAL_LIVE_BUFFER_STATE, buffer: [] }).state;
    const stale = live.settleResume({ state, buffer: [{ id: 9 }], ok: true, currentKey: 'page1' });
    assert.equal(stale.state, state);
    assert.deepEqual(stale.insert, []);
    assert.deepEqual(stale.buffer.map((x) => x.id), [9], 'the backlog is not silently dropped');
    assert.equal(stale.state.frozen, true);
});

// 合并节奏 > 0（批量插入）时：不暂停也先进缓冲，到点一次整批插入，且冻结时一拍不插。
test('detail: batched cadence buffers until a tick, then flushes as one batch', () => {
    let state = live.INITIAL_LIVE_BUFFER_STATE;
    let pending = [];
    for (const id of [1, 2, 3]) {
        const step = live.receiveLiveLog({ state, buffer: pending, log: { id }, live: true, flushMs: 1500, paused: false });
        state = step.state;
        pending = step.buffer;
        assert.deepEqual(step.insert, [], 'flushMs > 0 never inserts on arrival');
    }
    const frozen = live.freezeLiveBuffer({ state, buffer: pending });
    assert.deepEqual(live.tickLiveBuffer({ state: frozen.state, buffer: frozen.buffer, live: true, paused: false }).insert, []);
    const opened = live.tickLiveBuffer({ state: state, buffer: pending, live: true, paused: false });
    assert.deepEqual(opened.insert.map((x) => x.id), [3, 2, 1], 'one batch, newest first');
    assert.deepEqual(opened.buffer, []);
});

test('detail: same-id updates preserve the dialog snapshot, navigation changes it', () => {
    const first = { id: 42, response_content: 'old' };
    const newData = { id: 42, response_content: 'new' };
    assert.strictEqual(live.nextDetailSnapshot(first, 42, [newData]), first);
    assert.strictEqual(live.nextDetailSnapshot(first, null, []), first);
    const next = { id: 41 };
    assert.strictEqual(live.nextDetailSnapshot(first, 41, [first, next]), next);
});

test('detail: page-level portal and hook connect the real shared state-machine', () => {
    const page = source('src/components/modules/log/index.tsx');
    const item = source('src/components/modules/log/Item.tsx');
    const hook = source('src/api/endpoints/log.ts');
    const status = source('src/components/modules/log/AutoRefreshControl.tsx');
    assert.match(page, /freezeLiveInsert\(\);[\s\S]{0,200}setDetailSnapshot/);
    assert.match(page, /detailSnapshot && \(\s*<LogCard[\s\S]{0,200}detailOnly/);
    assert.doesNotMatch(page, /detailOpen=\{detailActiveId === log\.id\}/);
    assert.match(item, /<MorphingDialog open=\{false\} onOpenChange=\{handleDetailOpenChange\}>/);
    assert.match(item, /<MorphingDialog open=\{Boolean\(detailOpen\)\} onOpenChange=\{handleDetailOpenChange\}>/);
    assert.match(item, /data-log-id=\{log\.id\}/);
    assert.match(hook, /import \{[\s\S]*receiveLiveLog[\s\S]*settleResume[\s\S]*\} from '@\/lib\/log-live-buffer'/);
    assert.match(hook, /const freeze = useCallback\(\(\) => \{/);
    assert.match(hook, /const resume = useCallback\(async \(\) => \{/);
    assert.match(hook, /const ok = await refresh\(\)/);
    assert.match(hook, /if \(wasReconnecting && !liveStateRef\.current\.frozen/);
    assert.match(status, /pendingUnreadLabel/);
});
