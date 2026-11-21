// Run from web/: node --test tests/circuit-probe.test.cjs
//
// P3 主动探测（circuit probe）的前端验收。同一个 harness（tests/load-typescript.cjs +
// 手写 hooks）驱动真实组件，不跑浏览器、不打上游。
//
// 覆盖：
//   A 设置页「主动探测」整块可折叠、摘要带开关/今日已用/上限，5 个键写到正确的 key
//   B 渠道详情：熔断明细默认折叠、折叠后仍能看到条数、移动端三列等宽
//   C 探测状态徽标只说后端给的秒数（wait_upstream 用 remaining_seconds）
//   D 手动探测发 {id,key_id,model}，被拒绝时用后端的 remaining_seconds 说话
//   E 最近探测默认折叠、与熔断条目多少无关；摘要给最新一条的时间 + 结果；最多 20 条
//   F 渠道 probe_enabled 开关**显式**发 false
//   G Relay Logs 的探测标记（circuit_probe）认得出、文案走 i18n
//   H 三种语言都带全部新词条
const assert = require('node:assert/strict');
const { test } = require('node:test');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { load } = require('./load-typescript.cjs');

const jsxRuntime = {
    jsx: (type, props) => ({ type, props }),
    jsxs: (type, props) => ({ type, props }),
    Fragment: 'Fragment',
};

// 组件用 queueMicrotask 把 settings/初始值灌进 state。测试里让它立即执行，
// 这样一次 render 就能看到真实初始值（不用在每个用例里 await 微任务）。
// node --test 每个文件独立进程，这个补丁只影响本文件。
global.queueMicrotask = (callback) => callback();

/** SafeText 的真实工作是“把 value 当文本显示”，stub 也要保留这个语义，否则断言读不到内容。 */
const MonoSafeText = (props) => ({ type: 'MonoSafeText', props: { ...props, children: props.value ?? props.children } });

// t 把插值也吐出来，测试才能看见"后端给的秒数"是不是真的走到了文案里。
const tWithValues = (ns) => (key, values) => (values && Object.keys(values).length
    ? `${key}(${JSON.stringify(values)})`
    : key);

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));
const lucide = new Proxy({}, { get: (_, key) => key });

function walk(node, visit) {
    if (Array.isArray(node)) return node.forEach((child) => walk(child, visit));
    if (!node || typeof node !== 'object') return;
    visit(node);
    walk(node.props?.children, visit);
}

function collect(tree, predicate) {
    const found = [];
    walk(tree, (node) => { if (predicate(node)) found.push(node); });
    return found;
}

function text(node) {
    if (node === null || node === undefined || typeof node === 'boolean') return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join('');
    return text(node.props?.children);
}

function byAriaLabel(tree, label) {
    return collect(tree, (node) => node.props?.['aria-label'] === label)[0];
}

function byData(tree, attr) {
    return collect(tree, (node) => node.props && attr in node.props)[0];
}

/** 手写 hooks：状态跨 render 保留，effect 只在依赖变化时跑（和 React 一致）。 */
function reactHarness() {
    const slots = [];
    const queuedEffects = [];
    let cursor = 0;
    const react = {
        useState(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[i];
            return [slot.value, (next) => { slot.value = typeof next === 'function' ? next(slot.value) : next; }];
        },
        useRef(initial) {
            const i = cursor++;
            if (i >= slots.length) slots[i] = { current: initial };
            return slots[i];
        },
        useMemo(fn, deps) {
            const i = cursor++;
            const d = deps ?? [];
            if (!slots[i] || d.some((value, j) => !Object.is(value, slots[i].deps[j]))) slots[i] = { deps: d, value: fn() };
            return slots[i].value;
        },
        useCallback(fn) { return fn; },
        useEffect(fn, deps) {
            const i = cursor++;
            const d = deps ?? [];
            if (!slots[i] || d.some((value, j) => !Object.is(value, slots[i].deps[j]))) {
                slots[i] = { deps: d };
                queuedEffects.push(fn);
            }
        },
    };
    return {
        react,
        render(Component, props) {
            cursor = 0;
            queuedEffects.length = 0;
            let tree = Component(props);
            for (const effect of queuedEffects.splice(0)) effect();
            cursor = 0;
            queuedEffects.length = 0;
            tree = Component(props);
            for (const effect of queuedEffects.splice(0)) effect();
            return tree;
        },
    };
}

function makeMutations() {
    const recorder = () => ({
        writes: [],
        isPending: false,
        variables: undefined,
        options: null,
        mutate(variables, callbacks) {
            this.writes.push({ variables, callbacks });
            this.isPending = true;
            this.variables = variables;
        },
        settle(ok, error = new Error('rejected')) {
            const last = this.writes[this.writes.length - 1];
            assert.ok(last, 'settle() needs a pending write');
            this.isPending = false;
            if (ok) last.callbacks?.onSuccess?.(this.variables);
            else last.callbacks?.onError?.(error);
        },
    });
    return recorder;
}

// ---------------------------------------------------------------------------
// A. 设置页「主动探测」
// ---------------------------------------------------------------------------

