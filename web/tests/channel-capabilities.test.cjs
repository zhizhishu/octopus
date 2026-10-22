'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');

// src/lib/channel-capabilities.ts 是纯 TS、无 @/ 别名导入，load-typescript 直接就能跑，
// 不需要喂替身。
const {
    CAPABILITY_EMBEDDINGS,
    hasChannelCapability,
    setChannelCapability,
    channelCapabilitiesPayload,
    channelCapabilitiesUpdate,
} = load('src/lib/channel-capabilities.ts');

/**
 * 这两条是契约第七节点名的：能力位勾选后提交载荷含 capabilities: ['embeddings']，
 * 不勾时不发该字段（存量请求形状逐字节不变，契约铁律 1）。
 */

test('勾选「支持嵌入流量」后，创建载荷带 capabilities: [embeddings]', () => {
    // 用户在一个全新渠道上把勾选框打开（初始 capabilities 是空数组）。
    const next = setChannelCapability([], CAPABILITY_EMBEDDINGS, true);
    assert.deepEqual(next, ['embeddings']);

    const payload = channelCapabilitiesPayload(next);
    assert.deepEqual(payload, { capabilities: ['embeddings'] });
    assert.deepEqual(payload.capabilities, ['embeddings']);
});

test('未勾选时，创建载荷里根本没有 capabilities 字段', () => {
    // 存量渠道：表单里没碰过这个勾选框（capabilities 是 []，提交前还被 ?? [] 兜底过）。
    const payload = channelCapabilitiesPayload([]);
    assert.deepEqual(payload, {});
    assert.equal('capabilities' in payload, false);
    assert.equal(Object.hasOwn(payload, 'capabilities'), false);
    assert.deepEqual(Object.keys(payload), []);

    // 历史数据里 capabilities 可能是 undefined（老行没这一列），必须同一条路：
    const payloadUndefined = channelCapabilitiesPayload(undefined);
    assert.deepEqual(payloadUndefined, {});
    assert.equal('capabilities' in payloadUndefined, false);
});

/**
 * 下面几条守的是"勾选框不许清掉别的能力位"。
 * 后端 Capabilities 是 []string，将来会并肩出现 images / rerank；
 * 如果实现写成 `enabled ? [capability] : []`，一次取消勾选就把它们全抹了。
 */

test('取消勾选只摘掉 embeddings，其它能力位原样保留', () => {
    const withOthers = ['images', 'embeddings', 'rerank'];
    assert.deepEqual(setChannelCapability(withOthers, CAPABILITY_EMBEDDINGS, false), ['images', 'rerank']);
    // 再勾回来是追加，不重复。
    assert.deepEqual(setChannelCapability(['embeddings'], CAPABILITY_EMBEDDINGS, true), ['embeddings']);
    assert.equal(hasChannelCapability(['images', 'embeddings'], CAPABILITY_EMBEDDINGS), true);
    assert.equal(hasChannelCapability(['images'], CAPABILITY_EMBEDDINGS), false);
    assert.equal(hasChannelCapability(undefined, CAPABILITY_EMBEDDINGS), false);
});

test('input 数组不被就地修改（React state 不能被打穿）', () => {
    const original = ['embeddings'];
    const next = setChannelCapability(original, CAPABILITY_EMBEDDINGS, false);
    assert.deepEqual(original, ['embeddings']);
    assert.notEqual(original, next);
});

test('编辑：没改能力位就不发该字段', () => {
    assert.deepEqual(channelCapabilitiesUpdate(['embeddings'], ['embeddings']), {});
    const untouched = channelCapabilitiesUpdate([], []);
    assert.equal('capabilities' in untouched, false);
    assert.equal('capabilities' in channelCapabilitiesUpdate(undefined, undefined), false);
});

test('编辑：取消勾选要显式发 capabilities: []（否则后端清不掉）', () => {
    const cleared = channelCapabilitiesUpdate(['embeddings'], []);
    assert.equal('capabilities' in cleared, true);
    assert.deepEqual(cleared, { capabilities: [] });
    // 反向：从没有到有也要发。
    assert.deepEqual(channelCapabilitiesUpdate([], ['embeddings']), { capabilities: ['embeddings'] });
});
