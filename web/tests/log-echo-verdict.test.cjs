// Run from web/: node --test tests/log-echo-verdict.test.cjs
//
// 覆盖日志卡片「回显判定」收敛（humanize.describeEchoComparison）与卡片头部的
// 共享 6 列 × 2 排 grid 结构（data-log-* 验收钩子）。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const React = require('react');
const { load } = require('./load-typescript.cjs');

const { describeEchoComparison, isRegionNamespaceHint } = load('src/components/modules/log/humanize.ts');
const display = (value) => value ?? '';
const base = {
    request_model_name: '',
    actual_model_name: '',
    upstream_response_model: '',
    upstream_model_mismatch: undefined,
    upstream_mismatch_code: '',
    model_family_verdict: '',
    model_vendor_hint: '',
};

test('cn: prefix strict equivalence is not flagged as an amber mismatch', () => {
    const sample = { ...base, request_model_name: 'cn:deepseek-v4.1-flash', actual_model_name: 'cn:deepseek-v4.1-flash', upstream_response_model: 'deepseek-v4.1-flash', upstream_model_mismatch: true, model_family_verdict: 'suspect' };
    const c = describeEchoComparison(sample, display);
    assert.equal(c.tone, 'equivalent');
    assert.equal(c.toLabel, 'deepseek-v4.1-flash');
    assert.notEqual(c.tone, 'name-mismatch');
});

test('legacy cn: equivalence (verdict empty) also shows the echo name plainly', () => {
    const c = describeEchoComparison({ ...base, actual_model_name: 'cn:deepseek-v4.1-flash', upstream_response_model: 'deepseek-v4.1-flash', upstream_model_mismatch: true }, display);
    assert.equal(c.tone, 'equivalent');
});

test('cross_family code = actual vs echo -> comparison is actual -> echo (request may equal echo)', () => {
    // request == echo, but actual was swapped: a request->echo comparison would be empty.
    const c = describeEchoComparison({ ...base, request_model_name: 'deepseek-v4.1-flash', actual_model_name: 'gpt-5.5', upstream_response_model: 'deepseek-v4.1-flash', upstream_mismatch_code: 'cross_family' }, display);
    assert.equal(c.tone, 'cross-family');
    assert.equal(c.fromRaw, 'gpt-5.5');
    assert.equal(c.toRaw, 'deepseek-v4.1-flash');
    assert.equal(c.showComparison, true);
});

test('legacy suspect && mismatch===false = request vs echo -> comparison is request -> echo', () => {
    const c = describeEchoComparison({ ...base, request_model_name: 'gpt-5.5', actual_model_name: 'deepseek-v4.1-flash', upstream_response_model: 'deepseek-v4.1-flash', upstream_model_mismatch: false, model_family_verdict: 'suspect' }, display);
    assert.equal(c.tone, 'cross-family');
    assert.equal(c.fromRaw, 'gpt-5.5');
    assert.equal(c.toRaw, 'deepseek-v4.1-flash');
});

test('other unknown difference is amber, from=actual to=echo', () => {
    const c = describeEchoComparison({ ...base, actual_model_name: 'claude-opus-4-8', upstream_response_model: 'gpt-5.5', upstream_model_mismatch: true, model_family_verdict: 'suspect' }, display);
    assert.equal(c.tone, 'name-mismatch');
    assert.equal(c.fromRaw, 'claude-opus-4-8');
    assert.equal(c.toRaw, 'gpt-5.5');
});

test('alias variant is detected separately from cross-family', () => {
    const c = describeEchoComparison({ ...base, actual_model_name: 'model-family-alpha', upstream_response_model: 'model-family-alpha-20251101', upstream_model_mismatch: true, upstream_mismatch_code: 'alias_variant', model_family_verdict: 'suspect' }, display);
    assert.equal(c.tone, 'alias-variant');
});

test('trusted: region/namespace prefix is not shown as a vendor', () => {
    const c = describeEchoComparison({ ...base, upstream_response_model: 'cn:deepseek-v4.1-flash', model_family_verdict: 'trusted', model_vendor_hint: 'cn' }, display);
    assert.equal(c.tone, 'trusted');
    assert.equal(c.vendorPrefix, '');
    assert.equal(c.modelName, 'deepseek-v4.1-flash');
});

