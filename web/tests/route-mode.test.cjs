const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

// 方案路由只有「优先」「轮询」两种：route-mode.ts 负责界面 ↔ 后端 mode/priority 的换算。
const rm = () => load('src/components/modules/access-plan/route-mode.ts');
const t = (channel_id, priority, mode, extra = {}) => ({ channel_id, priority, mode, enabled: true, ...extra });
const ids = (list) => list.map((x) => x.channel_id);
const levelIds = (levels) => levels.map((level) => ids(level));
const enc = (list) => list.map((x) => [x.channel_id, x.mode, x.priority, x.priority_overridden]);

test('read: backend mode → 优先 / 轮询', () => {
    const { simpleModeOf, ruleModeOf } = rm();
    assert.equal(simpleModeOf(3), 'first');
    assert.equal(simpleModeOf(undefined), 'first');
    assert.equal(simpleModeOf(1), 'rotate');
    assert.equal(simpleModeOf(6), 'rotate');
    assert.equal(ruleModeOf([t(1, 1, undefined), t(2, 1, 6)]), 6);
    assert.equal(ruleModeOf([]), 3);
});

test('轮询 A=3 B=1 C=1: A first, then B and C rotate (matches balancer.SpreadTiered: bigger number first)', () => {
    const { tryLevels, sortByTryOrder, rotatePeers } = rm();
    const group = [t(2, 1, 6), t(1, 3, 6), t(3, 1, 6)];
    assert.deepEqual(ids(sortByTryOrder(group)), [1, 2, 3]);
    assert.deepEqual(levelIds(tryLevels(group)), [[1], [2, 3]]);
    assert.deepEqual(ids(rotatePeers(group[0], group)), [3]);
    assert.deepEqual(ids(rotatePeers(group[1], group)), []);
});

test('优先: smaller number first, ties by channel id (balancer.sortByRoutingPriority)', () => {
    const { tryLevels } = rm();
    assert.deepEqual(levelIds(tryLevels([t(3, 1, 3), t(1, 2, 3), t(2, 1, 3)])), [[2], [3], [1]]);
});

test('mode 1 is 轮询 with every number equal: shown as 1, all rotate together', () => {
    const { tryLevels, displayPriority } = rm();
    const group = [t(1, 5, 1), t(2, 1, 1), t(3, 9, 1)];
    assert.deepEqual(group.map((x) => displayPriority(x, 1)), [1, 1, 1]);
    assert.deepEqual(levelIds(tryLevels(group)), [[1, 2, 3]]);
});

test('turned-off channels are not in the try order', () => {
    const { tryLevels } = rm();
    assert.deepEqual(levelIds(tryLevels([t(1, 3, 6), t(2, 1, 6, { enabled: false }), t(3, 1, 6)])), [[1], [3]]);
});

test('write: changing a number in 轮询 upgrades the rule to mode 6 and keeps the other numbers', () => {
    const { setRotatePriority, tryLevels } = rm();
    const next = setRotatePriority([t(1, 1, 6), t(2, 1, 6), t(3, 1, 6)], 0, 3);
    assert.deepEqual(enc(next), [[1, 6, 3, true], [2, 6, 1, true], [3, 6, 1, true]]);
    assert.deepEqual(levelIds(tryLevels(next)), [[1], [2, 3]]);
});

test('write: from plain mode 1, stale stored numbers are reset to what was shown (1)', () => {
    const { setRotatePriority } = rm();
    const next = setRotatePriority([t(1, 4, 1), t(2, 7, 1), t(3, 2, 1)], 0, 3);
    assert.deepEqual(enc(next), [[1, 6, 3, true], [2, 6, 1, true], [3, 6, 1, true]]);
});

test('switch 轮询 → 优先 keeps who goes first (原版 flipped A=3,B=C=1 into B,C first)', () => {
    const { switchToFirst } = rm();
    const next = switchToFirst([t(2, 1, 6), t(1, 3, 6), t(3, 1, 6)]);
    assert.deepEqual(enc(next), [[1, 3, 1, true], [2, 3, 2, true], [3, 3, 3, true]]);
});

test('switch 优先 → 轮询 = everyone equal (mode 1, numbers 1, not overridden)', () => {
    const { switchToRotate } = rm();
    assert.deepEqual(enc(switchToRotate([t(1, 1, 3), t(2, 2, 3)])), [[1, 1, 1, false], [2, 1, 1, false]]);
});

test('drag in 优先 renumbers 1..N in the new order', () => {
    const { renumberFirst, moveItem } = rm();
    const ordered = [t(1, 1, 3), t(2, 2, 3), t(3, 3, 3)];
    assert.deepEqual(enc(renumberFirst(moveItem(ordered, 2, 0))), [[3, 3, 1, true], [1, 3, 2, true], [2, 3, 3, true]]);
    assert.deepEqual(ids(moveItem(ordered, 0, 9)), [2, 3, 1]);
});

test('round trip: 优先 → 轮询 → 优先 and 轮询 numbers survive a read', () => {
    const { switchToFirst, switchToRotate, setRotatePriority, tryLevels, sortByTryOrder } = rm();
    const first = switchToFirst([t(1, 1, 3), t(2, 2, 3), t(3, 3, 3)]);
    const rotate = setRotatePriority(switchToRotate(first), 0, 3);
    assert.deepEqual(levelIds(tryLevels(rotate)), [[1], [2, 3]]);
    assert.deepEqual(ids(switchToFirst(rotate)), ids(sortByTryOrder(rotate)));
    assert.deepEqual(levelIds(tryLevels(switchToFirst(rotate))), [[1], [2], [3]]);
});

test('priority is clamped to >= 1', () => {
    const { clampPriority } = rm();
    assert.equal(clampPriority(0), 1);
    assert.equal(clampPriority(-3), 1);
    assert.equal(clampPriority(2.7), 2);
    assert.equal(clampPriority(Number.NaN), 1);
});
