/* eslint-disable @typescript-eslint/no-require-imports */
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { load } = require('./load-typescript.cjs');

function harness() {
    let user = { id: 10, role: 'user' };
    const calls = [];
    const endpoint = load('src/api/endpoints/usage-report.ts', {
        '@tanstack/react-query': { useQuery: options => options },
        './user': { useAuthStore: selector => selector({ user }) },
        '../client': { apiClient: {
            get: async (...args) => { calls.push(args); return { rows: [], summary: {} }; },
            getBlob: async () => { throw new Error('export failed'); },
        } },
    });
    return { endpoint, calls, login: value => { user = value; } };
}

test('usage hook scopes its cache to identity and query and disables incomplete custom dates', async () => {
    const h = harness();
    const query = { range: '7d', group: 'user', user_id: undefined, model: '' };
    const alice = h.endpoint.useUsageReport(query);
    const aliceKeys = h.endpoint.useUsageReportKeys();
    assert.equal(alice.enabled, true);
    assert.deepEqual(await alice.queryFn({ signal: undefined }), { rows: [], summary: {} });
    assert.deepEqual(h.calls[0][1], { range: '7d', group: 'user' });
    h.login({ id: 20, role: 'user' });
    assert.notDeepEqual(h.endpoint.useUsageReport(query).queryKey, alice.queryKey);
    assert.notDeepEqual(h.endpoint.useUsageReportKeys().queryKey, aliceKeys.queryKey);
    h.login({ id: 10, role: 'admin' });
    assert.notDeepEqual(h.endpoint.useUsageReport(query).queryKey, alice.queryKey);
    assert.notDeepEqual(h.endpoint.useUsageReport({ ...query, group: 'model' }).queryKey, alice.queryKey);
    assert.equal(h.endpoint.useUsageReport({ range: 'custom', group: 'user' }).enabled, false);
    h.login(null);
    assert.equal(h.endpoint.useUsageReport(query).enabled, false);
    assert.equal(h.endpoint.useUsageReportKeys().enabled, false);
    await assert.rejects(h.endpoint.downloadUsageReport(query), /export failed/);
});

test('CSV download uses the authenticated blob endpoint and releases its object URL', async () => {
    const calls = [];
    const endpoint = load('src/api/endpoints/usage-report.ts', {
        '@tanstack/react-query': {}, './user': {},
        '../client': { apiClient: { getBlob: async path => { calls.push(path); return new Blob(['csv']); } } },
    });
    const oldDocument = global.document;
    const oldCreate = URL.createObjectURL;
    const oldRevoke = URL.revokeObjectURL;
    const anchor = { click: () => calls.push('clicked') };
    global.document = { createElement: () => anchor };
    URL.createObjectURL = () => 'blob:test';
    URL.revokeObjectURL = url => calls.push(url);
    try {
        await endpoint.downloadUsageReport({ range: 'today', group: 'apikey', api_key_id: 101, model: 'fake/model' });
        assert.equal(calls[0], '/api/v1/stats/usage-report.csv?range=today&group=apikey&api_key_id=101&model=fake%2Fmodel');
        assert.equal(anchor.download, 'usage-report.csv');
        assert.deepEqual(calls.slice(1), ['clicked', 'blob:test']);
    } finally {
        global.document = oldDocument;
        URL.createObjectURL = oldCreate;
        URL.revokeObjectURL = oldRevoke;
    }
});

function render(report, admin = false) {
    const React = require('react');
    const { renderToStaticMarkup } = require('react-dom/server');
    const wrap = ({ children }) => React.createElement('div', null, children);
    const ui = new Proxy({}, { get: () => wrap });
    const dashboard = load('src/components/modules/dashboard/model.ts');
    const { Usage, usageCacheRate } = load('src/components/modules/usage.tsx', {
        'next-intl': { useTranslations: () => key => key },
        'lucide-react': ui, recharts: ui,
        '@/components/common/PageWrapper': ui,
        '@/components/ui/button': { Button: ({ children, ...props }) => React.createElement('button', props, children) },
        '@/components/ui/input': ui, '@/components/ui/select': ui, '@/components/ui/table': ui,
        '@/api/endpoints/user': { useAuthStore: selector => selector({ user: { id: 10, role: admin ? 'admin' : 'user' } }), useUserList: () => ({ data: [] }) },
        '@/api/endpoints/usage-report': { useUsageReport: () => report, useUsageReportKeys: () => ({ data: [] }) },
        '@/components/modules/dashboard/model': dashboard,
        '@/stores/setting': { useSettingStore: () => ({ locale: 'en', setLocale: () => {} }) },
    });
    return { html: renderToStaticMarkup(React.createElement(Usage)), usageCacheRate };
}

test('usage renders real rows without a preview badge and reuses dashboard cache denominator', () => {
    const usage = { request_success: 3, request_failed: 1, input_token: 100, output_token: 20, cache_hit_token: 60, cache_write_token: 20, cache_input_token: 200, total_cost: 1.25 };
    const { html, usageCacheRate } = render({ data: { start: '2026-06-13', end: '2026-06-14', summary: usage, daily: [], rows: [{ id: '10', name: 'alice-live', usage, models: [] }] } });
    assert.match(html, /alice-live/);
    assert.match(html, /30\.0%/);
    assert.match(html, /\$1\.2500/);
    assert.doesNotMatch(html, /design preview|fake data/i);
    assert.doesNotMatch(html, /allUsers/);
    assert.equal(usageCacheRate({ ...usage, cache_hit_token: 0, cache_write_token: 0, cache_input_token: 0, input_token: 0 }), '—');
});

test('empty usage and request failures are explicit, with CSV disabled on failures', () => {
    const summary = { request_success: 0, request_failed: 0, input_token: 0, output_token: 0, cache_hit_token: 0, cache_write_token: 0, cache_input_token: 0, total_cost: 0, wait_time: 0 };
    const { html } = render({ data: { start: '2026-06-13', end: '2026-06-14', summary, daily: [], rows: [] } }, true);
    assert.match(html, /emptyUsage/);
    assert.match(html, /allUsers/);
    const error = render({ isError: true }).html;
    assert.match(error, /role="alert"/);
    assert.match(error, /loadError/);
    assert.match(error, /disabled=""/);
    assert.match(render({ isLoading: true }).html, /role="status"/);
});