const settingMocks = (probeStats, state) => {
    const settingModule = load('src/api/endpoints/setting.ts', {
        '@tanstack/react-query': {
            useQuery: () => ({ data: state.settings, isSuccess: true, isFetching: false, isError: false }),
            useQueryClient: () => ({ invalidateQueries() {}, setQueryData() {} }),
            useMutation: (options) => {
                state.mutation.options = options;
                return {
                    mutate: (variables, callbacks) => state.mutation.mutate(variables, callbacks),
                    get isPending() { return state.mutation.isPending; },
                    get variables() { return state.mutation.variables; },
                };
            },
        },
        '../client': { apiClient: {}, API_BASE_URL: '.' },
        '@/lib/logger': { logger: { log() {}, error() {} } },
        '@/components/common/Toast': { toast: { success() {}, error() {} } },
        './user': { useAuthStore: {} },
    });
    return {
        'next-intl': { useTranslations: (ns) => tWithValues(ns) },
        'react/jsx-runtime': jsxRuntime,
        '@/api/endpoints/setting': settingModule,
        '@/api/endpoints/channel': { useProbeStats: () => ({ data: probeStats }) },
        '@/components/ui/input': widgets('Input'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/ui/button': widgets('Button'),
        '@/components/animate-ui/components/animate/tooltip': widgets('Tooltip TooltipContent TooltipProvider TooltipTrigger'),
        '@/components/common/Toast': { toast: { success: (m) => state.notes.push(['success', m]), error: (m) => state.notes.push(['error', m]) } },
        'lucide-react': lucide,
    };
};

function makeSettingHarness({ settings = [], probeStats } = {}) {
    const state = { settings, notes: [], mutation: makeMutations()() };
    const hooks = reactHarness();
    const { SettingCircuitBreaker } = load('src/components/modules/setting/CircuitBreaker.tsx', {
        react: hooks.react,
        ...settingMocks(probeStats, state),
    });
    return {
        state,
        get mutation() { return state.mutation; },
        get notes() { return state.notes; },
        setSettings(next) { state.settings = next; },
        render() { return hooks.render(SettingCircuitBreaker, {}); },
        header(tree) { return byAriaLabel(tree, 'circuitBreaker.probe.title'); },
        switchByLabel(tree, label) { return byAriaLabel(tree, label); },
        inputByLabel(tree, label) {
            return collect(tree, (node) => node.type === 'Input' && node.props?.['aria-label'] === label)[0];
        },
    };
}

test('A: 主动探测整块默认折叠，标题行摘要给「开关状态 + 今日已用/上限」', async () => {
    const h = makeSettingHarness({
        settings: [
            { key: 'circuit_probe_enabled', value: 'true' },
            { key: 'circuit_probe_daily_cap', value: '200' },
        ],
        probeStats: {
            day: '2026-01-01',
            totals: { used: 37, cap: 200, recovered: 29, failed: 6, skipped: 2 },
            channels: [],
        },
    });
    let tree = h.render();
    const header = h.header(tree);
    assert.ok(header, '主动探测 的折叠标题必须存在');
    assert.equal(header.props['aria-expanded'], false, '默认折叠');
    const summary = text(byData(tree, 'data-probe-summary'));
    assert.match(summary, /circuitBreaker\.probe\.summaryOn/, '摘要要说开关是开的');
    assert.match(summary, /37 \/ 200/, '摘要要带今日已用/上限');
    // 折叠时不渲染明细输入
    assert.equal(h.inputByLabel(tree, 'circuitBreaker.probe.dailyCap.label'), undefined);

    header.props.onClick();
    tree = h.render();
    assert.equal(h.header(tree).props['aria-expanded'], true, '点一下展开');
    assert.ok(h.inputByLabel(tree, 'circuitBreaker.probe.dailyCap.label'), '展开后出现全站上限');
    assert.ok(h.inputByLabel(tree, 'circuitBreaker.probe.channelCap.label'), '展开后出现单渠道上限');
    assert.ok(h.inputByLabel(tree, 'circuitBreaker.probe.maxFailures.label'), '展开后出现连续失败停手');
});

test('A: 取不到探测统计时显示破折号，不拿 0 冒充「今天没探过」', async () => {
    const h = makeSettingHarness({ settings: [{ key: 'circuit_probe_daily_cap', value: '200' }] });
    const tree = h.render();
    const summary = text(byData(tree, 'data-probe-summary'));
    assert.match(summary, /—/, '没有统计就是 —');
    assert.match(summary, /circuitBreaker\.probe\.summaryOff/, '缺省按关闭显示');
    const usage = text(byData(tree, 'data-probe-usage') ?? '');
    assert.ok(usage === '' || usage.includes('—'), '展开后的今日已用同样是 —');
});

test('A: 总开关写 circuit_probe_enabled 的 true/false，失败回滚并报错', async () => {
    const h = makeSettingHarness({ settings: [{ key: 'circuit_probe_enabled', value: 'false' }] });
    let tree = h.render();
    h.header(tree).props.onClick();
    tree = h.render();
    const sw = () => h.switchByLabel(tree, 'circuitBreaker.probe.master.label');
    assert.equal(sw().props.checked, false);

    sw().props.onCheckedChange(true);
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_enabled', value: 'true' });
    h.mutation.settle(false);
    assert.ok(h.notes.some(([kind]) => kind === 'error'), '失败必须可见');
    tree = h.render();
    assert.equal(h.switchByLabel(tree, 'circuitBreaker.probe.master.label').props.checked, false, '失败回滚');

    h.switchByLabel(tree, 'circuitBreaker.probe.master.label').props.onCheckedChange(true);
    h.mutation.settle(true);
    assert.ok(h.notes.some(([kind]) => kind === 'success'));
    tree = h.render();
    assert.equal(h.switchByLabel(tree, 'circuitBreaker.probe.master.label').props.checked, true);

    h.switchByLabel(tree, 'circuitBreaker.probe.master.label').props.onCheckedChange(false);
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_enabled', value: 'false' },
        '关掉也要真的写到线上');
});

