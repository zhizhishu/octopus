// Run from web/: node --test tests/log-echo-verdict.test.cjs
//
// 覆盖日志卡片「回显判定」收敛（humanize.describeEchoComparison）、卡片头部的
// 可读性契约（自然三分区两排 grid / 不做断点式 max-w 挤压 / 回显格箭头阅读序）
// 与交互徽标的键盘激活（Enter/Space 就地开 Popover，不误开整卡）。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const React = require('react');
const { load } = require('./load-typescript.cjs');

const { describeEchoComparison, isRegionNamespaceHint, humanizeClient } = load('src/components/modules/log/humanize.ts');
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

// astra 裁决（2026-10-27）：moonshot 家族的规范回显（kimi-3.1-pro → moonshotai/Kimi-K3）
// 由后端降级成 alias_variant。UI 必须按 code 分档——渲染灰色「名称变体」而不是琥珀「回显不符」。
test('kimi canonical echo: alias_variant code renders the variant tone, never 回显不符', () => {
    const c = describeEchoComparison({
        ...base,
        request_model_name: 'kimi-3.1-pro',
        actual_model_name: 'kimi-3.1-pro',
        upstream_response_model: 'moonshotai/Kimi-K3',
        upstream_model_mismatch: true,
        upstream_mismatch_code: 'alias_variant',
        model_family_verdict: 'suspect',
    }, display);
    assert.equal(c.tone, 'alias-variant');
    assert.notEqual(c.tone, 'name-mismatch');
    assert.equal(c.fromRaw, 'kimi-3.1-pro');
    assert.equal(c.toRaw, 'moonshotai/Kimi-K3');
});

