// Run from web/: node --test tests/setting-save.test.cjs
// Covers System.tsx/Log.tsx setting-page regressions:
// A  upstreamHeaderTimeout loads on its own key (not nested under dataTimeout)
// B  failed saves are visible (toast) and never fake success; values roll back
// C  a pending save blocks further submits (single-flight via useMutation state)
// D  the no-breaker retry budget clamps to 300 seconds
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

function findElement(node, predicate) {
    if (Array.isArray(node)) return node.map((child) => findElement(child, predicate)).find(Boolean);
    if (!node || typeof node !== 'object') return undefined;
    return predicate(node) ? node : findElement(node.props?.children, predicate);
}

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));

function makeMutation() {
    return {
        writes: [], isPending: false, variables: undefined, options: null,
        asyncPromises: [],
        mutate(variables, callbacks) {
            this.writes.push({ variables, callbacks });
            this.isPending = true;
            this.variables = variables;
        },
        mutateAsync(variables) {
            this.writes.push({ variables, callbacks: null });
            this.isPending = true;
            this.variables = variables;
            this.asyncPromises.push(new Promise((resolve, reject) => {
                this.resolveAsync = resolve;
                this.rejectAsync = reject;
            }));
            return this.asyncPromises[this.asyncPromises.length - 1];
        },
        settleAsync(ok, error = new Error('save rejected')) {
            const last = this.writes[this.writes.length - 1];
            assert.ok(last, 'settleAsync() needs a pending write');
            this.isPending = false;
            if (ok) {
                this.options?.onSuccess?.(last.variables);
                this.resolveAsync?.(last.variables);
            } else {
                this.options?.onError?.(error);
                this.rejectAsync?.(error);
            }
        },
        settle(ok, error = new Error('save rejected')) {
            const last = this.writes[this.writes.length - 1];
            assert.ok(last, 'settle() needs a pending write');
            this.isPending = false;
            if (ok) {
                this.options?.onSuccess?.(last.variables);
                last.callbacks?.onSuccess?.(last.variables);
            } else {
                this.options?.onError?.(error);
                last.callbacks?.onError?.(error);
            }
        },
    };
}

// Loads the real component with the real setting.ts hook module; query/mutation
// state is driven by hand (same approach as route-default.test.cjs).
function makeHarness(componentPath, exportName, settings, extraMocks) {
    const mutation = makeMutation();
    const queryState = { data: settings, isSuccess: true, isFetching: false, isError: false };
    const queryClient = { invalidations: 0, invalidateQueries() { this.invalidations++; }, setQueryData() {} };
    const settingModule = load('src/api/endpoints/setting.ts', {
        '@tanstack/react-query': {
            useQuery: () => queryState,
            useQueryClient: () => queryClient,
            useMutation: (options) => {
                mutation.options = options;
                return {
                    mutate: (variables, callbacks) => mutation.mutate(variables, callbacks),
                    mutateAsync: (variables) => mutation.mutateAsync(variables),
                    get isPending() { return mutation.isPending; },
                    get variables() { return mutation.variables; },
                };
            },
        },
        '../client': { apiClient: {}, API_BASE_URL: '.' },
        '@/lib/logger': { logger: { log() {}, error() {} } },
        '@/components/common/Toast': { toast: { success() {}, error() {} } },
        './user': { useAuthStore: {} },
    });
    const slots = [];
    const queuedEffects = [];
    const notes = [];
    let cursor = 0;
    const react = {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            const set = (value) => { slot.value = typeof value === 'function' ? value(slot.value) : value; };
            slot.set = set;
            return [slot.value, set];
        },
        useRef(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { current: initial };
            return slots[i];
        },
        useMemo(fn, deps) {
            const i = cursor++;
            if (!slots[i] || deps.some((d, j) => !Object.is(d, slots[i].deps[j]))) slots[i] = { deps, value: fn() };
            return slots[i].value;
        },
        // Effects run like React: only when their deps change between renders.
        useEffect(fn, deps) {
            const i = cursor++;
            if (!slots[i] || deps.some((d, j) => !Object.is(d, slots[i].deps[j]))) {
                slots[i] = { deps };
                queuedEffects.push(fn);
            }
        },
    };
    const Component = load(componentPath, {
        react,
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/setting': settingModule,
        '@/components/common/Toast': { toast: {
            success: (m) => notes.push(['success', m]),
            error: (m) => notes.push(['error', m]),
        } },
        ...extraMocks,
    })[exportName];
    let tree;
    return {
        mutation, notes,
        get tree() { return tree; },
        // Simulates the 30s polling tick: the server returns a (new) settings array.
        setSettings(next) { queryState.data = next; },
        async render() {
            cursor = 0;
            queuedEffects.length = 0;
            tree = Component();
            for (const effect of queuedEffects.splice(0)) effect();
            await Promise.resolve(); // flush the queueMicrotask setState chain
            await Promise.resolve();
            cursor = 0;
            queuedEffects.length = 0;
            tree = Component();
            return tree;
        },
        inputByLabel(label) { return findElement(tree, (n) => n.type === 'Input' && n.props?.['aria-label'] === label); },
        inputByPlaceholder(placeholder) { return findElement(tree, (n) => n.type === 'Input' && n.props?.placeholder === placeholder); },
        // All rendered inputs, in document order (for value-keyed lookups in label-less trees).
        inputs() {
            const found = [];
            const walk = (node) => {
                if (Array.isArray(node)) return node.forEach(walk);
                if (!node || typeof node !== 'object') return;
                if (node.type === 'Input') found.push(node);
                walk(node.props?.children);
            };
            walk(tree);
            return found;
        },
        inputByValue(value) { return this.inputs().find((n) => String(n.props?.value) === String(value)); },
        selectByLabel(label) { return findElement(tree, (n) => n.type === 'select' && n.props?.['aria-label'] === label); },
        switchByLabel(label) { return findElement(tree, (n) => n.type === 'Switch' && n.props?.['aria-label'] === label); },
        // Log.tsx has no labelled inputs; the no-breaker budget is the last one.
        lastInput() {
            let last;
            const walk = (node) => {
                if (Array.isArray(node)) return node.forEach(walk);
                if (!node || typeof node !== 'object') return;
                if (node.type === 'Input') last = node;
                walk(node.props?.children);
            };
            walk(tree);
            return last;
        },
    };
}

