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
    //    2026-10-20 起：前端队列已并入后端真队列，不再有"加入队列"这一步。
    //    现在点一次「后台检测」= 一次 enqueue POST，所以这里只找那一个按钮。
    const startBtn = findElement(tree, (n) => n.type === 'Button' && text(n).includes('后台检测'));
    if (!startBtn) throw new Error('start button not found');
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
    const enqueued = [];
    const queueView = { onOpenReport: undefined };
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
            // 后台队列入口。这些测试关心的是"点一下会不会多打上游"，
            // 所以给一个可观测的替身：调用被记进 enqueued 数组，不真的发请求。
            useEnqueueAudit: () => ({
                isPending: false,
                mutateAsync: (vars) => { enqueued.push(vars); return Promise.resolve({ job_id: 1 }); },
            }),
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
        // 队列页自身的渲染在这里不测（有独立的 model-audit-queue.test.cjs）。
        // 但它现在是报告的唯一来源：用户在队列里点开某条结果，报告才回到本页，
        // 所以给一个能触发回调的存根，测试才能走完整的"回看"路径。
        './Queue': {
            ModelAuditQueue: ({ onOpenReport } = {}) => {
                queueView.onOpenReport = onOpenReport;
                return null;
            },
        },
    });
    return {
        calls,
        enqueued,
        queueView,
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
test('active audit page: mount and option changes never call upstream; one click = one enqueue', () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    let tree = page.render();
    const cleanup = page.mount();
    assert.equal(page.calls.length, 0, 'mount issues no audit call');
    assert.equal(page.enqueued.length, 0, 'mount issues no enqueue call');

    // 改变渠道/计划/探针勾选：均只改本地状态，不发请求、不入队。
    findElement(tree, (n) => n.type === 'SearchableSelect' && typeof n.props?.onValueChange === 'function').props.onValueChange('7');
    findElement(tree, (n) => n.type === 'button' && text(n).includes('快速')).props.onClick();
    findElement(tree, (n) => n.type === 'input' && n.props?.type === 'checkbox').props.onChange({ target: { checked: true } });
    assert.equal(page.calls.length, 0, 'option changes never call upstream');
    assert.equal(page.enqueued.length, 0, 'option changes never enqueue');

    // 点一次「后台检测」= 一次入队提交，而不是一次同步 /run。
    //
    // 这条守的是花钱边界：检测每个探针都是真实计费请求，点一次只能产生一次提交。
    // 以前守的是"一次 POST /run"，现在守"一次 POST /enqueue"，被保护的东西没变。
    tree = page.render();
    enqueueAndStart(tree, page);
    assert.equal(page.enqueued.length, 1, 'one click = one enqueue');
    assert.equal(page.calls.length, 0, 'the sync /run path is no longer used by the page');
    assert.equal(page.enqueued[0].channel_id, 7);
    assert.equal(page.enqueued[0].probes.includes('signature'), false, 'grok model never selects the signature probe');
    assert.ok(Array.isArray(page.enqueued[0].models) && page.enqueued[0].models.length === 1, 'submits exactly the selected model');

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
        // 探针选择现在随入队请求提交，从 enqueued 取；被守的规则本身没变。
        assert.equal((page.enqueued[0]?.probes ?? []).includes('signature'), c.expect, `${c.model} on type ${c.type}`);
    }
});
// 取消：现在取消的是后端任务，不是浏览器里那个 HTTP 连接。
//
// 为什么必须这样：检测的每一次探针都是真实计费请求。以前 abort 掉 fetch 只断开
// 当前这一个连接，后台队列该跑还是跑、该花钱还是花钱。要让"取消"名副其实，
// 只能打后端的 cancel 接口。所以这条测试断言的对象从 AbortSignal 换成了入队 + 取消目标。
test('cancel is delegated to the backend job, not to a browser abort', async () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    const tree = page.render();
    const cleanup = page.mount();

    enqueueAndStart(tree, page);
    assert.equal(page.enqueued.length, 1, 'submitting is one enqueue');
    assert.equal(page.calls.length, 0, 'no synchronous /run is issued any more');

    // 检测页本身不再持有"正在跑"的状态，因此也不再显示一个假的取消按钮；
    // 取消统一在队列页对具体任务操作（那条链路由 model-audit-queue.test.cjs 覆盖）。
    const localCancel = findElement(page.render(), (n) => n.type === 'Button' && text(n).includes('cancelCheck'));
    assert.equal(localCancel, undefined, 'the page no longer fakes a cancel it cannot honour');

    cleanup();
});

