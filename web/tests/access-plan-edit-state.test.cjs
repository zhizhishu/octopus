const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const edit = () => load('src/components/modules/access-plan/edit-state.ts');
const viewport = () => load('src/components/modules/access-plan/canvas-viewport.ts');
const source = () => readFileSync(path.join(__dirname, '../src/components/modules/access-plan/index.tsx'), 'utf8');

const tgt = (id, channel_id, priority) => ({ id, channel_id, upstream_model: 'gpt', priority });
const row = (targets) => ({ requestKey: 'gpt', targets });

// ── 1. 先填满：交互期间不换位 ──

test('frozen order keeps cards where they were while the numbers change', () => {
    const { captureOrder, applyFrozenOrder } = edit();
    const before = [row([tgt(1, 1, 1), tgt(2, 2, 2), tgt(3, 3, 3)])];
    const frozen = captureOrder(before);
    // 用户把 #1 点到 P4：真实排序变成 2,3,1
    const resorted = [row([tgt(2, 2, 2), tgt(3, 3, 3), tgt(1, 1, 4)])];
    const shown = applyFrozenOrder(resorted, frozen);
    assert.deepEqual(shown[0].targets.map((t) => t.id), [1, 2, 3], 'cards stay put under the pointer');
    assert.deepEqual(shown[0].ranks, [3, 1, 2], 'order labels show the real rank');
    assert.equal(shown[0].targets[0].priority, 4, 'the number itself updates live');
});

test('unfrozen rows render the real order; new cards append, removed cards drop out', () => {
    const { captureOrder, applyFrozenOrder } = edit();
    const rows = [row([tgt(2, 2, 1), tgt(1, 1, 2)])];
    assert.equal(applyFrozenOrder(rows, null), rows);
    const frozen = captureOrder([row([tgt(1, 1, 1), tgt(2, 2, 2), tgt(9, 9, 3)])]);
    const shown = applyFrozenOrder([row([tgt(2, 2, 1), tgt(4, 4, 2), tgt(1, 1, 3)])], frozen);
    assert.deepEqual(shown[0].targets.map((t) => t.id), [1, 2, 4]);
    const other = applyFrozenOrder([{ requestKey: 'claude', targets: [tgt(5, 5, 1)] }], frozen);
    assert.deepEqual(other[0].targets.map((t) => t.id), [5]);
    assert.equal(other[0].ranks, undefined);
});

test('card keys survive a save that re-creates rows with new ids', () => {
    const { targetOrderKey, captureOrder, applyFrozenOrder } = edit();
    assert.equal(targetOrderKey({ id: 7, channel_id: 1, upstream_model: 'GPT ' }), targetOrderKey({ id: 99, channel_id: 1, upstream_model: 'gpt' }));
    assert.notEqual(targetOrderKey({ channel_id: 1, upstream_model: 'a' }), targetOrderKey({ channel_id: 2, upstream_model: 'a' }));
    const frozen = captureOrder([row([tgt(1, 1, 1), tgt(2, 2, 2), tgt(3, 3, 3)])]);
    // 保存回包：同样三张卡，id 全换了，#1 被点到 P4
    const shown = applyFrozenOrder([row([tgt(12, 2, 2), tgt(13, 3, 3), tgt(11, 1, 4)])], frozen);
    assert.deepEqual(shown[0].targets.map((t) => t.channel_id), [1, 2, 3]);
});

test('order is released only after save settles, the pointer leaves, and the user stops', () => {
    const { shouldReleaseFrozenOrder, FROZEN_ORDER_IDLE_MS } = edit();
    const at = 10_000;
    const idle = at + FROZEN_ORDER_IDLE_MS;
    assert.equal(shouldReleaseFrozenOrder({ now: at + 100, lastChangeAt: at, saving: false, hovering: false }), false, 'still clicking');
    assert.equal(shouldReleaseFrozenOrder({ now: idle, lastChangeAt: at, saving: true, hovering: false }), false, 'save in flight');
    assert.equal(shouldReleaseFrozenOrder({ now: idle, lastChangeAt: at, saving: false, hovering: true }), false, 'mouse still on the stepper');
    assert.equal(shouldReleaseFrozenOrder({ now: idle, lastChangeAt: at, saving: false, hovering: false }), true);
});

// ── 5. 保存串行、最新的赢 ──

function deferred() {
    let resolve, reject;
    const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
    return { promise, resolve, reject };
}

test('save queue sends one at a time and only the latest result is applied', async () => {
    const { createLatestSaveQueue } = edit();
    const sent = [];
    const calls = [];
    const settled = [];
    const busy = [];
    const q = createLatestSaveQueue(
        (p) => { sent.push(p); const d = deferred(); calls.push(d); return d.promise; },
        (o) => settled.push(o),
        (b) => busy.push(b),
    );
    q.submit('v1');
    q.submit('v2');
    q.submit('v3'); // v2 is replaced: only the newest full list is worth sending
    assert.deepEqual(sent, ['v1']);
    assert.equal(q.busy(), true);
    calls[0].resolve('r1');
    await new Promise((r) => setImmediate(r));
    assert.deepEqual(sent, ['v1', 'v3']);
    assert.equal(settled[0].superseded, true, 'v1 result must not overwrite local state');
    calls[1].resolve('r3');
    await new Promise((r) => setImmediate(r));
    assert.equal(settled.length, 2);
    assert.deepEqual({ ok: settled[1].ok, value: settled[1].value, payload: settled[1].payload, superseded: settled[1].superseded }, { ok: true, value: 'r3', payload: 'v3', superseded: false });
    assert.deepEqual(busy, [true, false]);
    assert.equal(q.busy(), false);
});