// 反向契约：同一条回显但没有 alias_variant code（= 后端认定「完全不符」），
// 必须仍然渲染琥珀「回显不符」。证明 UI 是按 code 分档，不是把 kimi 名字一律放过。
test('kimi canonical echo without alias code still renders 回显不符', () => {
    const c = describeEchoComparison({
        ...base,
        request_model_name: 'kimi-3.1-pro',
        actual_model_name: 'kimi-3.1-pro',
        upstream_response_model: 'moonshotai/Kimi-K3',
        upstream_model_mismatch: true,
        upstream_mismatch_code: '',
        model_family_verdict: 'suspect',
    }, display);
    assert.equal(c.tone, 'name-mismatch');
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

test('unrecognized long client identifiers remain complete before layout', () => {
    for (const ua of ['deepseek-harness/0.1.5-rc.2 (win32; x64)', 'pi (win32 10.0.26200; x64) production-worker']) {
        assert.equal(humanizeClient(ua), ua, 'the formatter must not pre-truncate the client label');
    }
    assert.equal(humanizeClient('claude-cli/2.1.296 (external, sdk-cli)'), 'Claude CLI');
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

function text(node) {
    if (node == null || typeof node === 'boolean') return '';
    if (typeof node === 'string' || typeof node === 'number') return String(node);
    if (Array.isArray(node)) return node.map(text).join(' ');
    return text(node.props?.children);
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
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogHeader DialogTitle'),
        '@/api/endpoints/user': { useAuthStore: (select) => select({ user: { role: 'admin' } }) },
    });
}

// LogCard.type(...) returns an element tree; the card header is a LogRouteHeader
// element, so it must be invoked explicitly before its data-* hooks exist.
function renderCardHeader(LogCard, log) {
    const tree = LogCard.type({ log });
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

// 造一个最小的 keyboard 事件：只关心 preventDefault / stopPropagation / currentTarget.click。
function fireKey(handler, key) {
    const calls = { preventDefault: 0, stopPropagation: 0, click: 0 };
    handler({
        key,
        preventDefault: () => { calls.preventDefault += 1; },
        stopPropagation: () => { calls.stopPropagation += 1; },
        currentTarget: { click: () => { calls.click += 1; } },
    });
    return calls;
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

// 名称不符：发出 gpt-example / 回显 claude-example，mismatch=true、verdict=suspect 且无 cross_family code
// -> tone = name-mismatch，才会渲染「回显不符」Popover 徽标与 from → to 对照。
const mismatchLog = (overrides = {}) => sampleLog({
    request_model_name: 'claude-example',
    actual_model_name: 'gpt-example',
    upstream_response_model: 'claude-example',
    upstream_model_mismatch: true,
    upstream_mismatch_code: '',
    model_family_verdict: 'suspect',
    ...overrides,
});

// 带上 request_endpoint 与 attempt.upstream_path，才能同时渲染「接口」与「上游路径」两个 14rem 徽标。
const routedLog = (overrides = {}) => sampleLog({
    request_endpoint: '/v1/chat/completions',
    attempts: [
        { status: 'failed', channel_name: 'workbuddy', duration: 1, model_name: 'deepseek-v4.1-flash', attempt_num: 1, upstream_path: '/v1/chat/completions' },
        { status: 'success', channel_name: 'workbuddy', duration: 1, model_name: 'deepseek-v4.1-flash', attempt_num: 2 },
    ],
    ...overrides,
});

test('card header is a natural 3-zone grid, not per-breakpoint max-w squeezing', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog());
    assert.notEqual(grid.props['data-log-header'], undefined, 'header root carries data-log-header');

    const cls = String(grid.props.className);
    assert.match(cls, /md:grid-cols-\[auto_minmax\(0,1fr\)_auto\]/, 'desktop keeps status and model on the same row');
    assert.doesNotMatch(cls, /grid-cols-\[minmax\(0,1fr\)_auto\]/, 'mobile no longer gives the model its own full row');
    assert.doesNotMatch(cls, /grid-rows-\[/, 'no forced equal-height row track rem');
    assert.doesNotMatch(cls, /fit-content/, 'no fit-content squeeze column');

    const cells = collect(grid.props.children, (node) => node.props && node.props['data-log-row'] !== undefined).map((node) => node.props);
    const has = (row, cell) => cells.some((p) => p['data-log-row'] === row && p['data-log-cell'] === cell);
    assert.ok(has('1', '1'), 'row1 col1 (status · rescue · endpoint · path)');
    assert.ok(has('1', '2'), 'row1 col2 (request → channel → sent name)');
    assert.ok(has('1', '3'), 'row1 col3 (stream · pin)');
    assert.ok(has('2', '1'), 'row2 col1 (client)');
    assert.ok(has('2', '2'), 'row2 col2 (echo evidence · audit)');
    assert.ok(has('2', '3'), 'row2 col3 (details)');
    assert.equal(cells.length, 6, 'exactly three zones × two rows, no synthetic spacer cells');
    for (const p of cells) {
        assert.doesNotMatch(String(p.className), /row-start-/, 'cells follow the production row order instead of manual row placement');
    }

    for (const p of cells) {
        assert.match(String(p.className), /\bflex\b/, 'every zone group is a flex row');
        assert.match(String(p.className), /flex-(wrap|nowrap)/, 'and states its wrapping policy explicitly');
    }
    const echoCell = cells.find((p) => p['data-log-row'] === '2' && p['data-log-cell'] === '2');
    assert.match(String(echoCell.className), /flex-nowrap/, 'echo cell keeps the arrow pinned (outer row never wraps)');
    assert.doesNotMatch(String(echoCell.className), /flex-wrap/, 'no flex-wrap on the echo cell outer row');

    // 重试渠道徽标是一个独立元素（不是把 Fragment 当一个 grid 子节点撑破列）。
    const retry = findElement(grid, (node) => typeof node.type === 'function' && node.type.name === 'RetryBadgeWithPopover');
    assert.ok(retry, 'retry channel badge lives inside the route zone');
    const retryTree = retry.type(retry.props);
    assert.ok(findElement(retryTree, (node) => node.type === 'Badge' && String(node.props.className).includes('max-w-[min(12rem,100%)]')), 'retry badge must stay within the channel track');
});

test('endpoint / upstream-path badges use a single fixed max-w (no breakpoint ladder)', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, routedLog());
    const badges = collect(grid.props.children, (node) => node.props && String(node.props.className).includes('max-w-[14rem]'));
    assert.equal(badges.length, 2, 'endpoint badge + upstream-path badge both capped at 14rem');
    for (const node of badges) {
        const cls = String(node.props.className);
        assert.doesNotMatch(cls, /sm:max-w-/, 'no sm: max-w tier');
        assert.doesNotMatch(cls, /lg:max-w-/, 'no lg: max-w tier');
        assert.match(cls, /border-border\/70 bg-muted\/40/, 'same quiet warm-paper chrome');
    }
});

test('echo evidence cell pins the down arrow and wraps the echo group on an inner row', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, mismatchLog());
    const cell = findElement(grid, (node) => node.props?.['data-log-cell'] === '2' && node.props?.['data-log-row'] === '2');
    assert.ok(cell, 'row2 col2 evidence cell exists');
    const children = cell.props.children;
    assert.equal(children[0]?.type, 'ArrowDown', 'down arrow is the first, fixed child (never follows the echo group)');

    const inner = children[1];
    assert.ok(inner, 'an inner container holds the echo evidence');
    assert.match(String(inner.props.className), /flex-wrap/, 'inner container wraps long echo text');
    assert.match(String(inner.props.className), /\bmin-w-0\b/, 'inner container can shrink inside the pinned row');
    assert.ok(findElement(inner, (node) => node.props?.children === 'echoMismatch'), 'echo group lives inside the inner container');
});

