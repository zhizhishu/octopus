// Run from web/: node --test tests/model-audit-refresh.test.cjs
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { setImmediate } = require('node:timers/promises');
const { load } = require('./load-typescript.cjs');

// QueryObserver must use its browser timers, not its server-rendering branch.
global.window = {};
const { QueryClient, QueryObserver, MutationObserver, focusManager } = require('@tanstack/react-query');
const React = require('react');

const snapshot = (results = []) => ({ last_run_unix: 100, target_count: results.length, ran_count: results.length, results, disclaimer: 'fixture' });
const clean = { channel_id: 7, channel_name: 'test channel', model: 'test-model', trigger: 'schedule', verdict: 'none', score: 0 };
const suspicious = { ...clean, verdict: 'high', score: 90, trigger: 'echo_mismatch' };
const log = Object.freeze({ id: 42, time: 100, channel: 7, actual_model_name: 'test-model', request_model_name: 'requested-model', attempts: [], input_tokens: 0, output_tokens: 0, cost: 0, use_time: 1 });

function endpoint(client, apiClient) {
    return load('src/api/endpoints/model-audit.ts', {
        '@tanstack/react-query': { useQuery: (options) => options, useMutation: (options) => options, useQueryClient: () => client },
        '../client': { apiClient },
        '@/lib/logger': { logger: { error() {} } },
    });
}

// Execute the actual component functions and preserve hook memo identities between renders.
// DOM/widgets are deliberately stubbed; browser layout and interaction remain a browser test.
function hooks() {
    const slots = [];
    let cursor = 0;
    return {
        begin() { cursor = 0; },
        react: {
            ...React,
            useMemo(fn, deps) {
                const i = cursor++;
                if (!slots[i] || deps.some((value, j) => !Object.is(value, slots[i].deps[j]))) slots[i] = { deps, value: fn() };
                return slots[i].value;
            },
            useState(initial) { return [typeof initial === 'function' ? initial() : initial, () => {}]; },
            useCallback(fn) { return fn; },
            useEffect() {},
            useDeferredValue(value) { return value; },
        },
    };
}
const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));
function findElement(node, predicate) {
    if (Array.isArray(node)) return node.map((child) => findElement(child, predicate)).find(Boolean);
    if (!node || typeof node !== 'object') return undefined;
    return predicate(node) ? node : findElement(node.props?.children, predicate);
}
function text(node) {
    if (Array.isArray(node)) return node.map(text).join('');
    if (!node || typeof node === 'boolean') return '';
    if (typeof node !== 'object') return String(node);
    return text(node.props?.children);
}
function logComponents() {
    let queryResult = { data: undefined, isError: false };
    const pageHooks = hooks();
    const cardHooks = hooks();
    const common = {
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/user': { useAuthStore: (select) => select({ user: { role: 'admin' } }), useUserList: () => ({ data: [] }) },
        '@/components/ui/badge': widgets('Badge'),
        '@/components/ui/popover': widgets('Popover PopoverContent PopoverTrigger'),
        '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join(' ') },
    };
    const item = load('src/components/modules/log/Item.tsx', {
        ...common,
        react: cardHooks.react,
        'motion/react': { motion: {}, AnimatePresence: 'AnimatePresence' },
        '@uiw/react-json-view': {},
        '@uiw/react-json-view/githubDark': {},
        '@uiw/react-json-view/githubLight': {},
        'next-themes': {},
        zustand: { create: (init) => { const state = init(() => {}); return (select) => select(state); } },
        '@/api/endpoints/log': { getRelayLogSeverity: () => 'success' },
        './humanize': load('src/components/modules/log/humanize.ts'),
        '@/lib/model-icons': { getModelIcon: () => ({ Avatar: 'Avatar', color: 'grey' }) },
        '@/lib/model-aliases': { marketModelName: (name) => name },
        '@/components/common/CopyButton': widgets('CopyIconButton'),
        '@/components/common/SafeText': widgets('ErrorSafeText MonoSafeText SafeText'),
        '@/components/ui/morphing-dialog': widgets('MorphingDialog MorphingDialogTrigger MorphingDialogContainer MorphingDialogContent MorphingDialogClose MorphingDialogTitle MorphingDialogDescription'),
    });
    const { Log } = load('src/components/modules/log/index.tsx', {
        ...common,
        react: pageHooks.react,
        './Item': item,
        '@/api/endpoints/model-audit': { useScheduledAudit: (enabled) => { assert.equal(enabled, true); return queryResult; } },
        '@/api/endpoints/log': { useLogs: () => ({ logs: [log] }), useLogSeverityCounts: () => ({ data: { total: 1 } }), useExportLogs: () => ({}), useRequestStateStream: () => ({ states: [] }) },
        '@/api/endpoints/apikey': { useAPIKeyList: () => ({ data: [] }) },
        '@/api/endpoints/channel': { useChannelList: () => ({ data: [] }) },
        '@/api/endpoints/model': { useModelList: () => ({ data: [] }) },
        '@/api/endpoints/intervention': {},
        '@/api/endpoints/setting': { useSettingList: () => ({}), useSetSetting: () => ({}), SettingKey: {} },
        '@/components/common/VirtualizedGrid': widgets('VirtualizedGrid'),
        '@/components/common/PageWrapper': widgets('PageWrapper'),
        '@/components/ui/calendar': widgets('Calendar'),
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/common/Toast': { toast: {} },
        '@/components/animate-ui/components/animate/tooltip': widgets('TooltipProvider'),
    });
    let previousProps;
    let auditSlot;
    let renders = 0;
    return {
        render(result) {
            queryResult = result;
            pageHooks.begin();
            const grid = findElement(Log(), (node) => node.type === 'VirtualizedGrid');
            assert.ok(grid, 'actual Log must pass cards through its grid');
            const props = grid.props.renderItem(log).props;
            if (!previousProps || !item.LogCard.compare(previousProps, props)) {
                cardHooks.begin();
                const card = item.LogCard.type(props);
                auditSlot = findElement(card, (node) => Object.hasOwn(node.props ?? {}, 'auditSlot')).props.auditSlot;
                renders++;
            }
            previousProps = props;
            return { label: text(auditSlot), renders, props };
        },
    };
}

