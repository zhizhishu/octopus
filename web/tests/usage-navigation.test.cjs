/* eslint-disable @typescript-eslint/no-require-imports */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const config = load('src/route/config.tsx', {
    react: { lazy: () => ({}) },
    'lucide-react': new Proxy({}, { get: (_t, name) => ({ displayName: String(name) }) }),
    './lazy-with-preload': { lazyWithPreload: (factory) => ({ preload: factory }) },
});

test('real Usage follows dashboard and remains available to both roles', () => {
    const ids = config.ROUTES.map(r => r.id);
    assert.equal(ids[ids.indexOf('dashboard') + 1], 'usage');
    for (const role of ['admin', 'user', undefined]) {
        assert.ok(config.routesForRole(role).some(r => r.id === 'usage'));
    }
    assert.deepEqual([...config.USER_ROUTE_IDS], ['home', 'usage', 'key', 'log', 'mcp']);
    // ADMIN_ROUTE_IDS 跟着管理员侧栏的分组顺序走（ADMIN_NAV_GROUPS 各组按序拼接，18bd520f 重排过）：
    // 权限集合一字不变，只是顺序换了。
    assert.deepEqual([...config.ADMIN_ROUTE_IDS], ['home', 'dashboard', 'usage', 'user', 'key', 'access-plan', 'model', 'channel', 'redact', 'migration', 'mcp', 'prompt', 'audit', 'log', 'setting']);
});

test('routesForRole keeps the role order for the mobile bottom bar, not the ROUTES declaration order', () => {
    // ROUTES 是「有哪些路由」的注册表，18bd520f 为了管理员分组把 mcp 排到了 log 前面。
    // 普通用户手机底栏顺序由 USER_ROUTE_IDS 决定，必须是 home,usage,key,log,mcp——
    // 按 ROUTES 顺序过滤会把日志和 MCP 对调。
    assert.deepEqual(config.routesForRole('user').map(r => r.id), ['home', 'usage', 'key', 'log', 'mcp']);
    assert.deepEqual(config.routesForRole(undefined).map(r => r.id), ['home', 'usage', 'key', 'log', 'mcp']);
    assert.deepEqual(config.routesForRole(null).map(r => r.id), ['home', 'usage', 'key', 'log', 'mcp']);
    // 管理员的扁平列表同样按 ADMIN_ROUTE_IDS 顺序（桌面侧栏的分组渲染另走 navGroupsForRole）。
    assert.deepEqual(config.routesForRole('admin').map(r => r.id), [...config.ADMIN_ROUTE_IDS]);

    // 侧栏分组：组内顺序由 ADMIN_NAV_GROUPS 定，各组按序拼起来 = 管理员权限集合（一个不多一个不少），
    // 所以把 routesForRole 改成按角色顺序输出不会动管理员桌面分组。
    const groups = config.navGroupsForRole('admin');
    assert.ok(groups, 'admin must get nav groups');
    assert.deepEqual(groups.flatMap(g => [...g.routeIds]), [...config.ADMIN_ROUTE_IDS]);
    assert.equal(config.navGroupsForRole('user'), null);
    assert.equal(config.navGroupsForRole(undefined), null);
});

test('real Usage translations exist in all three locales', () => {
    const locales = ['en', 'zh_hans', 'zh_hant'].map(locale => JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${locale}.json`), 'utf8')));
    const expected = Object.keys(locales[0].usageReport).sort();
    for (const locale of locales) {
        assert.deepEqual(Object.keys(locale.usageReport).sort(), expected);
        assert.ok(locale.navbar.usage);
        assert.ok(Object.values(locale.usageReport).every(s => typeof s === 'string' && s.length > 0));
    }
});