test('A: 三个上限/停手次数各写自己的 key，非数字回落到已确认值', async () => {
    const h = makeSettingHarness({
        settings: [
            { key: 'circuit_probe_daily_cap', value: '200' },
            { key: 'circuit_probe_channel_daily_cap', value: '20' },
            { key: 'circuit_probe_max_consecutive_failures', value: '3' },
        ],
    });
    let tree = h.render();
    h.header(tree).props.onClick();
    tree = h.render();

    const submit = (label, value) => {
        h.inputByLabel(tree, label).props.onChange({ target: { value } });
        tree = h.render();
        h.inputByLabel(tree, label).props.onBlur();
        tree = h.render();
    };

    submit('circuitBreaker.probe.dailyCap.label', '500');
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_daily_cap', value: '500' });
    h.mutation.settle(true);
    tree = h.render();

    submit('circuitBreaker.probe.channelCap.label', '50');
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_channel_daily_cap', value: '50' });
    h.mutation.settle(true);
    tree = h.render();

    submit('circuitBreaker.probe.maxFailures.label', '5');
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_max_consecutive_failures', value: '5' });
    h.mutation.settle(true);
    tree = h.render();

    // 乱填不等于 0：回落到已确认值，不写线上
    const before = h.mutation.writes.length;
    submit('circuitBreaker.probe.channelCap.label', 'abc');
    assert.equal(h.mutation.writes.length, before, '非数字不发请求');
    assert.equal(String(h.inputByLabel(tree, 'circuitBreaker.probe.channelCap.label').props.value), '50');

    // 负数归 0（后端按次数读，负次没有意义）
    submit('circuitBreaker.probe.maxFailures.label', '-3');
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_max_consecutive_failures', value: '0' });
});

test('A: 向量探测开关写 circuit_probe_embeddings_enabled', async () => {
    const h = makeSettingHarness({ settings: [{ key: 'circuit_probe_embeddings_enabled', value: 'false' }] });
    let tree = h.render();
    h.header(tree).props.onClick();
    tree = h.render();
    h.switchByLabel(tree, 'circuitBreaker.probe.embeddings.label').props.onCheckedChange(true);
    assert.deepEqual(h.mutation.writes.at(-1).variables, { key: 'circuit_probe_embeddings_enabled', value: 'true' });
});

// ---------------------------------------------------------------------------
// B/C/D. 渠道详情「熔断明细」
// ---------------------------------------------------------------------------

function makeCircuitHarness({ entries = [] } = {}) {
    const notes = [];
    const reset = makeMutations()();
    const probe = makeMutations()();
    const hooks = reactHarness();
    const { CircuitList } = load('src/components/modules/channel/CircuitList.tsx', {
        react: hooks.react,
        'react/jsx-runtime': jsxRuntime,
        'next-intl': { useTranslations: (ns) => tWithValues(ns) },
        '@/api/endpoints/channel': {
            useChannelCircuits: () => ({ data: entries }),
            useResetChannelCircuit: () => reset,
            useRunChannelProbe: () => probe,
        },
        '@/components/common/Toast': { toast: { success: (m) => notes.push(['success', m]), error: (m) => notes.push(['error', m]) } },
        '@/components/common/SafeText': { MonoSafeText, SafeText: (props) => ({ type: 'SafeText', props: { ...props, children: props.value ?? props.children } }) },
        '@/lib/utils': { cn: (...values) => values.filter(Boolean).join(' ') },
        'lucide-react': lucide,
    });
    const render = () => hooks.render(CircuitList, { channelId: 7, keyLabel: (id) => `key ${id}` });
    return {
        notes, reset, probe, render,
        renderExpanded() {
            const tree = render();
            collect(tree, (node) => node.type === 'button' && node.props?.['aria-expanded'] === false)[0].props.onClick();
            return render();
        },
    };
}

const openEntry = {
    key_id: 1,
    model: 'gpt-5.2',
    state: 'open',
    remaining_seconds: 18,
    consecutive_failures: 3,
    trip_count: 1,
    reason: '5xx ×3',
};

test('B: 没有熔断条目时整块不渲染', async () => {
    const h = makeCircuitHarness({ entries: [] });
    const tree = h.render();
    assert.equal(tree, null, '没有条目就不该占位');
});

test('B: 有条目时默认折叠，展开和折叠均给出条数', async () => {
    const h = makeCircuitHarness({ entries: [openEntry, { ...openEntry, model: 'deepseek-v4', state: 'half_open' }] });
    let tree = h.render();
    const header = collect(tree, (node) => node.type === 'button' && node.props?.['aria-expanded'] !== undefined)[0];
    assert.ok(header, '标题行是一个可折叠按钮');
    assert.equal(header.props['aria-expanded'], false, '有条目也默认折叠');
    assert.equal(header.props['aria-label'], 'title({"count":2})', '折叠/展开都显示条数标题');
    assert.equal(collect(tree, (node) => node.type === 'li').length, 0);
    header.props.onClick();
    tree = h.render();
    const expanded = collect(tree, (node) => node.type === 'button' && node.props?.['aria-expanded'] !== undefined)[0];
    assert.equal(expanded.props['aria-expanded'], true);
    assert.equal(expanded.props['aria-label'], header.props['aria-label']);
    assert.ok(collect(tree, (node) => node.type === 'li').length === 2, '两条熔断项都渲染');

    expanded.props.onClick();
    tree = h.render();
    const collapsed = collect(tree, (node) => node.type === 'button' && node.props?.['aria-expanded'] !== undefined)[0];
    assert.equal(collapsed.props['aria-expanded'], false);
    assert.equal(collect(tree, (node) => node.type === 'li').length, 0, '折叠后条目不再渲染');
});

