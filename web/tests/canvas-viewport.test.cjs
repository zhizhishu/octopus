const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

// 方案无限画布：数据编辑（+1/−1、弹窗保存、增删映射、保存后回刷）不能让视口跳回 fit-all。
const viewport = () => load('src/components/modules/access-plan/canvas-viewport.ts');

const key = (overrides = {}) => ({ planId: 7, view: 'all|||', empty: false, ...overrides });

test('first mount fits, or restores the same plan viewport when one is saved', () => {
    const { decideCanvasViewport } = viewport();
    assert.equal(decideCanvasViewport(null, key(), false), 'fit');
    assert.equal(decideCanvasViewport(null, key(), true), 'restore');
});

test('data edits on the same plan and view keep the current pan and zoom', () => {
    const { decideCanvasViewport } = viewport();
    // +1 / −1, dialog save, add/remove mapping and the post-save refetch all rebuild
    // the flow with the same plan id and the same filter view.
    assert.equal(decideCanvasViewport(key(), key(), false), 'keep');
    assert.equal(decideCanvasViewport(key(), key(), true), 'keep');
});

test('switching plan fits', () => {
    const { decideCanvasViewport } = viewport();
    assert.equal(decideCanvasViewport(key(), key({ planId: 8 }), false), 'fit');
    assert.equal(decideCanvasViewport(key(), key({ planId: 8 }), true), 'fit');
});

test('switching the filter view fits; going from empty to content fits; empty never moves', () => {
    const { decideCanvasViewport, canvasViewSignature } = viewport();
    const channelView = canvasViewSignature('channel', '3', '', '');
    assert.equal(decideCanvasViewport(key(), key({ view: channelView }), false), 'fit');
    assert.equal(decideCanvasViewport(key({ empty: true }), key(), false), 'fit');
    assert.equal(decideCanvasViewport(key(), key({ empty: true }), false), 'keep');
    assert.equal(decideCanvasViewport(null, key({ empty: true }), false), 'keep');
});

test('view signature ignores search case and padding', () => {
    const { canvasViewSignature } = viewport();
    assert.equal(canvasViewSignature('all', '', '', '  GPT '), canvasViewSignature('all', '', '', 'gpt'));
    assert.notEqual(canvasViewSignature('all', '', '', 'gpt'), canvasViewSignature('all', '', '', 'claude'));
});

test('saved viewport is only reused for the same plan', () => {
    const { rememberCanvasViewport, savedCanvasViewport, forgetCanvasViewport } = viewport();
    forgetCanvasViewport();
    assert.equal(savedCanvasViewport(7), null);
    rememberCanvasViewport(7, { x: -120, y: 40, zoom: 1.4 });
    assert.deepEqual(savedCanvasViewport(7), { x: -120, y: 40, zoom: 1.4 });
    assert.equal(savedCanvasViewport(8), null);
    rememberCanvasViewport(7, { x: NaN, y: 0, zoom: 1 });
    assert.deepEqual(savedCanvasViewport(7), { x: -120, y: 40, zoom: 1.4 });
});

test('server version tracks route content (backend sends no updated_at), not object identity', () => {
    const { routeTargetsServerVersion } = viewport();
    const t = (priority) => [{ id: 1, request_model: 'gpt', channel_id: 2, upstream_model: 'gpt', priority }];
    assert.equal(routeTargetsServerVersion({ id: 7, route_targets: t(1) }), routeTargetsServerVersion({ id: 7, route_targets: t(1) }));
    assert.notEqual(routeTargetsServerVersion({ id: 7, route_targets: t(1) }), routeTargetsServerVersion({ id: 7, route_targets: t(2) }));
    assert.notEqual(routeTargetsServerVersion({ id: 7, updated_at: 1, route_targets: t(1) }), routeTargetsServerVersion({ id: 7, updated_at: 2, route_targets: t(1) }));
});

test('editor resyncs from server only when server data changed and no save is in flight', () => {
    const { shouldResyncRouteTargets } = viewport();
    assert.equal(shouldResyncRouteTargets('a', 'a', false), false);
    assert.equal(shouldResyncRouteTargets('a', 'b', false), true);
    assert.equal(shouldResyncRouteTargets('a', 'b', true), false);
});

// 源码守卫：确保回归点不会被悄悄改回来。
test('canvas source no longer remounts on updated_at nor fits on every flow change', () => {
    const src = require('node:fs').readFileSync(require('node:path').join(__dirname, '../src/components/modules/access-plan/index.tsx'), 'utf8');
    assert.ok(src.includes('key={`routes-${selectedPlan.id}`}'), 'routes editor key must only contain plan id');
    assert.ok(!/key=\{`routes-\$\{selectedPlan\.id\}-\$\{selectedPlan\.updated_at/.test(src), 'routes editor key must not include updated_at');
    assert.ok(!/<ReactFlow<FlowNode, Edge>[\s\S]{0,400}\n\s+fitView\n/.test(src), 'ReactFlow must not use the fitView prop (it refits on every remount)');
    assert.ok(src.includes('decideCanvasViewport('), 'flow effect must go through decideCanvasViewport');
});
