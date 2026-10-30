const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { clientLabel, accessBadge, lastAccess, windowParts, isNewInWindow, shortList, accessRuleParts } = load('src/components/modules/user/lib/access.ts');

test('客户端名：认得出的给名字 + 版本，认不出的照原文，没带 UA 是 undefined', () => {
    assert.equal(clientLabel('claude-cli/2.0.1 (external, cli)'), 'Claude CLI 2.0.1');
    assert.equal(clientLabel('codex_cli_rs/0.50.0 (Mac OS 15.1; arm64)'), 'Codex CLI 0.50.0');
    assert.equal(clientLabel('CherryStudio/1.5.2'), 'Cherry Studio 1.5.2');
    assert.equal(clientLabel('Mozilla/5.0 (Macintosh)'), '浏览器');
    assert.equal(clientLabel('my-tool/3.1 (+https://github.com/x/y)'), 'my-tool/3.1');
    assert.equal(clientLabel(''), undefined);
    assert.equal(clientLabel(undefined), undefined);
});

test('徽章：IP 过多优先，IP 和客户端一起换合成一个', () => {
    assert.equal(accessBadge([]), null);
    assert.equal(accessBadge(undefined), null);
    assert.equal(accessBadge([{ kind: 'new_ip' }]), 'newIP');
    assert.equal(accessBadge([{ kind: 'new_client' }]), 'newClient');
    assert.equal(accessBadge([{ kind: 'new_ip' }, { kind: 'new_client' }]), 'ipAndClient');
    assert.equal(accessBadge([{ kind: 'many_ips' }, { kind: 'new_client' }]), 'manyIPs');
});

test('最近调用：来源表更新就用它（带客户端），否则退回用户表的 last_relay', () => {
    const s = { last: { ip: '1.1.1.1', user_agent: 'claude-cli/2.0.1', api_key_id: 3, api_key_name: 'k', at: 200 } };
    assert.deepEqual(lastAccess({ last_relay_at: 100, last_relay_ip: '9.9.9.9' }, s), { ip: '1.1.1.1', userAgent: 'claude-cli/2.0.1', keyName: 'k', at: 200 });
    assert.deepEqual(lastAccess({ last_relay_at: 300, last_relay_ip: '1.1.1.1' }, s), { ip: '1.1.1.1', userAgent: 'claude-cli/2.0.1', at: 300 });
    assert.deepEqual(lastAccess({ last_relay_at: 300, last_relay_ip: '9.9.9.9' }, s), { ip: '9.9.9.9', userAgent: undefined, at: 300 });
    assert.equal(lastAccess({ last_relay_at: 0 }, undefined), null);
});

test('窗口文案与「新」标记', () => {
    assert.deepEqual(windowParts(3600), { unit: 'hours', n: 1 });
    assert.deepEqual(windowParts(1800), { unit: 'minutes', n: 30 });
    assert.equal(isNewInWindow(1000, 1000 + 3600, 3600), true);
    assert.equal(isNewInWindow(1000, 1000 + 3601, 3600), false);
    assert.deepEqual(shortList(['a', 'b', 'c', 'd', 'e']), { shown: ['a', 'b', 'c'], rest: 2 });
});

// 与后端 model.UserAccessAlertPreset 相同的四档（后端 TestUserAccessAlertLevels 钉住判定，这里钉住说明文字）。
const PRESETS = {
    off: { window_sec: 3600, many_ips: 0, new_ip: false, new_client: false, ipv4_prefix: 32 },
    loose: { window_sec: 3600, many_ips: 11, new_ip: false, new_client: false, ipv4_prefix: 24 },
    standard: { window_sec: 3600, many_ips: 3, new_ip: true, new_client: true, ipv4_prefix: 32 },
    strict: { window_sec: 3600, many_ips: 2, new_ip: true, new_client: true, ipv4_prefix: 32 },
};

test('风控等级说明：每一档列出的就是它真正会报的', () => {
    const keys = (cfg) => accessRuleParts(cfg).map((p) => p.key);
    assert.deepEqual(keys(PRESETS.off), ['off']);
    assert.deepEqual(keys(PRESETS.loose), ['manyIPs', 'group24']);
    assert.equal(accessRuleParts(PRESETS.loose)[0].n, 11);
    assert.deepEqual(keys(PRESETS.standard), ['manyIPs', 'newIP', 'newClient', 'group32']);
    assert.equal(accessRuleParts(PRESETS.standard)[0].n, 3);
    // 严格：任意第 2 个 IP 已经算「IP 过多」，不再单列「新 IP」。
    assert.deepEqual(keys(PRESETS.strict), ['manyIPs', 'newClient', 'group32']);
    assert.equal(accessRuleParts(PRESETS.strict)[0].n, 2);
    assert.deepEqual(keys(undefined), ['off']);
});

test('风控等级说明：自定义按填的数拼，全关等于不提示', () => {
    const keys = (cfg) => accessRuleParts(cfg).map((p) => p.key);
    assert.deepEqual(keys({ window_sec: 600, many_ips: 0, new_ip: true, new_client: false, ipv4_prefix: 24 }), ['newIP', 'group24']);
    assert.equal(accessRuleParts({ window_sec: 600, many_ips: 0, new_ip: true, new_client: false, ipv4_prefix: 24 })[0].windowSec, 600);
    assert.deepEqual(keys({ window_sec: 600, many_ips: 0, new_ip: false, new_client: true, ipv4_prefix: 24 }), ['newClient']);
    assert.deepEqual(keys({ window_sec: 600, many_ips: 0, new_ip: false, new_client: false, ipv4_prefix: 32 }), ['off']);
    assert.deepEqual(keys({ window_sec: 0, many_ips: 3, new_ip: true, new_client: true, ipv4_prefix: 32 }), ['off']);
});