test('B: 每一行的操作按钮正好三个，且移动端是三列等宽', async () => {
    const h = makeCircuitHarness({ entries: [openEntry] });
    const tree = h.renderExpanded();
    const row = collect(tree, (node) => node.type === 'li')[0];
    const actionRow = collect(row, (node) => typeof node.props?.className === 'string'
        && node.props.className.includes('grid-cols-3'))[0];
    assert.ok(actionRow, '操作按钮行必须用三列网格（窄屏整行等宽）');
    assert.match(actionRow.props.className, /\bw-full\b/, '整行');
    const actionButtons = collect(actionRow, (node) => node.type === 'button');
    assert.equal(actionButtons.length, 3, '正好三个按钮');
    for (const button of actionButtons) {
        assert.match(button.props.className, /justify-center/, '每列居中，等宽');
        assert.match(button.props.className, /max-sm:min-h-11/, '移动端触控高度 >= 44');
    }
    const labels = actionButtons.map((button) => text(button));
    assert.deepEqual(labels, ['probe.run', 'resetOne', 'resetModel']);
});

test('C: wait_upstream 用 remaining_seconds 说话，scheduled 用 next_in_seconds', async () => {
    const h = makeCircuitHarness({
        entries: [
            {
                ...openEntry,
                model: 'claude-sonnet-4.6',
                state: 'held',
                remaining_seconds: 1500,
                probe: { state: 'wait_upstream', next_in_seconds: 5, remaining_seconds: 1500 },
            },
            {
                ...openEntry,
                model: 'gpt-5.2',
                probe: { state: 'scheduled', next_in_seconds: 9 },
            },
            {
                ...openEntry,
                model: 'gemini-3.1-flash-image',
                probe: { state: 'not_probed_media', next_in_seconds: 0 },
            },
        ],
    });
    const tree = h.renderExpanded();
    const pills = collect(tree, (node) => node.props && node.props['data-probe-state'] !== undefined);
    assert.deepEqual(pills.map((node) => node.props['data-probe-state']), ['waitUpstream', 'scheduled', 'notProbed']);
    assert.match(text(pills[0]), /probe\.state\.waitUpstreamMinutes\(\{"minutes":25\}\)/, '25 分用的是 remaining_seconds 而不是 5 秒');
    assert.match(text(pills[1]), /probe\.state\.scheduled\(\{"seconds":9\}\)/);
    assert.match(text(pills[2]), /probe\.state\.notProbed/);
});

test('C: 认不出的探测状态原样露出，不吞成「未知」', async () => {
    const h = makeCircuitHarness({
        entries: [{ ...openEntry, probe: { state: 'some_future_state', next_in_seconds: 0 } }],
    });
    const tree = h.renderExpanded();
    const pill = collect(tree, (node) => node.props && node.props['data-probe-state'] !== undefined)[0];
    assert.equal(pill.props['data-probe-state'], 'unknown');
    assert.match(text(pill), /some future state/, '原文要能看见');
});

test('D: 立即探测发指定条目的作用域，拒绝时用后端的 remaining_seconds 说话', async () => {
    const h = makeCircuitHarness({ entries: [{ ...openEntry, endpoint: 'responses', capability: 'stream' }] });
    let tree = h.renderExpanded();
    const row = collect(tree, (node) => node.type === 'li')[0];
    const probeButton = collect(row, (node) => node.type === 'button' && text(node) === 'probe.run')[0];
    assert.ok(probeButton);
    assert.equal(probeButton.props.disabled, false);
    probeButton.props.onClick();
    assert.deepEqual(h.probe.writes.at(-1).variables, {
        id: 7, key_id: 1, model: 'gpt-5.2', endpoint: 'responses', capability: 'stream',
    });

    h.probe.settle(false, Object.assign(new Error('上游要求等待'), {
        probe: { state: 'wait_upstream', next_in_seconds: 0, remaining_seconds: 90 },
    }));
    const [, message] = h.notes.at(-1);
    assert.equal(h.notes.at(-1)[0], 'error');
    assert.match(message, /probe\.state\.waitUpstream\(\{"seconds":90\}\)/, '拒绝理由要说清还要等多久');

    tree = h.render();
    const again = collect(tree, (node) => node.type === 'button' && text(node) === 'probe.run')[0];
    again.props.onClick();
    h.probe.settle(true, undefined);
    assert.equal(h.notes.at(-1)[0], 'success');
});

test('D: 通配模型（*）不提供具体模型名，手探按钮禁用并说明原因', async () => {
    const h = makeCircuitHarness({ entries: [{ ...openEntry, model: '*' }] });
    const tree = h.renderExpanded();
    const row = collect(tree, (node) => node.type === 'li')[0];
    const buttons = collect(row, (node) => node.type === 'button');
    const probeButton = buttons.find((node) => text(node) === 'probe.run');
    assert.equal(probeButton.props.disabled, true, '没有模型名就不该发探测');
    assert.equal(probeButton.props.title, 'probe.noModel');
});