const lucide = new Proxy({}, { get: (_, key) => key });
const systemMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/switch': widgets('Switch'),
    '@/components/ui/button': widgets('Button'),
    '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogFooter DialogHeader DialogTitle'),
    '@/components/ui/popover': widgets('Popover PopoverContent PopoverTrigger'),
    '@/components/animate-ui/components/animate/tooltip': widgets('Tooltip TooltipContent TooltipProvider TooltipTrigger'),
    'lucide-react': lucide,
};
const logMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/switch': widgets('Switch'),
    '@/components/ui/button': widgets('Button'),
    '@/components/ui/progress': widgets('Progress'),
    '@/api/endpoints/log': {
        useClearLogs: () => ({ mutate() {}, isPending: false }),
        useLogStorage: () => ({ data: undefined }),
        useLogQueueHealth: () => ({ data: undefined }),
    },
    'lucide-react': lucide,
};
const makeSystemHarness = (settings) => makeHarness('src/components/modules/setting/System.tsx', 'SettingSystem', settings, systemMocks);
const makeLogHarness = (settings) => makeHarness('src/components/modules/setting/Log.tsx', 'SettingLog', settings, logMocks);
const cbMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/animate-ui/components/animate/tooltip': widgets('Tooltip TooltipContent TooltipProvider TooltipTrigger'),
    'lucide-react': lucide,
};
const llmSyncMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/button': widgets('Button'),
    '@/api/endpoints/channel': {
        useSyncChannel: () => ({ mutate() {}, isPending: false }),
        useLastSyncTime: () => ({ data: undefined }),
    },
    'lucide-react': lucide,
};
const priceCatalogMocks = {
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/button': widgets('Button'),
    '@/components/common/MobileFilterCollapse': { MobileFilterCollapse: 'MobileFilterCollapse' },
    '@/api/endpoints/model': {
        useUpdateModelPrice: () => ({ mutate() {}, isPending: false }),
        useLastUpdateTime: () => ({ data: undefined, isFetching: false, refetch() {} }),
    },
    'lucide-react': lucide,
};
const makeCbHarness = (settings) => makeHarness('src/components/modules/setting/CircuitBreaker.tsx', 'SettingCircuitBreaker', settings, cbMocks);
const makeLlmSyncHarness = (settings) => makeHarness('src/components/modules/setting/LLMSync.tsx', 'SettingLLMSync', settings, llmSyncMocks);
const makePriceCatalogHarness = (settings) => makeHarness('src/components/modules/model/PriceCatalog.tsx', 'ModelPriceCatalog', settings, priceCatalogMocks);

test('A: upstreamHeaderTimeout loads on its own key while the adjacent dataTimeout key is missing', async () => {
    const h = makeSystemHarness([{ key: 'upstream_header_timeout_seconds', value: '123' }]);
    await h.render();
    const input = h.inputByLabel('upstreamHeaderTimeout.label');
    assert.ok(input, 'upstreamHeaderTimeout input rendered');
    assert.equal(String(input.props.value), '123', 'must load from its own key without dataTimeout present');
});