test('admin row keeps the passive summary chip and the down arrow even without echo/audit', () => {
    const { LogCard } = loadCard();
    // 无回显/无主动字段；nil 被动字段 -> 「未分析」，不伪造样本。
    const grid = renderCardHeader(LogCard, sampleLog({
        channel_name: '', upstream_response_model: '', upstream_model_mismatch: undefined,
        upstream_mismatch_code: '', model_family_verdict: '',
    }));
    const cell = findElement(grid, (node) => node.props?.['data-log-cell'] === '2' && node.props?.['data-log-row'] === '2');
    assert.ok(cell, 'row2 col2 cell still exists as a layout slot');
    assert.equal(cell.props.children[0]?.type, 'ArrowDown', 'passive summary keeps the down arrow');
    assert.ok(findElement(cell, (node) => node.props?.children === 'passiveBadgeMissing'), 'nil passive -> not-analyzed chip');
});

test('passive summary badge shows the six features and stays neutral (never a risk colour)', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog({
        passive_summary: {
            version: 'v1', status: 'insufficient_evidence', reason: 'no_calibrated_profile', language: 'zh',
            features: { chars: 120, lines: 4, paragraphs: 2, code_blocks: 0, code_ratio: 0, markdown_lines: 1 },
        },
    }));
    const badge = collect(grid.props.children, (node) => node.type === 'Badge' && node.props?.children === 'passiveBadgeInsufficient')[0];
    assert.ok(badge, 'insufficient passive badge exists');
    const cls = String(badge.props.className);
    assert.doesNotMatch(cls, /amber-/, 'passive badge never amber');
    assert.doesNotMatch(cls, /red-/, 'passive badge never red');
    assert.match(cls, /text-muted-foreground/, 'passive badge is neutral');
    const popover = findElement(grid, (node) => node.type === 'Popover' && text(node).includes('passiveBadgeInsufficient'));
    assert.ok(popover, 'passive popover exists');
    const content = text(popover);
    assert.match(content, /passiveFeatureChars/);
    assert.ok(content.includes('120'), 'chars value shown');
    assert.ok(content.includes('v1'), 'version shown');
    assert.ok(content.includes('zh'), 'coarse language shown');
    assert.ok(content.includes('no_calibrated_profile') === false, 'insufficient reason is not dumped as a raw code');
});

test('not_applicable passive summary is neutral and shows its reason, never a warning', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog({
        passive_summary: { version: 'v1', status: 'not_applicable', reason: 'tool_output' },
    }));
    const badge = collect(grid.props.children, (node) => node.type === 'Badge' && node.props?.children === 'passiveBadgeNotApplicable')[0];
    assert.ok(badge, 'not-applicable passive badge exists');
    const cls = String(badge.props.className);
    assert.doesNotMatch(cls, /amber-/, 'not a warning colour');
    assert.doesNotMatch(cls, /red-/, 'not a risk colour');
    const popover = findElement(grid, (node) => node.type === 'Popover' && text(node).includes('passiveBadgeNotApplicable'));
    assert.ok(popover, 'not-applicable popover exists');
    assert.ok(text(popover).includes('tool_output'), 'reason shown in the popover');
});

