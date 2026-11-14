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
