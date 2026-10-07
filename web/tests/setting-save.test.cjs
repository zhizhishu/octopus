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
        mutate(variables, callbacks) {
            this.writes.push({ variables, callbacks });
            this.isPending = true;
            this.variables = variables;
        },
        mutateAsync(variables) {
            this.writes.push({ variables, callbacks: null });
            this.isPending = true;
            this.variables = variables;
            return new Promise(() => {});
        },
        settle(ok) {
            const last = this.writes[this.writes.length - 1];
            assert.ok(last, 'settle() needs a pending write');
            this.isPending = false;
            const error = new Error('save rejected');
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
