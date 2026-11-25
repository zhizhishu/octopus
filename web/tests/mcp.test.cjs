/* eslint-disable @typescript-eslint/no-require-imports */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const read = (file) => readFileSync(path.join(__dirname, '..', file), 'utf8');

test('MCP navigation is available to admin and normal users', () => {
    const config = load('src/route/config.tsx', {
        react: { lazy: () => ({}) },
        'lucide-react': new Proxy({}, { get: (_target, name) => ({ displayName: String(name) }) }),
        './lazy-with-preload': { lazyWithPreload: (factory) => ({ preload: factory }) },
    });
    for (const role of ['admin', 'user']) {
        assert.ok(config.routesForRole(role).some((route) => route.id === 'mcp'));
    }
});

test('MCP usage query is identity-scoped, ranged, and uses the real endpoint', async () => {
    let options;
    const calls = [];
    const hooks = load('src/api/endpoints/mcp.ts', {
        '@tanstack/react-query': { useQuery: (value) => { options = value; return value; } },
        '../client': { apiClient: { get: async (url) => { calls.push(url); return { totals: {}, top_keys: [] }; } } },
    });
    hooks.useMCPStats(12);
    assert.deepEqual(options.queryKey, ['mcp', 'stats', 12, 7]);
    assert.equal(options.enabled, true);
    await options.queryFn();
    hooks.useMCPStats(12, 30);
    assert.deepEqual(options.queryKey, ['mcp', 'stats', 12, 30]);
    await options.queryFn();
    assert.deepEqual(calls, ['/api/v1/mcp/stats?days=7', '/api/v1/mcp/stats?days=30']);
    assert.deepEqual([...hooks.MCP_RANGES], [1, 7, 30]);
    hooks.useMCPStats(undefined);
    assert.equal(options.enabled, false);
});

test('MCP settings hooks stay in admin-only components', () => {
    const source = read('src/components/modules/mcp/index.tsx');
    const [admin, rest] = source.split('/** 选当前账号自己的 Key');
    const user = rest.split('export function MCP()')[1];
    assert.match(admin, /function AdminSwitch\(\)[\s\S]*useSettingList\(\)/);
    assert.match(admin, /function AdminAdvanced\(\)[\s\S]*useModelList\(\)/);
    assert.doesNotMatch(rest, /useSettingList\(|useModelList\(|useSetSetting\(|<Switch/);
    assert.match(user, /isAdmin && <AdminSwitch \/>/);
    assert.match(user, /isAdmin && <AdminAdvanced \/>/);
    assert.match(user, /isAdmin && stats\.data && <div/);
    assert.match(source, /window.location.origin/);
    assert.match(user, /stats\.isLoading/);
    assert.match(user, /stats\.isError/);
    assert.doesNotMatch(source, /demo-key|oct\.example\.com/);
});

test('MCP config auto-fills the caller key but only shows it masked', () => {
    const source = read('src/components/modules/mcp/index.tsx');
    const user = source.split('export function MCP()')[1];
    assert.match(source, /const PLACEHOLDER_KEY = 'sk-octopus-xxxxxxxx'/);
    // API Key 登录直接用当前 token；网页账号登录才挂 KeyPicker（不对 API Key 会话调用列表接口）。
    assert.match(user, /\(isAPIKeyAuth \? token : picked\) \|\| PLACEHOLDER_KEY/);
    assert.match(user, /!isAPIKeyAuth && user && <KeyPicker/);
    assert.match(source, /function KeyPicker[\s\S]*useAPIKeyList\(\)[\s\S]*key\.enabled/);
    // 复制的是完整 Key，页面展示的是掩码。
    assert.match(user, /config\(apiKey\)/);
    assert.match(user, /maskKey\(apiKey\)/);
});

test('MCP locale keys are complete in all three languages', () => {
    const locales = ['zh_hans', 'zh_hant', 'en'].map((name) => JSON.parse(read(`public/locale/${name}.json`)));
    const keys = Object.keys(locales[0].mcp).sort();
    for (const locale of locales) {
        assert.equal(locale.navbar.mcp, 'MCP');
        assert.deepEqual(Object.keys(locale.mcp).sort(), keys);
        assert.ok(Object.values(locale.mcp).every((value) => value.length > 0));
    }
    const source = read('src/components/modules/mcp/index.tsx');
    for (const match of source.matchAll(/\bt\('([a-zA-Z0-9]+)'\)/g)) {
        assert.ok(keys.includes(match[1]), `missing MCP translation: ${match[1]}`);
    }
    for (const key of ['globalOnHint', 'globalOffHint', 'keyFilledHint', 'keyMissingHint', 'usage', 'ownUsage', 'copyUrl', 'copyConfig', 'generateDescription', 'editDescription']) {
        assert.ok(keys.includes(key), `missing dynamic MCP translation: ${key}`);
    }
});
