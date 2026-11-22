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
    assert.deepEqual([...config.ADMIN_ROUTE_IDS], ['home', 'dashboard', 'usage', 'user', 'key', 'channel', 'redact', 'access-plan', 'model', 'migration', 'prompt', 'audit', 'log', 'mcp', 'setting']);
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