test('a failed save that has a newer one queued still sends the newer one', async () => {
    const { createLatestSaveQueue } = edit();
    const sent = [];
    const calls = [];
    const settled = [];
    const q = createLatestSaveQueue((p) => { sent.push(p); const d = deferred(); calls.push(d); return d.promise; }, (o) => settled.push(o));
    q.submit('a');
    q.submit('b');
    calls[0].reject(new Error('boom'));
    await new Promise((r) => setImmediate(r));
    assert.equal(settled[0].ok, false);
    assert.equal(settled[0].superseded, true);
    assert.deepEqual(sent, ['a', 'b']);
    calls[1].reject(new Error('boom2'));
    await new Promise((r) => setImmediate(r));
    assert.equal(settled[1].ok, false);
    assert.equal(settled[1].superseded, false, 'last failure is reported so the editor can roll back');
});

test('drafts resync from the server only when clean and idle', () => {
    const { shouldResyncDraft } = edit();
    const { shouldResyncRouteTargets } = viewport();
    assert.equal(shouldResyncDraft('a', 'b', false, false), true);
    assert.equal(shouldResyncDraft('a', 'b', true, false), false);
    assert.equal(shouldResyncDraft('a', 'b', false, true), false, 'never wipe a half-edited draft');
    assert.equal(shouldResyncDraft('a', 'a', false, false), false);
    assert.equal(shouldResyncRouteTargets('a', 'b', false, true), false);
    assert.equal(shouldResyncRouteTargets('a', 'b', false), true, 'old 3-arg call keeps working');
});

// ── 4. 关窗前问一句 ──

test('dirty detection ignores key order and undefined fields', () => {
    const { sameDraft, decideDialogClose } = edit();
    assert.equal(sameDraft({ a: 1, b: [1, 2], c: undefined }, { b: [1, 2], a: 1 }), true);
    assert.equal(sameDraft([{ id: 1, enabled: true }], [{ id: 1, enabled: false }]), false);
    assert.equal(decideDialogClose(false), 'close');
    assert.equal(decideDialogClose(true), 'confirm');
});

test('unsaved-changes strings exist in en / zh_hans / zh_hant', () => {
    const keys = ['title', 'description', 'keepEditing', 'discard'];
    for (const loc of ['en', 'zh_hans', 'zh_hant']) {
        const dict = JSON.parse(readFileSync(path.join(__dirname, `../public/locale/${loc}.json`), 'utf8')).accessPlan.unsaved;
        for (const k of keys) assert.ok(typeof dict[k] === 'string' && dict[k].length > 0, `${loc}.${k}`);
    }
    const zh = JSON.parse(readFileSync(path.join(__dirname, '../public/locale/zh_hans.json'), 'utf8')).accessPlan.unsaved;
    assert.equal(zh.description, '改动还没保存，确定要关？');
});

// ── 源码守卫 ──

test('canvas source: stable dialog keys, guarded closes, collapsible minimap, mobile height', () => {
    const src = source();
    assert.ok(!/key=\{`[a-z]+-\$\{[a-zA-Z.]+\.id\}-\$\{[a-zA-Z.]+updated_at/.test(src), 'no editor key may include updated_at');
    assert.ok(src.includes('key={`basic-${plan.id}`}'));
    assert.ok(src.includes('key={`billing-${plan.id}`}'));
    assert.ok(src.includes('key={`settings-${selectedPlan.id}`}'));
    assert.equal((src.match(/<UnsavedChangesConfirm/g) || []).length, 3, 'route quick edit, plan basics, plan settings');
    assert.ok(src.includes('data-testid="minimap-toggle"'));
    assert.ok(!src.includes('className="!hidden sm:!block"'), 'minimap is collapsible, not just hidden on phones');
    assert.ok(src.includes('h-[max(360px,calc(100dvh-16rem))]'));
    assert.ok(src.includes('applyFrozenOrder(sortedCanvasRows, frozenOrder)'));
    assert.ok(src.includes('createLatestSaveQueue'));
});

test('route dialog source: two modes, drag order in 优先, numbers in 轮询, touch-safe dialog', () => {
    const src = source();
    assert.ok(src.includes("from '@hello-pangea/dnd'"));
    assert.ok(src.includes('<RouteModeCards value={editingSimpleMode} onChange={setEditingMode} />'));
    assert.ok(src.includes("isDragDisabled={editingSimpleMode !== 'first'}"));
    assert.ok(src.includes('setRotatePriority(group, own, priority)'));
    assert.ok(src.includes('sm:max-w-[min(72rem,calc(100vw-3rem))]'), 'wide dialog on desktop');
    assert.ok(src.includes('top-0 right-0 bottom-0 left-0 m-auto flex h-fit'), 'no translate centering (breaks dnd fixed positioning)');
    assert.ok(src.includes('translate-none'), 'CSS translate (even 0 0) makes the dialog the containing block for position:fixed');
    assert.ok(!src.includes('renderClone='), 'renderClone unmounts the source node and breaks touch drag');
    assert.ok(!src.includes('modeSpreadTiered'), 'only 优先 / 轮询 in the UI');
    assert.ok(src.includes('setEditingRuleField({ fallback_mode:'), 'fallback is rule-level in the dialog (backend keeps only the first target\'s value)');
    assert.ok(!src.includes('setEditingRuleField({ system_prompt_override') && !src.includes('PromptLayerControls'), 'no prompt in the route dialog: prompts are edited only on the 提示词 page');
    assert.ok(!src.includes('onClick={() => removeTarget(index)}'), 'no delete: channel sync re-adds deleted lines (enabled)');
});
