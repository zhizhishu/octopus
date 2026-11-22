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

test('billing mockup routes preserve ordering and are available to both roles', () => {
    const ids = config.ROUTES.map(r => r.id);
    assert.equal(ids[ids.indexOf('dashboard') + 1], 'usage');
    assert.equal(ids[ids.indexOf('user') + 1], 'billing');
    for (const role of ['admin', 'user', undefined]) {
        const available = config.routesForRole(role).map(r => r.id);
        assert.ok(available.includes('usage'));
        assert.ok(available.includes('billing'));
    }
});

test('all billing mockup translations exist in all three locales', () => {
    const locales = ['en', 'zh_hans', 'zh_hant'].map(locale => JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${locale}.json`), 'utf8')));
    const expected = Object.keys(locales[0].billingMock).sort();
    for (const locale of locales) {
        assert.deepEqual(Object.keys(locale.billingMock).sort(), expected);
        assert.ok(locale.navbar.usage);
        assert.ok(locale.navbar.billing);
        assert.ok(Object.values(locale.billingMock).every(s => typeof s === 'string' && s.length > 0));
    }
});
