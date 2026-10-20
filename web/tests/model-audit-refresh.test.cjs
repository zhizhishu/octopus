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
        // 直接渲染单个 LogCard 并取「模型检测」槽位：回归本行审计字段（手动/历史自动来源）。
        renderAudit(logRow) {
            cardHooks.begin();
            const card = item.LogCard.type({ log: logRow });
            const header = findElement(card, (node) => Object.hasOwn(node.props ?? {}, 'auditSlot'));
            assert.ok(header, 'card variant header must expose the audit slot');
            return header.props.auditSlot;
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
    // 快照本身不再给「本行没有审计字段」的日志贴主动徽标（不拿整个渠道的表现冒充这一条被检测过）。
    saved = snapshot([clean]);
    await tick();
    assert.equal(displayed.label, '', 'a clean snapshot must not paint a badge on a row without its own audit fields');
    assert.strictEqual(displayed.props.log, log, 'id/time and the whole log remain unchanged');
    const stableRenders = displayed.renders;
    await tick();
    assert.equal(displayed.renders, stableRenders, 'identical log props preserve the card render');

    // 快照内容变化（新增/高危/unknown/skipped/移除）都不改变本行徽标。
    for (const next of [
        snapshot([suspicious, clean]),
        snapshot([{ ...clean, verdict: 'unknown', error_count: 1 }, clean]),
        snapshot([{ ...clean, skipped: true }, clean]),
        snapshot([clean]),
        snapshot(),
    ]) {
        saved = next;
        await tick();
        assert.equal(displayed.label, '', 'snapshot state never paints a per-row detect badge');
    }

    const lastData = observer.getCurrentResult().data;
    failure = true;
    await tick();
    assert.equal(observer.getCurrentResult().isError, true);
    assert.strictEqual(observer.getCurrentResult().data, lastData, 'failure does not synthesize new data');
    assert.equal(displayed.label, '', 'failed refresh never paints a row badge');
    failure = false;
    await tick();
    assert.equal(displayed.label, '', 'recovered snapshot still paints no row badge');
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

test('row audit badge reads only its own fields; source separates manual and historical auto', () => {
    const ui = logComponents();
    const rowLog = (overrides = {}) => ({ ...log, ...overrides });

    // 本行无审计字段 -> 无徽标（快照不冒充本行，见上一测试）。
    assert.equal(text(ui.renderAudit(rowLog())), '', 'no own audit fields -> no badge');

    // 未知家族/证据不足仍是「证据不足」，不被升级成风险；来源标明手动。
    const unknown = text(ui.renderAudit(rowLog({ model_audit_verdict: 'unknown', model_audit_trigger: 'manual' })));
    assert.match(unknown, /auditInsufficient/);
    assert.match(unknown, /auditSourceManual/, 'manual source labelled');

    // 历史自动来源与手动不同，不冒充手动。
    const auto = text(ui.renderAudit(rowLog({ model_audit_verdict: 'high', model_audit_trigger: 'schedule' })));
    assert.match(auto, /auditSourceSchedule/);
    assert.doesNotMatch(auto, /auditSourceManual/);

    // 旧口径 echo_mismatch 也归历史自动；未知/空来源标「历史来源未记录」。
    assert.match(text(ui.renderAudit(rowLog({ model_audit_verdict: 'low', model_audit_trigger: 'echo_mismatch' }))), /auditSourceSchedule/);
    assert.match(text(ui.renderAudit(rowLog({ model_audit_verdict: 'none', model_audit_trigger: '' }))), /auditSourceUnknown/);
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

test('run input strips the local AbortSignal from the body and forwards it as request options', async () => {
    const client = new QueryClient({ defaultOptions: { queries: { gcTime: Infinity }, mutations: { gcTime: Infinity } } });
    let captured;
    const api = endpoint(client, {
        async get() { return {}; },
        async post(url, data, params, options) { captured = { url, data, params, options }; return { report: { verdict: 'none' } }; },
    });
    const controller = new AbortController();
    await api.useRunModelAudit().mutationFn({ channel_id: 7, model: 'test-model', probes: ['liveness'], log_id: 42, signal: controller.signal });
    assert.equal(captured.url, '/api/v1/model-audit/run');
    assert.equal('signal' in captured.data, false, 'signal is not part of the network body');
    assert.equal(captured.data.log_id, 42, 'real payload fields survive');
    assert.equal(captured.options.signal, controller.signal, 'signal forwarded as request options');
});

test('apiClient forwards an optional AbortSignal to fetch without changing defaults', async () => {
    const { apiClient } = load('src/api/client.ts', { './types': { HttpStatus: { UNAUTHORIZED: 401 } } });
    const calls = [];
    const originalFetch = global.fetch;
    global.fetch = async (url, init) => {
        calls.push({ url, init });
        return { ok: true, headers: { get: () => 'application/json' }, json: async () => ({ data: { ok: true } }), text: async () => '' };
    };
    try {
        const controller = new AbortController();
        await apiClient.post('/x', { a: 1 }, undefined, { signal: controller.signal });
        assert.equal(calls.length, 1);
        assert.equal(calls[0].init.signal, controller.signal, 'signal reaches fetch');
        assert.equal(calls[0].init.method, 'POST');
        assert.equal(calls[0].init.body, JSON.stringify({ a: 1 }));

        // 不传 signal 时保持默认：signal 为 undefined，不改变任何现有请求行为。
        await apiClient.get('/y');
        assert.equal(calls[1].init.signal, undefined, 'no signal by default');
    } finally {
        global.fetch = originalFetch;
    }
});

// 组件级模拟：直接调用真实 ModelAudit 组件函数，用可重渲染的 hooks 观察状态转换。
// 不新增框架/DOM：只在现有 element-tree 基础上驱动按钮/选项。

// 检测页现在是队列式：先把模型加入队列，再点「开始检测队列」才会真正发请求。
// 旧流程是单选模型直接「开始检查」——入队这一步是新增的、也是防重复计费的关键，
// 所以测试必须走真实路径，不能绕过它直接调发送。
function enqueueAndStart(tree, page) {
    // 1) 若模型还没进队列，先点「加入队列」。点击只改 React 状态，必须重新 render
    //    才能看到队列渲染出来 —— 假 hooks 不重建树，和真实 React 的时序不同。
    let stage = findElement(tree, (n) => n.type === 'Button' && text(n).includes('加入队列'));
    if (stage) stage.props.onClick();
    tree = page.render();
    // 2) 再点开始（按钮文案已变，按关键片段找，避免文案微调就挂测试）。
    const startBtn = findElement(tree, (n) => n.type === 'Button' && text(n).includes('开始检测队列'));
    if (!startBtn) throw new Error('start button not found after enqueue');
    startBtn.props.onClick();
    return tree;
}

function auditPage() {
    const memo = [];
    const refs = [];
    const states = [];
    const effects = [];
    let m = 0, r = 0, s = 0, e = 0;
    const hooksState = {
        begin() { m = 0; r = 0; s = 0; e = 0; },
        react: {
            ...React,
            useMemo(fn, deps) {
                const i = m++;
                const d = deps ?? [];
                if (!memo[i] || !deps || d.some((v, j) => !Object.is(v, memo[i].deps[j]))) memo[i] = { deps: d, value: fn() };
                return memo[i].value;
            },
            useState(initial) {
                const i = s++;
                if (!(i in states)) states[i] = typeof initial === 'function' ? initial() : initial;
                const set = (v) => { states[i] = typeof v === 'function' ? v(states[i]) : v; };
                return [states[i], set];
            },
            useRef(initial) { const i = r++; if (!refs[i]) refs[i] = { current: initial }; return refs[i]; },
            useCallback(fn) { return fn; },
            useEffect(fn) { effects[e++] = fn; },
        },
    };
    const calls = [];
    let channelData = [];
    let mutateImpl = () => new Promise(() => {});
    const mutation = { mutateAsync(vars) { calls.push(vars); return mutateImpl(vars); } };
    const toast = { warning() {}, info() {}, error(...a) { toast.errors.push(a); }, errors: [] };
    const { ModelAudit } = load('src/components/modules/model-audit/index.tsx', {
        react: hooksState.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        'next-intl': { useTranslations: () => (key) => key },
        '@/api/endpoints/channel': { ChannelType: { Chat: 0, Anthropic: 2 }, useChannelList: () => ({ data: channelData, isLoading: false }) },
        '@/api/endpoints/model-audit': {
            AUDIT_PROBE_IDS: ['liveness', 'identity', 'glitch', 'token_delta', 'echo_rewrite', 'context_canary', 'signature'],
            useLogAnomalies: () => ({ data: undefined }),
            useRunModelAudit: () => mutation,
            useScheduledAudit: () => ({ data: undefined }),
        },
        '@/components/common/PageWrapper': widgets('PageWrapper'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/common/Toast': { toast },
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogHeader DialogTitle'),
        '@/lib/utils': { cn: (...p) => p.filter(Boolean).join(' ') },
        '@/components/modules/channel/channel-utils': { getSelectedChannelModels: (channel) => channel.selected_models ?? [] },
        '@/lib/channel-test': { sanitizeChannelTestError: (value) => value },
        // 批量队列页在单次模式的测试里不渲染，给个空存根即可。
        // 它自身的 API hooks 有独立依赖，不该把单次模式的测试拖下水。
        './Queue': { ModelAuditQueue: () => null },
    });
    return {
        calls,
        toast,
        setChannels(next) { channelData = next; },
        setMutate(fn) { mutateImpl = fn; },
        render() { hooksState.begin(); return ModelAudit(); },
        mount() {
            const cleanups = [];
            for (const fn of effects) { const c = fn(); if (typeof c === 'function') cleanups.push(c); }
            return () => cleanups.forEach((c) => c());
        },
    };
}

const grokChannel = [{ raw: { id: 7, name: 'c', enabled: true, type: 2, selected_models: ['grok-4.7'] } }];

// 挂载/改选项发 0 次；点一次=1 次 POST；pending 期间重复点也不再加。
test('active audit page: mount and option changes never call upstream; one click = one POST', () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    let tree = page.render();
    const cleanup = page.mount();
    assert.equal(page.calls.length, 0, 'mount issues no audit call');

    // 改变渠道/计划/探针勾选：均只改本地状态，不发请求。
    findElement(tree, (n) => n.type === 'SearchableSelect' && typeof n.props?.onValueChange === 'function').props.onValueChange('7');
    findElement(tree, (n) => n.type === 'button' && text(n).includes('快速')).props.onClick();
    findElement(tree, (n) => n.type === 'input' && n.props?.type === 'checkbox').props.onChange({ target: { checked: true } });
    assert.equal(page.calls.length, 0, 'option changes never call upstream');

    enqueueAndStart(tree, page);
    assert.equal(page.calls.length, 1, 'one click = one POST');
    assert.equal(page.calls[0].channel_id, 7);
    assert.equal(page.calls[0].probes.includes('signature'), false, 'grok model never selects the signature probe');
    assert.ok(page.calls[0].signal instanceof AbortSignal, 'run carries an AbortSignal');

    tree = page.render();
    const running = findElement(tree, (n) => n.type === 'Button' && text(n).includes('正在检查'));
    assert.ok(running, 'start button shows the running state');
    assert.ok(findElement(tree, (n) => n.type === 'Button' && text(n).includes('cancelCheck')), 'cancel button appears while running');
    running.props.onClick(); // pending 期间再次点击
    assert.equal(page.calls.length, 1, 'double click while pending is ignored');
    cleanup();
});

// silent signature 预筛：Anthropic 渠道 + 明确 Claude 型号才选；不把含 'claude' 子串的假名泛化。
test('signature probe needs both an Anthropic channel and a boundary-matched Claude model', () => {
    const cases = [
        { type: 2, model: 'claude-opus-4-8', expect: true },
        { type: 2, model: 'anthropic/claude-3-5-sonnet', expect: true },
        { type: 2, model: 'grok-4.7', expect: false },
        { type: 2, model: 'claudex-fake', expect: false },
        { type: 2, model: 'notclaude', expect: false },
        { type: 0, model: 'claude-opus-4-8', expect: false },
    ];
    for (const c of cases) {
        const page = auditPage();
        page.setChannels([{ raw: { id: 1, name: 'c', enabled: true, type: c.type, selected_models: [c.model] } }]);
        const tree = page.render();
        enqueueAndStart(tree, page);
        assert.equal((page.calls[0]?.probes ?? []).includes('signature'), c.expect, `${c.model} on type ${c.type}`);
    }
});

// 取消：abort 信号、显示已取消状态、不显示成功报告、不自动重跑、不当作失败。
test('cancel aborts the run signal and shows the cancelled state without success or re-run', async () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    page.setMutate((vars) => new Promise((_, reject) => {
        vars.signal.addEventListener('abort', () => { const e = new Error('aborted'); e.name = 'AbortError'; reject(e); });
    }));
    let tree = page.render();
    const cleanup = page.mount();
    enqueueAndStart(tree, page);
    assert.equal(page.calls.length, 1);
    const signal = page.calls[0].signal;
    assert.equal(signal.aborted, false);

    tree = page.render();
    const cancel = findElement(tree, (n) => n.type === 'Button' && text(n).includes('cancelCheck'));
    assert.ok(cancel, 'cancel button visible while running');
    cancel.props.onClick();
    assert.equal(signal.aborted, true, 'cancel aborts the HTTP request signal');
    await new Promise((r) => setTimeout(r, 0));

    tree = page.render();
    assert.ok(findElement(tree, (n) => text(n) === 'cancelledTitle'), 'cancelled state is shown');
    assert.equal(text(tree).includes('本次报告'), false, 'no success/report is shown after cancel');
    assert.equal(findElement(tree, (n) => text(n).includes('导出 JSON')), undefined, 'no export button after cancel');
    assert.equal(page.calls.length, 1, 'cancel never triggers another run');
    assert.equal(page.toast.errors.length, 0, 'cancel is not treated as a failure');
    cleanup();
});

// 组件卸载：abort 仍在飞行的 controller。
test('unmount aborts an in-flight run', () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    const tree = page.render();
    const cleanup = page.mount();
    enqueueAndStart(tree, page);
    const signal = page.calls[0].signal;
    assert.equal(signal.aborted, false);
    cleanup();
    assert.equal(signal.aborted, true, 'unmount aborts the current controller');
});

const minimalReport = () => ({
    channel_id: 7,
    channel_name: 'c',
    upstream_model: 'grok-4.7',
    endpoint: '/v1/messages',
    duration_ms: 12,
    report: {
        requested_model: 'grok-4.7',
        score: 0,
        verdict: 'none',
        findings: [],
        results: [{ probe_id: 'identity', ok: true, data: { insufficient_evidence: true, reason: 'no_calibrated_profile' } }],
        errors: [],
        disclaimer: 'x',
    },
});

// 证据不足要显示 reason，且 headline 不得谎称「未见需要复核项」。
test('insufficient-evidence rows show their reason and headline no longer claims a clean result', async () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    page.setMutate(() => Promise.resolve(minimalReport()));
    let tree = page.render();
    const cleanup = page.mount();
    enqueueAndStart(tree, page);
    await new Promise((r) => setTimeout(r, 0));
    tree = page.render();
    const body = text(tree);
    assert.ok(body.includes('statusInsufficient'), 'row shows insufficient status');
    assert.ok(body.includes('no_calibrated_profile'), 'reason shown (not_applicable_reason/reason alias)');
    assert.ok(body.includes('headlineInsufficientTitle'), 'headline reports insufficient evidence');
    assert.equal(body.includes('未见需要复核的项'), false, 'headline must not claim a clean result');
    cleanup();
});

// 下一轮开始即清空旧报告；取消后没有可导出的旧报告。
test('starting a new run clears the previous report so cancel cannot export it', async () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    let resolveRun;
    page.setMutate(() => new Promise((res) => { resolveRun = res; }));
    let tree = page.render();
    const cleanup = page.mount();
    enqueueAndStart(tree, page);
    resolveRun(minimalReport());
    await new Promise((r) => setTimeout(r, 0));
    tree = page.render();
    assert.ok(findElement(tree, (n) => text(n).includes('导出 JSON')), 'export visible after a successful run');

    // 第二轮：开始即清空旧报告，然后取消。
    page.setMutate((vars) => new Promise((_, reject) => { vars.signal.addEventListener('abort', () => reject(new Error('aborted'))); }));
    enqueueAndStart(tree, page);
    tree = page.render();
    assert.equal(text(tree).includes('本次报告'), false, 'old report is cleared as soon as a new run starts');
    findElement(tree, (n) => n.type === 'Button' && text(n).includes('cancelCheck')).props.onClick();
    await new Promise((r) => setTimeout(r, 0));
    tree = page.render();
    assert.equal(findElement(tree, (n) => text(n).includes('导出 JSON')), undefined, 'no export after cancel');
    cleanup();
});