test('B: numeric save failure toasts an error and rolls the input back to the confirmed value', async () => {
    const h = makeSystemHarness([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onChange({ target: { value: '30' } });
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onBlur();
    assert.deepEqual(h.mutation.writes.map((w) => w.variables), [{ key: 'relay_stream_keepalive_interval_seconds', value: '30' }]);
    h.mutation.settle(false);
    assert.ok(h.notes.some(([kind]) => kind === 'error'), 'failure must be visible');
    assert.ok(!h.notes.some(([kind]) => kind === 'success'), 'failure must not fake success');
    await h.render();
    assert.equal(String(h.inputByLabel('streamKeepalive.label').props.value), '15', 'unconfirmed value rolls back');
});

test('B: numeric save success still confirms, updates the baseline, and does not resubmit', async () => {
    const h = makeSystemHarness([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onChange({ target: { value: '30' } });
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onBlur();
    h.mutation.settle(true);
    assert.ok(h.notes.some(([kind]) => kind === 'success'));
    await h.render();
    assert.equal(String(h.inputByLabel('streamKeepalive.label').props.value), '30');
    h.inputByLabel('streamKeepalive.label').props.onBlur();
    assert.equal(h.mutation.writes.length, 1, 'unchanged confirmed value is a no-op');
});

test('B: switch save failure toasts, rolls back, and retries without a no-op deadlock', async () => {
    const h = makeSystemHarness([{ key: 'user_registration_enabled', value: 'false' }]);
    await h.render();
    const sw = () => h.switchByLabel('userRegistration.label');
    assert.equal(sw().props.checked, false);
    sw().props.onCheckedChange(true);
    assert.deepEqual(h.mutation.writes.map((w) => w.variables), [{ key: 'user_registration_enabled', value: 'true' }]);
    h.mutation.settle(false);
    assert.ok(h.notes.some(([kind]) => kind === 'error'), 'failure must be visible');
    await h.render();
    assert.equal(sw().props.checked, false, 'failed switch rolls back');
    sw().props.onCheckedChange(true);
    assert.equal(h.mutation.writes.length, 2, 'retry after failure reaches the wire again');
    h.mutation.settle(true);
    await h.render();
    assert.equal(sw().props.checked, true);
});

test('C: while a save is pending, further blurs cannot stack submits or overwrite the in-flight value', async () => {
    const h = makeSystemHarness([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onChange({ target: { value: '30' } });
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onBlur();
    assert.equal(h.mutation.writes.length, 1);
    assert.equal(h.mutation.isPending, true);

    h.inputByLabel('streamKeepalive.label').props.onChange({ target: { value: '45' } });
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onBlur();
    assert.equal(h.mutation.writes.length, 1, 'pending save blocks a second submit of the same field');

    h.inputByPlaceholder('statsSaveInterval.placeholder').props.onChange({ target: { value: '120' } });
    await h.render();
    h.inputByPlaceholder('statsSaveInterval.placeholder').props.onBlur();
    assert.equal(h.mutation.writes.length, 1, 'pending save blocks submits of other fields too (shared single-flight)');

    h.mutation.settle(true);
    await h.render();
    h.inputByLabel('streamKeepalive.label').props.onBlur();
    assert.deepEqual(h.mutation.writes[1].variables, { key: 'relay_stream_keepalive_interval_seconds', value: '45' },
        'the newer local edit can be submitted once the first save lands');
});

test('D: no-breaker retry budget clamps to the 300-second ceiling', async () => {
    // 存量 600 是旧上限留下的真实脏值：保存时必须被钳到 300，而不能原样回写。
    const h = makeLogHarness([{ key: 'relay_no_breaker_retry_budget_seconds', value: '600' }]);
    await h.render();
    assert.equal(String(h.lastInput().props.value), '600');
    assert.equal(String(h.lastInput().props.max), '300', 'input max attribute matches the new ceiling');
    const submit = async (raw) => {
        h.lastInput().props.onChange({ target: { value: raw } });
        await h.render();
        h.lastInput().props.onBlur();
        await h.render(); // the blur-triggered setState re-renders, like React would
    };
    await submit('600');
    assert.equal(h.mutation.writes[0].variables.value, '300', 'stored legacy 600 clamps to 300 on save');
    h.mutation.settle(true);
    await h.render();
    await submit('999');
    assert.equal(h.mutation.writes.length, 1, '999 normalizes to 300 which equals the confirmed value: no-op');
    assert.equal(String(h.lastInput().props.value), '300', 'display still normalizes to 300, not 600');
    h.mutation.settle(true);
    await submit('50');
    assert.equal(h.mutation.writes[1].variables.value, '50', 'in-range value passes through');
    h.mutation.settle(true);
    await submit('-5');
    assert.equal(h.mutation.writes[2].variables.value, '0', 'negative clamps to 0');
    h.mutation.settle(true);
    await submit('abc');
    assert.equal(h.mutation.writes[3].variables.value, '300', 'unparsable falls back to the 300 default');
    h.mutation.settle(true);
    await h.render();
    assert.equal(String(h.lastInput().props.value), '300');
});

// -----------------------------------------------------------------------------
// U10/U11/U12 additions below. Existing tests above stay untouched.
// -----------------------------------------------------------------------------

const errorClasses = [
    ['401', Object.assign(new Error('unauthorized'), { code: 401 })],
    ['403', Object.assign(new Error('forbidden'), { code: 403 })],
    ['500', Object.assign(new Error('boom'), { code: 500 })],
    ['offline', new TypeError('fetch failed')],
];

test('U10: circuit breaker numeric save failure toasts once and rolls the input back to the confirmed value', async () => {
    for (const [label, error] of errorClasses) {
        const h = makeCbHarness([
            { key: 'circuit_breaker_threshold', value: '50' },
            { key: 'circuit_breaker_cooldown', value: '60' },
            { key: 'circuit_breaker_max_cooldown', value: '600' },
        ]);
        await h.render();
        h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '80' } });
        await h.render();
        h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
        assert.deepEqual(h.mutation.writes.map((w) => w.variables), [{ key: 'circuit_breaker_threshold', value: '80' }], `${label}: write reaches the wire`);
        h.mutation.settle(false, error);
        assert.equal(h.notes.filter(([kind]) => kind === 'error').length, 1, `${label}: exactly one error toast`);
        assert.ok(!h.notes.some(([kind]) => kind === 'success'), `${label}: failure must not fake success`);
        await h.render();
        assert.equal(String(h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.value), '50', `${label}: numeric input rolls back`);
        h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '80' } });
        await h.render();
        h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
        assert.equal(h.mutation.writes.length, 2, `${label}: retry after failure reaches the wire again (no no-op deadlock)`);
    }
});