test('trusted: a genuine vendor prefix is shown as a self-reported prefix', () => {
    const c = describeEchoComparison({ ...base, upstream_response_model: 'vercel-deepseek/deepseek-v4.1', model_family_verdict: 'trusted', model_vendor_hint: 'vercel-deepseek' }, display);
    assert.equal(c.tone, 'trusted');
    assert.equal(c.vendorPrefix, 'vercel-deepseek');
    assert.equal(c.modelName, 'deepseek-v4.1');
});

test('no upstream echo yields none', () => {
    assert.equal(describeEchoComparison({ ...base, actual_model_name: 'x' }, display).tone, 'none');
});

test('region/namespace hint detection', () => {
    assert.equal(isRegionNamespaceHint('cn'), true);
    assert.equal(isRegionNamespaceHint('global'), true);
    assert.equal(isRegionNamespaceHint('us-east'), true);
    assert.equal(isRegionNamespaceHint('vercel-deepseek'), false);
    assert.equal(isRegionNamespaceHint(''), false);
});

// --- card header DOM structure ---------------------------------------------

function hooks() {
    const slots = [];
    let cursor = 0;
    return {
        react: {
            ...React,
            useMemo(fn, deps) {
                const i = cursor++;
                const d = deps ?? [];
                if (!slots[i] || !deps || d.some((value, j) => !Object.is(value, slots[i].deps[j]))) slots[i] = { deps: d, value: fn() };
                return slots[i].value;
            },
            useState(initial) { return [typeof initial === 'function' ? initial() : initial, () => {}]; },
            useCallback(fn) { return fn; },
            useEffect() {},
        },
    };
}

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));

function findElement(node, predicate) {
    if (Array.isArray(node)) {
        for (const child of node) {
            const found = findElement(child, predicate);
            if (found) return found;
        }
        return undefined;
    }
    if (!node || typeof node !== 'object') return undefined;
    if (predicate(node)) return node;
    return findElement(node.props?.children, predicate);
}

function loadCard() {
    const cardHooks = hooks();
    return load('src/components/modules/log/Item.tsx', {
        react: cardHooks.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        'next-intl': { useTranslations: () => (key) => key },
        'motion/react': { motion: {}, AnimatePresence: 'AnimatePresence' },
        '@uiw/react-json-view': {},
        '@uiw/react-json-view/githubDark': {},
        '@uiw/react-json-view/githubLight': {},
        'next-themes': {},
        zustand: { create: (init) => { const state = init(() => {}); return (select) => select(state); } },
        '@/api/endpoints/log': { getRelayLogSeverity: () => 'success' },
        './humanize': load('src/components/modules/log/humanize.ts'),
        '@/lib/model-icons': { getModelIcon: () => ({ Avatar: 'Avatar', color: '#000' }) },
        '@/lib/model-aliases': { marketModelName: (name) => name },
        '@/components/ui/popover': widgets('Popover PopoverContent PopoverTrigger'),
        '@/components/ui/badge': widgets('Badge'),
        '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join(' ') },
        '@/components/common/CopyButton': widgets('CopyIconButton'),
        '@/components/common/SafeText': widgets('ErrorSafeText MonoSafeText SafeText'),
        '@/components/ui/morphing-dialog': widgets('MorphingDialog MorphingDialogTrigger MorphingDialogContainer MorphingDialogContent MorphingDialogClose MorphingDialogTitle MorphingDialogDescription'),
        '@/api/endpoints/user': { useAuthStore: (select) => select({ user: { role: 'admin' } }) },
    });
}

// LogCard.type(...) returns an element tree; the card header is a LogRouteHeader
// element, so it must be invoked explicitly before its data-* hooks exist.
function renderCardHeader(LogCard, log) {
    const tree = LogCard.type({ log, auditByChannelModel: undefined });
    const headerEl = findElement(tree, (node) => typeof node.type === 'function' && node.type.name === 'LogRouteHeader' && node.props?.variant === 'card');
    assert.ok(headerEl, 'card variant LogRouteHeader element must exist');
    return headerEl.type(headerEl.props);
}