test('saved snapshot polling updates unchanged log cards: empty, new, changed, failed, removed', async (t) => {
    t.mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: 1000 });
    focusManager.setFocused(true);
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity }, mutations: { gcTime: Infinity } } });
    let saved = snapshot();
    let failure = false;
    const requests = [];
    const api = endpoint(client, {
        async get(url) { requests.push(url); if (failure) throw new Error('snapshot unavailable'); return structuredClone(saved); },
        async post() { assert.fail('snapshot refresh must never run an upstream audit'); },
    });
    const observer = new QueryObserver(client, api.useScheduledAudit());
    const ui = logComponents();
    let displayed;
    const unsubscribe = observer.subscribe((result) => { displayed = ui.render(result); });
    t.after(() => { unsubscribe(); client.clear(); focusManager.setFocused(undefined); });
    await setImmediate();
    assert.equal(displayed.label, '');
    assert.equal(requests.length, 1);

    const tick = async () => { t.mock.timers.tick(5000); await setImmediate(); };
    saved = snapshot([clean]);
    await tick();
    assert.match(displayed.label, /auditClean/);
    assert.strictEqual(displayed.props.log, log, 'id/time and the whole log remain unchanged');
    const stableRenders = displayed.renders;
    await tick();
    assert.equal(displayed.renders, stableRenders, 'identical API data preserves query/map identity');

    // Append puts newest first. The previous clean result must not overwrite it.
    saved = snapshot([suspicious, clean]);
    await tick();
    assert.match(displayed.label, /auditHigh/);
    assert.doesNotMatch(displayed.label, /auditClean/);

    saved = snapshot([{ ...clean, verdict: 'unknown', error_count: 1 }, clean]);
    await tick();
    assert.match(displayed.label, /auditInsufficient/);
    saved = snapshot([{ ...clean, skipped: true }, clean]);
    await tick();
    assert.equal(displayed.label, '', 'a newer skipped result cannot fall back to old green');

    saved = snapshot([clean]);
    await tick();
    assert.match(displayed.label, /auditClean/);
    const lastData = observer.getCurrentResult().data;
    failure = true;
    await tick();
    assert.equal(observer.getCurrentResult().isError, true);
    assert.strictEqual(observer.getCurrentResult().data, lastData, 'failure does not synthesize new data');
    assert.equal(displayed.label, '', 'failed refresh must hide cached green evidence');
    failure = false;
    await tick();
    assert.match(displayed.label, /auditClean/);

    saved = snapshot();
    await tick();
    assert.equal(displayed.label, '', 'removed results disappear without a new log');
    assert.ok(requests.every((url) => url === '/api/v1/model-audit/scheduled'));

    focusManager.setFocused(false);
    const beforeHidden = requests.length;
    await tick();
    assert.equal(requests.length, beforeHidden, 'hidden page does not poll');
    focusManager.setFocused(true);
    await tick();
    assert.equal(requests.length, beforeHidden + 1, 'visible page resumes polling');
    unsubscribe();
    const beforeUnmount = requests.length;
    await tick();
    assert.equal(requests.length, beforeUnmount, 'unmounted query does not poll');
});