test('client badge shows fully: no ellipsis, wraps long words, badge overflow unlocked', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog());
    const client = collect(grid.props.children, (node) => node.type === 'Badge' && String(node.props.className).includes('break-all'))[0];
    assert.ok(client, 'client badge exists');
    const cls = String(client.props.className);
    assert.match(cls, /overflow-visible/, 'overrides Badge base overflow-hidden');
    assert.match(cls, /whitespace-normal/, 'overrides Badge base whitespace-nowrap');
    assert.match(cls, /break-all/, 'long client tokens break instead of ellipsis');
    assert.doesNotMatch(cls, /truncate/, 'never degrades to ellipsis');
});

test('card model names truncate so the header stays two rows', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog());
    const modelTexts = collect(grid.props.children, (node) => node.props && node.props.mode === 'truncate');
    assert.ok(modelTexts.length >= 2, 'request model and sent model truncate inside the card');
});

test('summary band no longer forces both rows onto one line', () => {
    const { LogCard } = loadCard();
    const tree = LogCard.type({ log: sampleLog({ id: 3 }) });
    const rows = collect(tree, (node) => node.props && (node.props['data-log-row'] === '3' || node.props['data-log-row'] === '4')).map((node) => node.props);
    assert.equal(rows.length, 2, 'both summary rows present');
    for (const p of rows) {
        assert.match(String(p.className), /flex-wrap/, 'summary rows wrap instead of clipping');
        assert.doesNotMatch(String(p.className), /flex-nowrap/, 'no forced single line');
        assert.doesNotMatch(String(p.className), /overflow-hidden/, 'no clipping that hides metrics');
    }
});

test('echo comparison text wraps and risk badges override the Badge overflow clip', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, mismatchLog());

    const echoGroupEl = findElement(grid, (node) => node.props?.['data-echo-state'] !== undefined);
    assert.ok(echoGroupEl, 'echo group exists for a name mismatch');
    const echoTexts = collect(echoGroupEl, (node) => node.type === 'SafeText');
    assert.ok(echoTexts.length >= 2, 'both sides of the comparison are rendered');
    for (const node of echoTexts) {
        assert.equal(node.props.mode, 'wrap', 'echo/comparison text wraps instead of falling back to truncate');
    }

    const risk = collect(grid.props.children, (node) => node.type === 'Badge' && node.props?.children === 'echoMismatch')[0];
    assert.ok(risk, 'echo-mismatch risk badge exists');
    const cls = String(risk.props.className);
    assert.match(cls, /overflow-visible/, 'risk badge overrides Badge base overflow-hidden');
    assert.match(cls, /whitespace-normal/, 'risk badge overrides Badge base whitespace-nowrap');
    assert.doesNotMatch(cls, /truncate/, 'risk badge never truncates its title');
});

test('interactive badges activate on Enter/Space in place, and ignore other keys', () => {
    const { LogCard } = loadCard();
    // name-mismatch 渲染「回显不符」Popover 徽标 + 客户端徽标 + 被动摘要徽标（grid 内 3 个）+ 重试渠道徽标（retryTree 1 个）。
    // 该行没有本行审计字段 → 无 detect 触发器（快照不冒充本行）；被动摘要与主动字段隔离。
    const grid = renderCardHeader(LogCard, mismatchLog());

    const retry = findElement(grid, (node) => typeof node.type === 'function' && node.type.name === 'RetryBadgeWithPopover');
    assert.ok(retry, 'retry badge element found');
    const retryTree = retry.type(retry.props);

    const triggers = [grid, retryTree].flatMap((source) => collect(source, (node) => typeof node.props?.onKeyDown === 'function'));
    assert.equal(triggers.length, 4, 'client + echo-mismatch + passive summary + retry; a row without own audit fields has no detect trigger');
    const handlers = triggers.map((node) => node.props.onKeyDown);
    for (const trigger of triggers) {
        let bubbled = 1;
        trigger.props.onClick({ stopPropagation() { bubbled = 0; } });
        assert.equal(bubbled, 0, 'the dispatched click is stopped before reaching the card');
    }

    for (const handler of handlers) {
        for (const key of ['Enter', ' ']) {
            assert.deepEqual(fireKey(handler, key), { preventDefault: 1, stopPropagation: 1, click: 1 }, `key "${key}" activates the badge and stops bubbling`);
        }
        assert.deepEqual(fireKey(handler, 'Tab'), { preventDefault: 0, stopPropagation: 1, click: 0 }, 'other keys only stop bubbling, never open the card');
    }
});