test('U10: circuit breaker save success confirms, updates the baseline, and a re-blur is a no-op', async () => {
    const h = makeCbHarness([{ key: 'circuit_breaker_threshold', value: '50' }]);
    await h.render();
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '80' } });
    await h.render();
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
    h.mutation.settle(true);
    assert.ok(h.notes.some(([kind]) => kind === 'success'), 'success must be visible');
    await h.render();
    assert.equal(String(h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.value), '80');
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
    assert.equal(h.mutation.writes.length, 1, 'unchanged confirmed value is a no-op');
});

test('U10: llm sync interval save failure keeps the draft and toasts once', async () => {
    for (const [label, error] of errorClasses) {
        const h = makeLlmSyncHarness([{ key: 'sync_llm_interval', value: '60' }]);
        await h.render();
        h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onChange({ target: { value: '120' } });
        await h.render();
        h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onBlur();
        assert.deepEqual(h.mutation.writes.map((w) => w.variables), [{ key: 'sync_llm_interval', value: '120' }], `${label}: write reaches the wire`);
        h.mutation.settle(false, error);
        assert.equal(h.notes.filter(([kind]) => kind === 'error').length, 1, `${label}: exactly one error toast`);
        assert.ok(!h.notes.some(([kind]) => kind === 'success'), `${label}: failure must not fake success`);
        await h.render();
        assert.equal(String(h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.value), '120', `${label}: failed draft is kept (retry by re-blur)`);
    }
});

test('U10: llm sync interval save success confirms and does not resubmit', async () => {
    const h = makeLlmSyncHarness([{ key: 'sync_llm_interval', value: '60' }]);
    await h.render();
    h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onChange({ target: { value: '120' } });
    await h.render();
    h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onBlur();
    h.mutation.settle(true);
    assert.ok(h.notes.some(([kind]) => kind === 'success'));
    await h.render();
    assert.equal(String(h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.value), '120');
    h.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onBlur();
    assert.equal(h.mutation.writes.length, 1, 'unchanged confirmed value is a no-op');
});

test('U10: price update interval save failure keeps the draft and never touches the manual-update toast path', async () => {
    for (const [label, error] of errorClasses) {
        const h = makePriceCatalogHarness([{ key: 'model_info_update_interval', value: '3600' }]);
        await h.render();
        h.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onChange({ target: { value: '7200' } });
        await h.render();
        h.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onBlur();
        assert.deepEqual(h.mutation.writes.map((w) => w.variables), [{ key: 'model_info_update_interval', value: '7200' }], `${label}: write reaches the wire`);
        h.mutation.settle(false, error);
        assert.equal(h.notes.filter(([kind]) => kind === 'error').length, 1, `${label}: exactly one error toast`);
        assert.ok(!h.notes.some(([kind]) => kind === 'success'), `${label}: failure must not fake success`);
        await h.render();
        assert.equal(String(h.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.value), '7200', `${label}: failed draft is kept`);
    }
});

test('U10: pending save blocks further blurs across fields (circuit breaker / llm sync / price catalog)', async () => {
    const cb = makeCbHarness([{ key: 'circuit_breaker_threshold', value: '50' }]);
    await cb.render();
    cb.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '80' } });
    await cb.render();
    cb.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
    assert.equal(cb.mutation.writes.length, 1);
    cb.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '90' } });
    await cb.render();
    cb.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
    assert.equal(cb.mutation.writes.length, 1, 'pending save blocks a second submit');

    const llm = makeLlmSyncHarness([{ key: 'sync_llm_interval', value: '60' }]);
    await llm.render();
    llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onChange({ target: { value: '120' } });
    await llm.render();
    llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onBlur();
    assert.equal(llm.mutation.writes.length, 1);
    llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onChange({ target: { value: '240' } });
    await llm.render();
    llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onBlur();
    assert.equal(llm.mutation.writes.length, 1, 'pending save blocks a second submit');

    const pc = makePriceCatalogHarness([{ key: 'model_info_update_interval', value: '3600' }]);
    await pc.render();
    pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onChange({ target: { value: '7200' } });
    await pc.render();
    pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onBlur();
    assert.equal(pc.mutation.writes.length, 1);
    pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onChange({ target: { value: '9000' } });
    await pc.render();
    pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onBlur();
    assert.equal(pc.mutation.writes.length, 1, 'pending save blocks a second submit');
});

