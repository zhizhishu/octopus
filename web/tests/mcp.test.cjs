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

test('MCP usage query is identity-scoped and uses the real endpoint', async () => {
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
    assert.deepEqual(calls, ['/api/v1/mcp/stats?days=7']);
    hooks.useMCPStats(undefined);
    assert.equal(options.enabled, false);
});

test('MCP controls and admin-only queries are confined to AdminTools', () => {
    const source = read('src/components/modules/mcp/index.tsx');
    const [admin, user] = source.split('export function MCP()');
    assert.match(admin, /useSettingList\(\)/);
    assert.match(admin, /useModelList\(\)/);
    assert.doesNotMatch(user, /useSettingList\(|useModelList\(|useSetSetting\(|<Switch/);
    assert.match(user, /user\?\.role === 'admin' && <AdminTools/);
    assert.match(source, /window.location.origin/);
    assert.match(user, /Bearer sk-octopus-xxxxxxxx/);
    assert.match(user, /stats\.isLoading/);
    assert.match(user, /stats\.isError/);
    assert.match(user, /stats\.data\.top_keys\.length === 0/);
    assert.doesNotMatch(source, /demo-key|oct\.example\.com/);
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
    for (const match of source.matchAll(/\bt\('([a-zA-Z]+)'\)/g)) {
        assert.ok(keys.includes(match[1]), `missing MCP translation: ${match[1]}`);
    }
});