// 卸载：关掉页面不该中断后台检测。
//
// 这正好是旧行为反过来的一条。以前卸载会 abort 请求，看起来"干净"，实际是把
// 用户已经付过钱的检测半路掐死。现在活儿已经在后端队列里，关页面只是关掉观察窗口。
test('unmounting the page leaves the background job alone', () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    const tree = page.render();
    const cleanup = page.mount();

    enqueueAndStart(tree, page);
    assert.equal(page.enqueued.length, 1, 'the job was submitted');

    cleanup(); // 模拟离开页面
    assert.equal(page.calls.length, 0, 'no request was aborted because none was in flight');
    assert.equal(page.enqueued.length, 1, 'the submission is not withdrawn by leaving');
});


// 这个测试框架刻意不展开函数组件（只做元素树比对），所以队列存根不会自己执行。
// 想验证"从队列点开历史报告"这条路径，就得手动把它跑一次，拿它注册的回调。
function mountQueueStub(tree, page) {
    const node = findElement(tree, (n) => typeof n.type === 'function' && n.type.name === 'ModelAuditQueue');
    assert.ok(node, 'queue stub is present in the tree');
    node.type(node.props);
    assert.equal(typeof page.queueView.onOpenReport, 'function', 'queue page exposes the report callback');
}

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
//
// 报告现在从队列结果回看进来（openStoredReport 解析后端存的 JSON），
// 所以这条测试走真实入口：切到队列、模拟点开某条已完成结果。
test('insufficient-evidence rows show their reason and headline no longer claims a clean result', async () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    let tree = page.render();
    const cleanup = page.mount();

    // 切到「检测记录」，让队列页渲染出来并拿到 onOpenReport 回调。
    findElement(tree, (n) => n.type === 'button' && text(n).includes('检测记录')).props.onClick();
    tree = page.render();
    mountQueueStub(tree, page);

    const view = minimalReport().report;
    page.queueView.onOpenReport({
        id: 1, job_id: 1, log_id: 0, channel_id: 7, channel_name: 'c',
        model: 'grok-4.7', upstream_model: '', status: 'done',
        verdict: 'none', score: 0, finding_n: 0,
        report: JSON.stringify(view), error_msg: '', duration_ms: 12,
        created_at: new Date().toISOString(),
    });
    tree = page.render();
    // 点开报告后要切回详情视图才会渲染它（mode 决定渲染哪一侧）。
    findElement(tree, (n) => n.type === 'button' && text(n).includes('发起检测')).props.onClick();
    tree = page.render();

    const body = text(tree);
    assert.ok(body.includes('statusInsufficient'), 'row shows insufficient status');
    assert.ok(body.includes('no_calibrated_profile'), 'reason shown (not_applicable_reason/reason alias)');
    assert.ok(body.includes('headlineInsufficientTitle'), 'headline reports insufficient evidence');
    assert.equal(body.includes('未见需要复核的项'), false, 'headline must not claim a clean result');
    cleanup();
});

// 下一轮开始即清空旧报告；否则用户会把上一轮的报告当成这一轮的导出。
test('starting a new run clears the previous report so it cannot be exported as the new one', async () => {
    const page = auditPage();
    page.setChannels(grokChannel);
    let tree = page.render();
    const cleanup = page.mount();

    // 先从队列回看一份历史报告：此时详情面板是「有报告」状态。
    findElement(tree, (n) => n.type === 'button' && text(n).includes('检测记录')).props.onClick();
    tree = page.render();
    mountQueueStub(tree, page);
    page.queueView.onOpenReport({
        id: 1, job_id: 1, log_id: 0, channel_id: 7, channel_name: 'c',
        model: 'grok-4.7', upstream_model: '', status: 'done',
        verdict: 'none', score: 0, finding_n: 0,
        report: JSON.stringify(minimalReport().report), error_msg: '', duration_ms: 12,
        created_at: new Date().toISOString(),
    });
    tree = page.render();
    mountQueueStub(tree, page);
    findElement(tree, (n) => n.type === 'button' && text(n).includes('发起检测')).props.onClick();
    tree = page.render();
    assert.ok(findElement(tree, (n) => text(n).includes('导出 JSON')), 'export visible while a report is open');

    // 回到「发起检测」并提交新一轮：旧报告必须立刻消失。
    findElement(tree, (n) => n.type === 'button' && text(n).includes('发起检测')).props.onClick();
    tree = page.render();
    enqueueAndStart(tree, page);
    // 提交是异步的：清报告发生在 await 之后，必须让微任务跑完再看。
    await new Promise((r) => setTimeout(r, 0));
    tree = page.render();
    assert.equal(text(tree).includes('本次报告'), false, 'old report is cleared as soon as a new run starts');
    assert.equal(findElement(tree, (n) => text(n).includes('导出 JSON')), undefined, 'no stale export button after starting a new run');
});