test('U11: system numeric save survives 401/403/500/offline with one error toast and rollback', async () => {
    for (const [label, error] of errorClasses) {
        const h = makeSystemHarness([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
        await h.render();
        h.inputByLabel('streamKeepalive.label').props.onChange({ target: { value: '30' } });
        await h.render();
        h.inputByLabel('streamKeepalive.label').props.onBlur();
        h.mutation.settle(false, error);
        assert.equal(h.notes.filter(([kind]) => kind === 'error').length, 1, `${label}: exactly one error toast`);
        assert.ok(!h.notes.some(([kind]) => kind === 'success'), `${label}: failure must not fake success`);
        await h.render();
        assert.equal(String(h.inputByLabel('streamKeepalive.label').props.value), '15', `${label}: numeric rollback`);
    }
});

test('U11: log text-key save failure keeps the draft across the same error classes', async () => {
    for (const [label, error] of errorClasses) {
        const h = makeLogHarness([{ key: 'relay_log_keep_period', value: '7' }]);
        await h.render();
        h.inputByPlaceholder('log.keepPeriod.placeholder').props.onChange({ target: { value: '30' } });
        await h.render();
        h.inputByPlaceholder('log.keepPeriod.placeholder').props.onBlur();
        h.mutation.settle(false, error);
        assert.equal(h.notes.filter(([kind]) => kind === 'error').length, 1, `${label}: exactly one error toast`);
        await h.render();
        assert.equal(String(h.inputByPlaceholder('log.keepPeriod.placeholder').props.value), '30', `${label}: text draft is kept`);
    }
});

test('U12: 30s poll with unchanged data does not clobber an in-progress edit (circuit breaker)', async () => {
    const h = makeCbHarness([{ key: 'circuit_breaker_threshold', value: '50' }]);
    await h.render();
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '80' } });
    await h.render();
    h.setSettings([{ key: 'circuit_breaker_threshold', value: '50' }]);
    await h.render();
    assert.equal(String(h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.value), '80', 'same-value poll must keep the draft');
});

test('U12: poll with unchanged data keeps the failed-save draft (log keepPeriod)', async () => {
    const h = makeLogHarness([{ key: 'relay_log_keep_period', value: '7' }]);
    await h.render();
    h.inputByPlaceholder('log.keepPeriod.placeholder').props.onChange({ target: { value: '30' } });
    await h.render();
    h.inputByPlaceholder('log.keepPeriod.placeholder').props.onBlur();
    h.mutation.settle(false);
    h.setSettings([{ key: 'relay_log_keep_period', value: '7' }]);
    await h.render();
    assert.equal(String(h.inputByPlaceholder('log.keepPeriod.placeholder').props.value), '30', 'same-value poll must keep the failed draft');
});