function collect(node, predicate, out = []) {
    if (Array.isArray(node)) { node.forEach((child) => collect(child, predicate, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (predicate(node)) out.push(node);
    collect(node.props?.children, predicate, out);
    return out;
}

const sampleLog = (overrides = {}) => ({
    id: 1, time: 1, channel: 7, channel_name: 'workbuddy',
    request_model_name: 'deepseek-v4.1-flash', actual_model_name: 'cn:deepseek-v4.1-flash',
    upstream_response_model: 'deepseek-v4.1-flash', upstream_model_mismatch: true,
    upstream_mismatch_code: '', model_family_verdict: 'suspect',
    request_user_agent: 'pi/1.0', is_stream: true,
    attempts: [
        { status: 'failed', channel_name: 'workbuddy', duration: 1, model_name: 'deepseek-v4.1-flash', attempt_num: 1 },
        { status: 'success', channel_name: 'workbuddy', duration: 1, model_name: 'deepseek-v4.1-flash', attempt_num: 2 },
    ],
    input_tokens: 0, output_tokens: 0, cache_hit_tokens: 0, cost: 0, use_time: 1, ftut: 1,
    ...overrides,
});

test('card header is one shared 6-col/2-row grid with an arrow over the channel column', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog());
    assert.notEqual(grid.props['data-log-header'], undefined, 'header root carries data-log-header');

    const cells = collect(grid.props.children, (node) => node.props && node.props['data-log-row'] !== undefined).map((node) => node.props);
    const has = (row, cell) => cells.some((p) => p['data-log-row'] === row && p['data-log-cell'] === cell);
    assert.ok(has('1', '1'), 'row1 col1 (status/endpoint/path)');
    assert.ok(has('1', '2'), 'row1 col2 (request model)');
    assert.ok(has('1', '3'), 'row1 col3 (arrow, 14px column)');
    assert.ok(has('1', '4'), 'row1 col4 (channel / retry badge)');
    assert.ok(has('1', '5'), 'row1 col5 (actual model, never hidden)');
    assert.ok(has('1', '6'), 'row1 col6 (stream)');
    assert.ok(has('2', '1'), 'row2 col1 (client)');
    assert.ok(has('2', '4'), 'row2 col4 (evidence under channel column)');
    assert.ok(has('2', '6'), 'row2 col6 (details, clickable on all sizes)');

    const evidence = cells.find((p) => p['data-log-row'] === '2' && p['data-log-cell'] === '4');
    assert.match(String(evidence.className), /col-start-4/);
    assert.match(String(evidence.className), /col-span-2/);
    assert.match(String(evidence.className), /row-start-2/);
    assert.ok(findElement(evidence.children, (node) => node.type === 'ArrowDown'), 'evidence cell contains a down arrow');

    // 重试渠道徽标是一个独立元素（不是把 Fragment 当一个 grid 子节点撑破列），落在 col4 格子里。
    const channelCell = cells.find((p) => p['data-log-row'] === '1' && p['data-log-cell'] === '4');
    assert.ok(
        findElement(channelCell.children, (node) => typeof node.type === 'function' && node.type.name === 'RetryBadgeWithPopover'),
        'retry channel badge lives inside the col4 cell'
    );
});

test('summary band exposes data-log-row 3 and 4', () => {
    const { LogCard } = loadCard();
    const tree = LogCard.type({ log: sampleLog({ id: 2, channel_name: 'c', upstream_response_model: '', upstream_model_mismatch: undefined, model_family_verdict: '', attempts: [] }), auditByChannelModel: undefined });
    const rows = collect(tree, (node) => node.props && (node.props['data-log-row'] === '3' || node.props['data-log-row'] === '4')).map((node) => node.props['data-log-row']);
    assert.ok(rows.includes('3'), 'summary row 3 present');
    assert.ok(rows.includes('4'), 'summary row 4 present');
});

test('echo tone none still shows the existing echo name (never an empty row)', () => {
    const { LogCard } = loadCard();
    // mismatch 未计算/为 false（非 true）+ verdict 空 + 非 cn: 等价 -> tone none，但上游已自报。
    const grid = renderCardHeader(LogCard, sampleLog({ upstream_model_mismatch: false, model_family_verdict: '', upstream_response_model: 'gpt-5.5-2025', actual_model_name: 'gpt-5.5' }));
    assert.ok(findElement(grid, (node) => node.props?.value === 'gpt-5.5-2025'), 'echo name is displayed when present');
});

test('echo tone none with no echo shows upstreamSilent', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog({ upstream_response_model: '', upstream_model_mismatch: undefined, model_family_verdict: '' }));
    assert.ok(findElement(grid, (node) => node.props?.children === 'upstreamSilent'), 'upstreamSilent placeholder is displayed');
});