test('summary band exposes data-log-row 3 and 4', () => {
    const { LogCard } = loadCard();
    const tree = LogCard.type({ log: sampleLog({ id: 2, channel_name: 'c', upstream_response_model: '', upstream_model_mismatch: undefined, model_family_verdict: '', attempts: [] }) });
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

// 真实生产样本：request=actual=grok-4.7，echo=grok-4.7-build，code=alias_variant，mismatch=true，verdict=suspect，
// 行审计字段为空。这是「名字变体被展示成风险」的现象修正：仍保留原始对照，只把语义降为中性名称变体。
test('real Grok alias sample: name variant stays visible, neutral tone, raw names untouched', () => {
    const grokLog = sampleLog({
        request_model_name: 'grok-4.7',
        actual_model_name: 'grok-4.7',
        upstream_response_model: 'grok-4.7-build',
        upstream_model_mismatch: true,
        upstream_mismatch_code: 'alias_variant',
        model_family_verdict: 'suspect',
    });
    // 判定层：仍是 alias-variant，tone 与原始字段不改（不统统等价、不改后台 mismatch）。
    const c = describeEchoComparison(grokLog, display);
    assert.equal(c.tone, 'alias-variant');
    assert.equal(c.fromRaw, 'grok-4.7');
    assert.equal(c.toRaw, 'grok-4.7-build');
    assert.equal(c.showComparison, true);

    // 展示层：中性低强调徽标（非风险色），两侧原始名（含 -build）都在、未被删改。
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, grokLog);
    const badge = collect(grid.props.children, (node) => node.type === 'Badge' && node.props?.children === 'echoAliasVariant')[0];
    assert.ok(badge, 'alias-variant badge exists');
    const cls = String(badge.props.className);
    assert.doesNotMatch(cls, /amber-/, 'alias badge is not amber');
    assert.doesNotMatch(cls, /red-/, 'alias badge is not red');
    assert.match(cls, /text-muted-foreground/, 'alias badge is low emphasis');
    assert.ok(findElement(grid, (node) => node.props?.value === 'grok-4.7-build'), 'echo name kept verbatim');
    assert.ok(findElement(grid, (node) => node.props?.value === 'grok-4.7'), 'sent name kept verbatim');
});

// 未知家族：不得被归成 cross-family 红标（证据不足就证据不足）。
test('unknown family verdict never becomes a cross-family red flag', () => {
    const c = describeEchoComparison({ ...base, request_model_name: 'grok-4.7', actual_model_name: 'grok-4.7', upstream_response_model: 'some-other-model', upstream_model_mismatch: true, model_family_verdict: 'unknown' }, display);
    assert.notEqual(c.tone, 'cross-family');
});

// 已知 cross-family 仍可见、不隐藏：保留徽标与两侧对照。
test('known cross-family stays visible with both sides of the comparison', () => {
    const { LogCard } = loadCard();
    const grid = renderCardHeader(LogCard, sampleLog({
        request_model_name: 'gpt-5.5',
        actual_model_name: 'gpt-5.5',
        upstream_response_model: 'deepseek-v4.1-flash',
        upstream_model_mismatch: false,
        upstream_mismatch_code: 'cross_family',
        model_family_verdict: 'suspect',
    }));
    assert.ok(collect(grid.props.children, (node) => node.type === 'Badge' && node.props?.children === 'familyReplaced')[0], 'cross-family badge still rendered');
    assert.ok(findElement(grid, (node) => node.props?.value === 'deepseek-v4.1-flash'), 'echo side visible');
    assert.ok(findElement(grid, (node) => node.props?.value === 'gpt-5.5'), 'source side visible');
});