test('disabled admin query never fetches; initial failure cannot create clean evidence', async (t) => {
    t.mock.timers.enable({ apis: ['setTimeout', 'setInterval', 'Date'], now: 1000 });
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity }, mutations: { gcTime: Infinity } } });
    let requests = 0;
    const api = endpoint(client, { async get() { requests++; throw new Error('unavailable'); } });
    const observer = new QueryObserver(client, api.useScheduledAudit(false));
    const unsubscribe = observer.subscribe(() => {});
    t.after(() => { unsubscribe(); client.clear(); });
    t.mock.timers.tick(30000);
    await setImmediate();
    assert.equal(requests, 0);
    observer.setOptions(api.useScheduledAudit(true));
    await setImmediate();
    const result = observer.getCurrentResult();
    assert.equal(result.isError, true);
    assert.equal(result.data, undefined);
    assert.equal(logComponents().render(result).label, '');
});

test('manual success invalidates related queries, failure does not, and neither fabricates a snapshot', async (t) => {
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity }, mutations: { gcTime: Infinity } } });
    const scheduledKey = ['model-audit', 'scheduled'];
    const anomaliesKey = ['model-audit', 'log-anomalies'];
    let fail = true;
    const requests = [];
    const api = endpoint(client, {
        async get(url) { requests.push(['GET', url]); return snapshot(); },
        async post(url) { requests.push(['POST', url]); if (fail) throw new Error('audit failed'); return { report: { verdict: 'none' } }; },
    });
    client.setQueryData(scheduledKey, snapshot());
    client.setQueryData(anomaliesKey, { findings: [] });
    client.setQueryData(['unrelated'], 'keep');
    const observer = new QueryObserver(client, api.useScheduledAudit());
    const unsubscribe = observer.subscribe(() => {});
    t.after(() => { unsubscribe(); client.clear(); });
    const mutation = new MutationObserver(client, api.useRunModelAudit());
    const input = { channel_id: 7, model: 'test-model' };
    await assert.rejects(mutation.mutate(input), /audit failed/);
    assert.equal(client.getQueryState(scheduledKey).isInvalidated, false);
    assert.equal(client.getQueryState(anomaliesKey).isInvalidated, false);
    assert.equal(requests.filter(([method]) => method === 'GET').length, 0);

    fail = false;
    await mutation.mutate(input);
    assert.equal(requests.filter(([method]) => method === 'GET').length, 1, 'active saved query refetches immediately');
    assert.equal(client.getQueryState(anomaliesKey).isInvalidated, true, 'inactive related query becomes stale');
    assert.equal(client.getQueryState(['unrelated']).isInvalidated, false);
    assert.deepEqual(client.getQueryData(scheduledKey).results, [], 'manual report is not a scheduled snapshot');
    assert.equal(requests.filter(([method]) => method === 'POST').length, 2, 'only explicit mutations run audits');
});