test('U12: poll with unchanged data keeps the draft (system streamKeepalive / llm sync / price catalog)', async () => {
    const sys = makeSystemHarness([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
    await sys.render();
    sys.inputByLabel('streamKeepalive.label').props.onChange({ target: { value: '45' } });
    await sys.render();
    sys.setSettings([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
    await sys.render();
    assert.equal(String(sys.inputByLabel('streamKeepalive.label').props.value), '45', 'same-value poll must keep the draft');

    const llm = makeLlmSyncHarness([{ key: 'sync_llm_interval', value: '60' }]);
    await llm.render();
    llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.onChange({ target: { value: '120' } });
    await llm.render();
    llm.setSettings([{ key: 'sync_llm_interval', value: '60' }]);
    await llm.render();
    assert.equal(String(llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.value), '120', 'same-value poll must keep the draft');

    const pc = makePriceCatalogHarness([{ key: 'model_info_update_interval', value: '3600' }]);
    await pc.render();
    pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.onChange({ target: { value: '7200' } });
    await pc.render();
    pc.setSettings([{ key: 'model_info_update_interval', value: '3600' }]);
    await pc.render();
    assert.equal(String(pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.value), '7200', 'same-value poll must keep the draft');
});

test('U12: external server change still overwrites the displayed value', async () => {
    const sys = makeSystemHarness([{ key: 'relay_stream_keepalive_interval_seconds', value: '15' }]);
    await sys.render();
    sys.setSettings([{ key: 'relay_stream_keepalive_interval_seconds', value: '20' }]);
    await sys.render();
    assert.equal(String(sys.inputByLabel('streamKeepalive.label').props.value), '20', 'a real server change is applied');

    const cb = makeCbHarness([{ key: 'circuit_breaker_threshold', value: '50' }]);
    await cb.render();
    cb.setSettings([{ key: 'circuit_breaker_threshold', value: '60' }]);
    await cb.render();
    assert.equal(String(cb.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.value), '60', 'a real server change is applied');

    const log = makeLogHarness([{ key: 'relay_log_keep_period', value: '7' }]);
    await log.render();
    log.setSettings([{ key: 'relay_log_keep_period', value: '14' }]);
    await log.render();
    assert.equal(String(log.inputByPlaceholder('log.keepPeriod.placeholder').props.value), '14', 'a real server change is applied');

    const llm = makeLlmSyncHarness([{ key: 'sync_llm_interval', value: '60' }]);
    await llm.render();
    llm.setSettings([{ key: 'sync_llm_interval', value: '600' }]);
    await llm.render();
    assert.equal(String(llm.inputByPlaceholder('llmSync.syncInterval.placeholder').props.value), '600', 'a real server change is applied');

    const pc = makePriceCatalogHarness([{ key: 'model_info_update_interval', value: '3600' }]);
    await pc.render();
    pc.setSettings([{ key: 'model_info_update_interval', value: '86400' }]);
    await pc.render();
    assert.equal(String(pc.inputByPlaceholder('llmPrice.updateInterval.placeholder').props.value), '86400', 'a real server change is applied');
});

test('U12: a newer local edit survives a post-save refetch of the same value', async () => {
    const h = makeCbHarness([{ key: 'circuit_breaker_threshold', value: '50' }]);
    await h.render();
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '80' } });
    await h.render();
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onBlur();
    h.mutation.settle(true);
    // The refetch lands with the just-saved value, then the user edits again before/without another save.
    h.setSettings([{ key: 'circuit_breaker_threshold', value: '80' }]);
    await h.render();
    h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.onChange({ target: { value: '95' } });
    await h.render();
    h.setSettings([{ key: 'circuit_breaker_threshold', value: '80' }]);
    await h.render();
    assert.equal(String(h.inputByPlaceholder('circuitBreaker.threshold.placeholder').props.value), '95', 'refetch of the confirmed value must not eat the newer edit');
});

test('U12: seeded sentinel values load on every System string key on first render (state/ref pairing guard)', async () => {
    // One pair per string setting: [server key, unique sentinel value, optional placeholder key].
    // cors_allow_origins is covered separately (no input bound to its value).
    const pairs = [
        ['proxy_url', 'http://proxy-sentinel:1', 'proxyUrl.placeholder'],
        ['trusted_proxies', '10.0.0.sentinel/32', null],
        ['stats_save_interval', '60', 'statsSaveInterval.placeholder'],
        ['relay_stream_keepalive_interval_seconds', '12', 'streamKeepalive.placeholder'],
        ['relay_stream_data_interval_timeout_seconds', '77', 'streamDataTimeout.placeholder'],
        ['upstream_header_timeout_seconds', '88', 'upstreamHeaderTimeout.placeholder'],
        ['first_byte_keepalive_delay_seconds', '89', 'firstByteKeepalive.placeholder'],
        ['responses_session_ttl_seconds', '99', 'responsesSessionTTL.placeholder'],
        ['claude_beta_strip_flags', 'strip-me', null],
        ['upstream_error_custom_message', 'custom-message-sentinel', null],
        ['upstream_error_public_code', 'public-code-sentinel', null],
        ['checkin_reward_amount', '111', null],
        ['checkin_reward_min', '112', null],
        ['checkin_reward_max', '113', null],
        ['email_smtp_host', 'smtp.example', null],
        ['email_smtp_port', '2525', 'emailVerification.portPlaceholder'],
        ['email_smtp_user', 'user-sentinel', null],
        ['email_smtp_password', 'secret-sentinel', null],
        ['email_smtp_from', 'from-sentinel', null],
        ['email_smtp_from_name', 'FromName-sentinel', null],
    ];
    const selectSeeds = [
        ['claude_cli_reasoning_effort', 'high', 'claudeCLIReasoningEffort.label'],
        ['email_provider', 'smtp', 'emailVerification.provider'],
        ['upstream_error_body_mode', 'custom_message', 'upstreamError.bodyMode'],
        ['checkin_reward_mode', 'random', 'checkIn.rewardMode'],
    ];
    const baseSettings = pairs.map(([key, value]) => ({ key, value }))
        .concat(selectSeeds.map(([key, value]) => ({ key, value })))
        .concat([{ key: 'cors_allow_origins', value: 'https://sentinel.example' }]);
    const h = makeSystemHarness(baseSettings);
    await h.render();
    const locate = ([key, sentinel, placeholder]) =>
        placeholder ? h.inputByPlaceholder(placeholder) : h.inputByValue(sentinel);
    for (const pair of pairs) {
        assert.ok(locate(pair), `${pair[0]}: seeded value must load into the matching input on first render`);
    }
    for (const [key, seed, label] of selectSeeds) {
        const select = h.selectByLabel(label);
        assert.ok(select, `${key}: seeded select value must load on first render`);
        assert.equal(String(select.props.value), seed, `${key}: sentinel seeds the select`);
    }
    assert.ok(findElement(h.tree, (n) => n.type === 'button' && n.props?.title === 'https://sentinel.example'),
        'cors_allow_origins: seeded origin must load into the chip trigger title');
});

test('U12: every System string key keeps its failed draft against a same-value poll (per-key guard)', async () => {
    // Same seeds as the sentinel test; each key drafts a different value, the save fails,
    // and a same-value poll (fresh array) must not restore the server value over the draft.
    const pairs = [
        ['proxy_url', 'http://proxy-sentinel:1', 'proxyUrl.placeholder'],
        ['trusted_proxies', '10.0.0.sentinel/32', null],
        ['stats_save_interval', '60', 'statsSaveInterval.placeholder'],
        ['relay_stream_keepalive_interval_seconds', '12', 'streamKeepalive.placeholder'],
        ['relay_stream_data_interval_timeout_seconds', '77', 'streamDataTimeout.placeholder'],
        ['upstream_header_timeout_seconds', '88', 'upstreamHeaderTimeout.placeholder'],
        ['first_byte_keepalive_delay_seconds', '89', 'firstByteKeepalive.placeholder'],
        ['responses_session_ttl_seconds', '99', 'responsesSessionTTL.placeholder'],
        ['claude_beta_strip_flags', 'strip-me', null],
        ['upstream_error_custom_message', 'custom-message-sentinel', null],
        ['upstream_error_public_code', 'public-code-sentinel', null],
        ['checkin_reward_amount', '111', null],
        ['checkin_reward_min', '112', null],
        ['checkin_reward_max', '113', null],
        ['email_smtp_host', 'smtp.example', null],
        ['email_smtp_port', '2525', 'emailVerification.portPlaceholder'],
        ['email_smtp_user', 'user-sentinel', null],
        ['email_smtp_password', 'secret-sentinel', null],
        ['email_smtp_from', 'from-sentinel', null],
        ['email_smtp_from_name', 'FromName-sentinel', null],
    ];
    const numericValidated = new Set([
        'relay_stream_keepalive_interval_seconds', 'relay_stream_data_interval_timeout_seconds',
        'upstream_header_timeout_seconds', 'first_byte_keepalive_delay_seconds',
        'responses_session_ttl_seconds',
    ]);
    const selectSeeds = [
        ['claude_cli_reasoning_effort', 'high', 'claudeCLIReasoningEffort.label', 'medium'],
        ['email_provider', 'smtp', 'emailVerification.provider', 'http'],
        ['upstream_error_body_mode', 'custom_message', 'upstreamError.bodyMode', 'octopus_standard'],
        ['checkin_reward_mode', 'random', 'checkIn.rewardMode', 'fixed'],
    ];
    const httpPairs = [
        ['email_http_base_url', 'emailVerification.httpBaseUrlPlaceholder'],
        ['email_http_from', 'emailVerification.httpFromPlaceholder'],
        ['email_http_admin_auth', 'emailVerification.httpAdminAuthPlaceholder'],
        ['email_http_site_auth', 'emailVerification.httpSiteAuthPlaceholder'],
    ];
    const baseSettings = pairs.map(([key, value]) => ({ key, value }))
        .concat(selectSeeds.map(([key, value]) => ({ key, value })))
        .concat([{ key: 'cors_allow_origins', value: 'https://sentinel.example' }]);
    const poll = () => h.setSettings(baseSettings.map((s) => ({ ...s })));
    const h = makeSystemHarness(baseSettings);
    await h.render();
    const locate = ([key, sentinel, placeholder]) =>
        placeholder ? h.inputByPlaceholder(placeholder) : h.inputByValue(sentinel);
    // 1) plain input keys (provider still smtp). Numeric-validated keys roll back by
    // design on failure, so their guard is exercised with a NEW in-progress edit;
    // text keys must keep the failed draft itself.
    for (const pair of pairs) {
        const [key, sentinel, placeholder] = pair;
        const input = locate(pair);
        assert.ok(input, `${key}: input available for save flow`);
        const draft = numericValidated.has(key) ? String(Number(sentinel) + 3) : `${sentinel}-edit`;
        input.props.onChange({ target: { value: draft } });
        await h.render();
        const blurred = placeholder ? h.inputByPlaceholder(placeholder) : h.inputByValue(draft);
        blurred.props.onBlur();
        assert.deepEqual(h.mutation.writes[h.mutation.writes.length - 1].variables, { key, value: draft }, `${key}: draft reaches the wire`);
        h.mutation.settle(false);
        let expect;
        if (numericValidated.has(key)) {
            expect = String(Number(draft) + 1);
            input.props.onChange({ target: { value: expect } });
            await h.render();
        } else {
            expect = draft;
        }
        poll();
        await h.render();
        const shown = placeholder ? h.inputByPlaceholder(placeholder) : h.inputByValue(expect);
        assert.ok(shown, `${key}: same-value poll must not clobber the draft`);
    }
    // 2) select keys (the email_provider flip renders the http-email inputs)
    for (const [key, seed, label, next] of selectSeeds) {
        const select = h.selectByLabel(label);
        select.props.onChange({ target: { value: next } });
        await h.render();
        assert.deepEqual(h.mutation.writes[h.mutation.writes.length - 1].variables, { key, value: next }, `${key}: change reaches the wire`);
        h.mutation.settle(false);
        poll();
        await h.render();
        assert.equal(String(h.selectByLabel(label).props.value), next, `${key}: same-value poll must not clobber the failed draft`);
    }
    // 3) http-email inputs (provider=http now)
    for (const [key, placeholder] of httpPairs) {
        const input = h.inputByPlaceholder(placeholder);
        assert.ok(input, `${key}: input must render (provider=http)`);
        const draft = `${key}-draft`;
        input.props.onChange({ target: { value: draft } });
        await h.render();
        h.inputByPlaceholder(placeholder).props.onBlur();
        assert.deepEqual(h.mutation.writes[h.mutation.writes.length - 1].variables, { key, value: draft }, `${key}: draft reaches the wire`);
        h.mutation.settle(false);
        poll();
        await h.render();
        assert.equal(String(h.inputByPlaceholder(placeholder).props.value), draft, `${key}: same-value poll must not clobber the failed draft`);
    }
    // 4) cors: a failed removal followed by a same-value poll must keep the chip.
    const removeBtn = findElement(h.tree, (n) => n.type === 'button' && n.props?.['aria-label'] === 'remove https://sentinel.example');
    assert.ok(removeBtn, 'cors_allow_origins: remove chip button rendered');
    removeBtn.props.onClick();
    assert.deepEqual(h.mutation.writes[h.mutation.writes.length - 1].variables,
        { key: 'cors_allow_origins', value: '' }, 'cors removal reaches the wire');
    h.mutation.settle(false);
    poll();
    await h.render();
    const chipRestored = findElement(h.tree, (n) => n.type === 'button' && n.props?.title === 'https://sentinel.example');
    assert.ok(!chipRestored, 'cors_allow_origins: same-value poll must not restore a failed removal (removal draft kept)');
});

test('U12: header-editor dialog keeps the failed-save draft against a same-value poll (claude/codex keys)', async () => {
    // The 9 header-defaults keys live in the edit dialog only. The draft (headerDraft) is
    // independent of the settings effect; the save path still exercises each key's state/ref
    // pairing (apply() writes both). On failure the dialog stays open and a same-value poll
    // must not disturb the draft; the retry must reach the wire again.
    const cases = [
        ['claude_header_defaults_user_agent', 0],
        ['claude_header_defaults_package_version', 0],
        ['claude_header_defaults_runtime_version', 0],
        ['claude_header_defaults_os', 0],
        ['claude_header_defaults_arch', 0],
        ['claude_header_defaults_timeout', 0],
        ['codex_header_defaults_user_agent', 1],
        ['codex_header_defaults_beta_features', 1],
    ];
    const findAll = (node, predicate, acc = []) => {
        if (Array.isArray(node)) { node.forEach((c) => findAll(c, predicate, acc)); return acc; }
        if (!node || typeof node !== 'object') return acc;
        if (predicate(node)) acc.push(node);
        findAll(node.props?.children, predicate, acc);
        return acc;
    };
    for (const [key, editorIndex] of cases) {
        const h = makeSystemHarness([{ key, value: 'seed-value' }]);
        await h.render();
        const editButtons = findAll(h.tree, (n) => n.type === 'Button' && n.props?.onClick && String(n.props.children).includes('headerDefaults.edit'));
        assert.ok(editButtons.length === 2, `${key}: claude+codex edit buttons rendered`);
        editButtons[editorIndex].props.onClick();
        await h.render();
        const input = h.inputByValue('seed-value');
        assert.ok(input, `${key}: dialog input rendered`);
        input.props.onChange({ target: { value: 'draft-value' } });
        await h.render();
        const saveBtn = findAll(h.tree, (n) => n.type === 'Button' && Array.isArray(n.props.children) && n.props.children.some((c) => c && c.type === 'Check'))[0];
        assert.ok(saveBtn, `${key}: dialog save button rendered`);
        saveBtn.props.onClick();
        await h.render();
        assert.deepEqual(h.mutation.writes.map((w) => w.variables), [{ key, value: 'draft-value' }], `${key}: draft reaches the wire`);
        h.mutation.settleAsync(false);
        // Same-value poll while the failed dialog is still open.
        h.setSettings([{ key, value: 'seed-value' }]);
        await h.render();
        assert.ok(h.inputByValue('draft-value'), `${key}: failed dialog draft must survive the poll`);
        const retrySave = findAll(h.tree, (n) => n.type === 'Button' && Array.isArray(n.props.children) && n.props.children.some((c) => c && c.type === 'Check'))[0];
        retrySave.props.onClick();
        assert.equal(h.mutation.writes.length, 2, `${key}: retry after failure reaches the wire again`);
    }
});

test('locale: three languages carry the new keys (guards against MISSING_MESSAGE)', () => {
    for (const lang of ['zh_hans', 'zh_hant', 'en']) {
        const data = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${lang}.json`), 'utf8'));
        const setting = data.setting;
        assert.ok(setting.saveFailed, `${lang}: setting.saveFailed missing`);
        const log = setting.log;
        assert.ok(log.intervention?.label && log.intervention?.description, `${lang}: log.intervention label/description missing`);
        assert.ok(log.intervention?.timeoutLabel && log.intervention?.timeoutHint, `${lang}: log.intervention timeout texts missing`);
        assert.ok(log.noBreaker?.label && log.noBreaker?.hint, `${lang}: log.noBreaker texts missing`);
    }
});