// 源码级锁（不冒充行为验证）：后端把「现在不能探」当正常回答（200 + state 结构体），
// 所以只有 state === 'scheduled' 才算接受；只看 res.ok 会把被拒的探测当成功报给用户。
// 这一条在 tests 里没法用组件 harness 验（真相在 fetch 层），所以按 source lock 写明。
test('D: 手动探测只把 state=scheduled 当成功，其余 state 走拒绝分支', () => {
    const src = readFileSync(path.join(__dirname, '..', 'src', 'api', 'endpoints', 'channel.ts'), 'utf8');
    assert.match(src, /export const PROBE_STATE_SCHEDULED = 'scheduled';/);
    assert.match(src, /const accepted = res\.ok && probe\?\.state === PROBE_STATE_SCHEDULED;/);
    assert.match(src, /if \(!accepted\) \{[\s\S]*?error\.probe = probe;/);
    assert.match(src, /remaining_seconds/);
});

// ---------------------------------------------------------------------------
// E. 最近探测
// ---------------------------------------------------------------------------

function makeHistoryHarness({ history = [] } = {}) {
    const hooks = reactHarness();
    const renderElement = (type, props) =>
        typeof type === 'function' ? type(props) : { type, props };
    const { ProbeHistoryList } = load('src/components/modules/channel/ProbeHistoryList.tsx', {
        react: hooks.react,
        'react/jsx-runtime': { ...jsxRuntime, jsx: renderElement, jsxs: renderElement },
        'next-intl': { useTranslations: (ns) => tWithValues(ns) },
        '@/api/endpoints/channel': { useChannelProbes: () => ({ data: { channel_id: 7, day: '2026-01-01', totals: {}, global_totals: {}, history } }) },
        '@/components/common/SafeText': { MonoSafeText, SafeText: (props) => ({ type: 'SafeText', props }) },
        '@/lib/utils': { cn: (...values) => values.filter(Boolean).join(' ') },
        'lucide-react': lucide,
    });
    return { render: () => hooks.render(ProbeHistoryList, { channelId: 7 }) };
}

const historyEntry = (overrides = {}) => ({
    timestamp: 1770000000, // 本地时区无关：只断言"时间串存在"，不断言具体钟点
    model: 'gpt-5.2',
    key_id: 2,
    prompt: '4821 + 3307',
    answer: '8128',
    duration_ms: 1200,
    result: 'ok',
    ...overrides,
});

test('E: 最近探测默认折叠，且与熔断条目多少无关', async () => {
    // 20 条历史下面也不许自己展开：展开只看用户点没点。
    const h = makeHistoryHarness({ history: Array.from({ length: 20 }, () => historyEntry()) });
    let tree = h.render();
    const header = byAriaLabel(tree, 'probe.history.title');
    assert.ok(header);
    assert.equal(header.props['aria-expanded'], false, '默认折叠');
    assert.equal(collect(tree, (node) => node.type === 'li').length, 0);

    header.props.onClick();
    tree = h.render();
    assert.equal(byAriaLabel(tree, 'probe.history.title').props['aria-expanded'], true);
    assert.equal(collect(tree, (node) => node.type === 'li').length, 20, '展开后逐条给出');
});

test('E: 折叠摘要给最新一条的时间 + 结果', async () => {
    const h = makeHistoryHarness({ history: [historyEntry({ result: 'timeout' }), historyEntry({ result: 'ok' })] });
    const tree = h.render();
    const summary = text(byData(tree, 'data-probe-history-summary'));
    assert.match(summary, /·/, '时间与结果之间要有分隔');
    assert.match(summary, /probe\.result\.failed/, '最新一条是 timeout → 仍失败');
    assert.ok(!/probe\.result\.recovered/.test(summary), '不能拿更早那条成功记录冒充最新结论');
});

test('E: 没有历史时摘要明说没探过', async () => {
    const h = makeHistoryHarness({ history: [] });
    const tree = h.render();
    assert.equal(text(byData(tree, 'data-probe-history-summary')), 'probe.history.empty');
});

test('E: 展开后给出题目/回答/耗时/结果，最多 20 条', async () => {
    const h = makeHistoryHarness({ history: Array.from({ length: 25 }, (_, i) => historyEntry({ model: `m${i}` })) });
    let tree = h.render();
    byAriaLabel(tree, 'probe.history.title').props.onClick();
    tree = h.render();
    const rows = collect(tree, (node) => node.type === 'li');
    assert.equal(rows.length, 20, '再多也只显示最近 20 条');
    const first = text(rows[0]);
    assert.match(first, /4821 \+ 3307/);
    assert.match(first, /8128/);
    assert.match(first, /1\.2s/);
    assert.match(first, /probe\.result\.recovered/);
});

// ---------------------------------------------------------------------------
// F. 渠道 probe_enabled 开关
// ---------------------------------------------------------------------------

function makeToggleHarness({ probeEnabled = true, totals, globalTotals } = {}) {
    const notes = [];
    const update = makeMutations()();
    const hooks = reactHarness();
    const { ChannelProbeToggle } = load('src/components/modules/channel/ChannelProbeToggle.tsx', {
        react: hooks.react,
        'react/jsx-runtime': jsxRuntime,
        'next-intl': { useTranslations: (ns) => tWithValues(ns) },
        '@/api/endpoints/channel': {
            useUpdateChannel: () => update,
            useChannelProbes: () => ({
                data: totals && globalTotals ? { totals, global_totals: globalTotals, history: [] } : undefined,
            }),
        },
        '@/components/ui/switch': widgets('Switch'),
        '@/components/common/Toast': { toast: { success: (m) => notes.push(['success', m]), error: (m) => notes.push(['error', m]) } },
        'lucide-react': lucide,
    });
    const channel = { id: 7, probe_enabled: probeEnabled };
    return {
        update, notes,
        render: () => hooks.render(ChannelProbeToggle, { channel }),
    };
}

test('F: 关掉探测时显式发 probe_enabled:false（不能靠“缺省不改”）', async () => {
    const h = makeToggleHarness({ probeEnabled: true });
    let tree = h.render();
    const sw = byAriaLabel(tree, 'probe.label');
    assert.equal(sw.props.checked, true);
    sw.props.onCheckedChange(false);
    const write = h.update.writes.at(-1).variables;
    assert.equal(write.id, 7);
    assert.equal(write.probe_enabled, false, 'false 必须在 payload 里');
    assert.ok(Object.hasOwn(write, 'probe_enabled'));

    tree = h.render();
    byAriaLabel(tree, 'probe.label').props.onCheckedChange(true);
    assert.equal(h.update.writes.at(-1).variables.probe_enabled, true);
});

test('F: 今日用量同时给本渠道和全站两档；取不到显示破折号', async () => {
    const h = makeToggleHarness({
        totals: { used: 6, cap: 20 },
        globalTotals: { used: 37, cap: 200 },
    });
    const tree = h.render();
    const row = byData(tree, 'data-channel-probe');
    const body = text(row);
    assert.match(body, /6 \/ 20/, '本渠道 6 / 20');
    assert.match(body, /37 \/ 200/, '全站 37 / 200');

    const empty = makeToggleHarness({ totals: { used: 0, cap: 0 }, globalTotals: { used: 0, cap: 0 } });
    const emptyTree = empty.render();
    assert.doesNotMatch(text(byData(emptyTree, 'data-channel-probe')), /0 \/ 0/, 'undefined cap 不编 0 / 0');
});

// ---------------------------------------------------------------------------
// G. Relay Logs 的探测标记
// ---------------------------------------------------------------------------

const humanize = load('src/components/modules/log/humanize.ts');

test('G: 认得出 circuit_probe 探测行（request_endpoint / error_strategy 首段）', () => {
    assert.equal(humanize.CIRCUIT_PROBE_MARKER, 'circuit_probe');
    assert.equal(humanize.isCircuitProbeLog({ request_endpoint: 'circuit_probe' }), true);
    assert.equal(humanize.isCircuitProbeLog({
        request_endpoint: 'circuit_probe',
        error_strategy: 'circuit_probe;billable=false;stats_counted=false;breaker_counted=false',
    }), true, 'error_strategy 的固定三段与后端源码一致');
    assert.equal(humanize.isCircuitProbeLog({ error_strategy: 'circuit_probe' }), true);
    assert.equal(humanize.isCircuitProbeLog({ request_endpoint: 'chat', error_strategy: 'upstream_error' }), false);
    assert.equal(humanize.isCircuitProbeLog({}), false);
    assert.equal(humanize.isCircuitProbeLog(null), false);
    assert.equal(humanize.isCircuitProbeLog({ request_endpoint: 'circuit_probe_extra' }), false, '不做前缀猜测');
});

test('G: 探测标记的文案走 i18n（log.card.probeMarker），不是写死中文', () => {
    const src = readFileSync(path.join(__dirname, '..', 'src/components/modules/log/Item.tsx'), 'utf8');
    assert.match(src, /isCircuitProbeLog/);
    assert.match(src, /t\('probeMarker'\)/);
});

// ---------------------------------------------------------------------------
// H. 三语言词条
// ---------------------------------------------------------------------------

test('H: zh_hans / zh_hant / en 都带全部探测词条', () => {
    const stateKeys = ['scheduledNow', 'scheduled', 'scheduledMinutes', 'waitUpstreamNow', 'waitUpstream',
        'waitUpstreamMinutes', 'running', 'skippedTraffic', 'notProbed', 'off', 'stopped', 'unknown'];
    const resultKeys = ['recovered', 'failed', 'skipped', 'noVerdict', 'unknown', 'none'];
    const historyKeys = ['title', 'time', 'model', 'key', 'prompt', 'answer', 'duration', 'result', 'empty', 'note'];
    // 原因词条不再手抄一份：直接从 KNOWN_RESULT_REASONS 解析，少一个词条就红。
    const formatSource = readFileSync(
        path.join(__dirname, '..', 'src', 'components', 'modules', 'channel', 'probe-format.ts'),
        'utf8'
    );
    const reasonBlock = /const KNOWN_RESULT_REASONS = new Set\(\[([\s\S]*?)\]\);/.exec(formatSource)?.[1] ?? '';
    const reasonTokens = [...reasonBlock.matchAll(/'([a-z0-9_-]+)'/g)].map((match) => match[1]);
    assert.ok(reasonTokens.length >= 10, `KNOWN_RESULT_REASONS 解析失败（得到 ${reasonTokens.length} 个）`);
    for (const lang of ['zh_hans', 'zh_hant', 'en']) {
        const data = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', `${lang}.json`), 'utf8'));
        const probe = data.setting?.circuitBreaker?.probe;
        assert.ok(probe, `${lang}: setting.circuitBreaker.probe 缺失`);
        for (const key of ['title', 'summaryOn', 'summaryOff', 'usedToday', 'recovered', 'failed', 'skipped']) {
            assert.ok(probe[key], `${lang}: probe.${key}`);
        }
        for (const group of ['master', 'dailyCap', 'channelCap', 'maxFailures', 'embeddings']) {
            assert.ok(probe[group]?.label, `${lang}: probe.${group}.label`);
        }
        for (const key of ['whenTitle', 'when', 'whatTitle', 'what', 'skipTitle', 'skip']) {
            assert.ok(probe.note?.[key], `${lang}: probe.note.${key}`);
        }

        const circuits = data.channel?.circuits?.probe;
        assert.ok(circuits, `${lang}: channel.circuits.probe 缺失`);
        for (const key of ['run', 'runHint', 'accepted', 'failed', 'noModel', 'resetModelSameScope']) {
            assert.ok(circuits[key], `${lang}: circuits.probe.${key}`);
        }
        for (const key of stateKeys) assert.ok(circuits.state?.[key], `${lang}: circuits.probe.state.${key}`);
        for (const key of resultKeys) assert.ok(circuits.result?.[key], `${lang}: circuits.probe.result.${key}`);
        assert.ok(circuits.result?.withReason, `${lang}: circuits.probe.result.withReason`);
        for (const token of reasonTokens) {
            assert.ok(circuits.result?.reason?.[token], `${lang}: circuits.probe.result.reason.${token} 缺失`);
        }
        for (const key of historyKeys) assert.ok(circuits.history?.[key], `${lang}: circuits.probe.history.${key}`);

        assert.ok(data.channel?.detail?.probe?.label, `${lang}: channel.detail.probe.label`);
        assert.ok(data.channel?.detail?.probe?.today, `${lang}: channel.detail.probe.today`);
        assert.ok(data.channel?.detail?.probe?.global, `${lang}: channel.detail.probe.global`);
        assert.ok(data.channel?.detail?.probe?.saveFailed, `${lang}: channel.detail.probe.saveFailed`);
        assert.ok(data.log?.card?.probeMarker, `${lang}: log.card.probeMarker`);
    }
    const zh = JSON.parse(readFileSync(path.join(__dirname, '..', 'public', 'locale', 'zh_hans.json'), 'utf8'));
    assert.equal(zh.log.card.probeMarker, '探测', '日志里的标记就是「探测」两个字');
    assert.equal(zh.channel.circuits.probe.run, '立即探测');
});

// ---------------------------------------------------------------------------
// 纯逻辑：probe-format
// ---------------------------------------------------------------------------

const format = load('src/components/modules/channel/probe-format.ts');

test('probe-format: 后端真实 state 逐个对应到一种徽标', () => {
    assert.deepEqual(
        format.probeStateView({ state: 'wait_upstream', next_in_seconds: 5, remaining_seconds: 1500 }),
        { kind: 'waitUpstream', seconds: 1500, raw: 'wait_upstream' }
    );
    assert.deepEqual(
        format.probeStateView({ state: 'scheduled', next_in_seconds: 9 }),
        { kind: 'scheduled', seconds: 9, raw: 'scheduled' }
    );
    // 熔断条目里的 probe 只给 next_in_seconds（等待已经折进去），照样能说清楚。
    assert.equal(format.probeStateView({ state: 'wait_upstream', next_in_seconds: 42 }).seconds, 42);
    assert.equal(format.probeStateView({ state: 'running' }).kind, 'probing');
    assert.equal(format.probeStateView({ state: 'skipped_traffic' }).kind, 'skippedTraffic');
    assert.equal(format.probeStateView({ state: 'not_probed_media' }).kind, 'notProbed');
    assert.equal(format.probeStateView({ state: 'disabled' }).kind, 'off');
    assert.equal(format.probeStateView({ state: 'stopped' }).kind, 'stopped');
    assert.equal(format.probeStateView(undefined), null, '没有探测信息就不编一个状态');
    assert.equal(format.probeStateView({ state: '   ' }), null);
});

test('probe-format: 状态文案的秒/分档位由词条决定，代码不拼单位', () => {
    const t = (key, values) => (values ? `${key}(${JSON.stringify(values)})` : key);
    assert.equal(format.probeStateLabel({ kind: 'scheduled', seconds: 9, raw: 'scheduled' }, t), 'probe.state.scheduled({"seconds":9})');
    assert.equal(format.probeStateLabel({ kind: 'scheduled', seconds: 300, raw: 'scheduled' }, t), 'probe.state.scheduledMinutes({"minutes":5})');
    assert.equal(format.probeStateLabel({ kind: 'scheduled', seconds: 0, raw: 'scheduled' }, t), 'probe.state.scheduledNow');
    assert.equal(format.probeStateLabel({ kind: 'probing', seconds: 0, raw: 'running' }, t), 'probe.state.running');
    assert.equal(format.probeStateLabel({ kind: 'unknown', seconds: 0, raw: 'some_state' }, t), 'probe.state.unknown({"state":"some state"})');
});

test('probe-format: 时间戳接受 Unix 秒 / 毫秒 / 已排好的时间串', () => {
    assert.equal(format.formatProbeClock(0), '');
    assert.equal(format.formatProbeClock(undefined), '');
    assert.equal(format.formatProbeClock('14:58:12'), '14:58:12');
    const fromSeconds = format.formatProbeClock(1770000000);
    assert.match(fromSeconds, /^\d{2}:\d{2}:\d{2}$/, 'Unix 秒按本地时间排成 HH:MM:SS');
    assert.equal(format.formatProbeClock(1770000000000), fromSeconds, '毫秒与秒同一时刻给同一结果');
    // 后端历史行的 timestamp 是 RFC3339（circuit_probe_scheduler.go 的 recordHistory）。
    // 不断言具体钟点：服务器时区未知，只断言「排成了 HH:MM:SS」且与同一时刻的毫秒值一致。
    const rfc = format.formatProbeClock('2026-01-01T09:08:07+08:00');
    assert.match(rfc, /^\d{2}:\d{2}:\d{2}$/, 'RFC3339 排成 HH:MM:SS');
    assert.equal(rfc, format.formatProbeClock(Date.parse('2026-01-01T09:08:07+08:00')), '同一时刻两种写法结果一致');
    assert.equal(format.formatProbeClock('not-a-time'), 'not-a-time', '认不出的格式原样露出');
});

test('probe-format: 耗时 / 结论摘要 / 用量百分比', () => {
    assert.equal(format.formatProbeDuration(0), '');
    assert.equal(format.formatProbeDuration(1200), '1.2s');
    assert.equal(format.formatProbeDuration(300), '300ms');

    const t = (key, values) => (values ? `${key}(${JSON.stringify(values)})` : key);
    assert.equal(format.probeResultLabel('ok', t), 'probe.result.recovered');
    assert.equal(format.probeResultLabel('timeout', t), 'probe.result.failed');
    assert.equal(format.probeResultLabel('skipped', t), 'probe.result.skipped');
    assert.equal(format.probeResultLabel('', t), 'probe.result.none');
    assert.equal(format.probeResultLabel('brand_new', t), 'probe.result.unknown({"result":"brand new"})');
    // 后端真实取值：recovered / stale / failed[:outcome] / skipped[:reason]
    assert.equal(format.probeResultLabel('recovered', t), 'probe.result.recovered');
    assert.equal(format.probeResultLabel('mixed_scope', t), 'probe.result.recovered', '混范围也是探好了');
    assert.equal(
        format.probeResultLabel('stale', t),
        'probe.result.withReason({"status":"probe.result.noVerdict","reason":"probe.result.reason.stale"})',
        '条目已变：不算我们修好的，也不当成失败'
    );
    assert.equal(format.probeResultLabel('failed', t), 'probe.result.failed');

    assert.equal(format.probeUsagePercent(37, 200), 18.5);
    assert.equal(format.probeUsagePercent(0, 0), 0, 'cap 0 不画进度，不拿 0% 冒充');
    assert.equal(format.probeUsagePercent(500, 200), 100, '不超 100%');
});

// ---------------------------------------------------------------------------
// B 侧现在是「带原因的结论串」：failed:<outcome> / skipped:<reason>
// ---------------------------------------------------------------------------

test('probe-format: failed:<outcome> 定 failed，原因单独翻译不弄丢', () => {
    const t = (key, values) => (values ? `${key}(${JSON.stringify(values)})` : key);
    const label = (value) => format.probeResultLabel(value, t);
    assert.equal(
        label('failed:timeout'),
        'probe.result.withReason({"status":"probe.result.failed","reason":"probe.result.reason.timeout"})'
    );
    assert.equal(
        label('failed:wrong_answer'),
        'probe.result.withReason({"status":"probe.result.failed","reason":"probe.result.reason.wrong_answer"})'
    );
    // 认不出的 outcome 不吞：下划线拆成空格原样露出
    assert.equal(
        label('failed:brand_new_outcome'),
        'probe.result.withReason({"status":"probe.result.failed","reason":"brand new outcome"})'
    );
    assert.equal(format.probeResultKind('failed:timeout'), 'failed');
});

test('probe-format: skipped:<reason> 定 skipped，调度侧四个理由都能翻', () => {
    const t = (key, values) => (values ? `${key}(${JSON.stringify(values)})` : key);
    for (const reason of ['skipped_traffic', 'wait_upstream', 'capped', 'streak']) {
        const label = format.probeResultLabel(`skipped:${reason}`, t);
        assert.equal(format.probeResultKind(`skipped:${reason}`), 'skipped', `${reason} 是跳过不是失败`);
        assert.equal(label, t('probe.result.withReason', {
            status: 'probe.result.skipped',
            reason: 'probe.result.reason.' + reason,
        }), reason + ' 要翻成人话');
    }
    // A 侧 outcome 也能当跳过理由
    assert.equal(format.probeResultKind('skipped:canceled'), 'skipped');
    assert.match(
        format.probeResultLabel('skipped:canceled', t),
        /probe\.result\.reason\.canceled/
    );
});

test('probe-format: ambiguous 永不算 failed（A 侧强制非归因）', () => {
    const t = (key, values) => (values ? `${key}(${JSON.stringify(values)})` : key);
    // 就算某条行把 ambiguous 写在 failed: 后面，也不冤枉渠道
    assert.equal(format.probeResultKind('failed:ambiguous'), 'noVerdict');
    assert.equal(format.probeResultKind('ambiguous'), 'noVerdict');
    assert.equal(format.probeResultKind('skipped:ambiguous'), 'skipped');
    const label = format.probeResultLabel('failed:ambiguous', t);
    assert.doesNotMatch(label, /probe\.result\.failed/, '绝不能显示“仍失败”');
    assert.match(label, /probe\.result\.noVerdict/);
    assert.match(label, /probe\.result\.reason\.ambiguous/);

    // 折叠摘要也不能因 ambiguous 透出失败
    const summary = format.probeHistorySummary([{ timestamp: '2026-01-01T09:08:07+08:00', result: 'failed:ambiguous' }], t);
    assert.doesNotMatch(summary, /probe\.result\.failed/);
    assert.match(summary, /probe\.result\.reason\.ambiguous/);
});
