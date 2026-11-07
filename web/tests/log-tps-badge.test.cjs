// Run from web/: node --test tests/log-tps-badge.test.cjs
//
// 生成速度徽标的界面契约（2026-11-06）：
//   1. 文本生成行**恒有**徽标——算得出给数字，算不出给中性的「TPS —」+ 人话原因；
//   2. 绝不拿 0 冒充速度，也不把机器码糊到界面上；
//   3. 原因说明走 Popover（点按/Enter/Space 可达），不依赖只有鼠标能看见的 title；
//   4. 图/视频/音频/向量/审核/重排端点与连通性测试行整块不出；
//   5. 管理员载荷（有 error）与用户摘要载荷（只有 error_code/error_status）结论一致。
const assert = require('node:assert/strict');
const { test } = require('node:test');
const React = require('react');
const { load } = require('./load-typescript.cjs');

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

function collect(node, predicate, out = []) {
    if (Array.isArray(node)) { node.forEach((child) => collect(child, predicate, out)); return out; }
    if (!node || typeof node !== 'object') return out;
    if (predicate(node)) out.push(node);
    collect(node.props?.children, predicate, out);
    return out;
}

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

function loadCard(severity = 'success') {
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
        '@/api/endpoints/log': { getRelayLogSeverity: () => severity },
        './humanize': load('src/components/modules/log/humanize.ts'),
        '@/lib/model-icons': { getModelIcon: () => ({ Avatar: 'Avatar', color: '#000' }) },
        '@/lib/model-aliases': { marketModelName: (name) => name },
        '@/components/ui/popover': widgets('Popover PopoverContent PopoverTrigger'),
        '@/components/ui/badge': widgets('Badge'),
        '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join(' ') },
        '@/components/common/CopyButton': widgets('CopyIconButton'),
        './LogImagePlaceholder': { LogImagePlaceholderText: 'LogImagePlaceholderText', renderLogImageJsonString: () => undefined },
        '@/components/common/SafeText': widgets('ErrorSafeText MonoSafeText SafeText'),
        '@/components/ui/morphing-dialog': widgets('MorphingDialog MorphingDialogTrigger MorphingDialogContainer MorphingDialogContent MorphingDialogClose MorphingDialogTitle MorphingDialogDescription'),
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogHeader DialogTitle'),
        '@/api/endpoints/user': { useAuthStore: (select) => select({ user: { role: 'admin' } }) },
    });
}

const sampleLog = (overrides = {}) => ({
    id: 1, time: 1, channel: 7, channel_name: 'c',
    request_model_name: 'deepseek-v4.1-flash', actual_model_name: 'deepseek-v4.1-flash',
    upstream_response_model: '', upstream_model_mismatch: undefined,
    upstream_mismatch_code: '', model_family_verdict: '',
    request_user_agent: 'pi/1.0', is_stream: true,
    request_endpoint: 'chat', request_path: '/v1/chat/completions',
    attempts: [], input_tokens: 0, output_tokens: 0, cache_hit_tokens: 0, cost: 0,
    use_time: 0, ftut: 0,
    ...overrides,
});

// 1000 token / 总 10s / 首字 2s → 生成 8s → 125 t/s
const goodTimings = { output_tokens: 1000, use_time: 10000, ftut: 2000 };

// t() 在测试里返回键名本身，所以徽标文字以 'tps' 开头。
const tpsBadges = (tree) => collect(tree, (node) => node.type === 'Badge' && text(node).trim().startsWith('tps'));

function renderOne(log) {
    const { LogCard } = loadCard();
    const tree = LogCard.type({ log });
    return { tree, badges: tpsBadges(tree) };
}

test('可采信窗口：徽标给出数字（不占位、不省略）', () => {
    const { tree, badges } = renderOne(sampleLog(goodTimings));
    assert.equal(badges.length, 1, 'exactly one TPS badge');
    const label = text(badges[0]);
    assert.ok(label.includes('125'), `badge shows the number; got ${label}`);
    assert.ok(label.includes('tpsUnit'), 'unit comes from the existing locale key');
    const popover = findElement(tree, (node) => node.type === 'Popover' && text(node).includes('tpsHintGen'));
    assert.ok(popover, 'an available reading explains the formula');
});

test('不可采信窗口：中性「TPS —」+ 人话原因，绝不填 0', () => {
    const { tree, badges } = renderOne(sampleLog({ ...goodTimings, output_tokens: 73 }));
    assert.equal(badges.length, 1, 'the badge is still there — absence is not the answer');
    const label = text(badges[0]);
    assert.ok(label.includes('—'), `badge shows a neutral dash; got ${label}`);
    assert.equal(/\d/.test(label), false, `never fake a speed; got ${label}`);
    assert.doesNotMatch(String(badges[0].props.className), /amber-|destructive|red-/, 'a missing reading is not a failure colour');
    const popover = findElement(tree, (node) => node.type === 'Popover' && text(node).includes('tpsUnavailableHint'));
    assert.ok(popover, 'the reason is reachable in the popover');
    const content = text(popover);
    assert.ok(content.includes('tpsReasonTooShort'), 'reason is translated to a human label');
    assert.equal(content.includes('output_too_short'), false, 'raw reason code never reaches the user');
});

// D1 回归：普通用户拿到的是 RelayLogUserSummary（没有 error 字段，只有 error_code/error_status）。
// 同一个失败行两种载荷必须给出同一个原因——不能管理员显示「TPS —」、用户看到编出来的速度。
test('同一条失败行：管理员载荷与用户摘要载荷渲染一致', () => {
    const timings = goodTimings;
    const adminBadge = text(renderOne(sampleLog({ ...timings, error: 'upstream 500', error_code: 'server_error', error_status: 500 })).badges[0]);
    const summaryBadge = text(renderOne(sampleLog({ ...timings, error_code: 'server_error', error_status: 500 })).badges[0]);
    assert.equal(adminBadge, summaryBadge, 'both payload shapes render the same badge');
    assert.equal(/\d/.test(adminBadge), false, 'a failed row never shows a speed');

    const summaryTree = renderOne(sampleLog({ ...timings, error_code: 'server_error', error_status: 500 })).tree;
    const popover = findElement(summaryTree, (node) => node.type === 'Popover' && text(node).includes('tpsUnavailableHint'));
    assert.ok(text(popover).includes('tpsReasonFailed'), 'the summary payload still lands on request_failed');
});

test('重试后成功（warn）照常给数字：闸门只拒失败，不拒重试', () => {
    const { badges } = renderOne(sampleLog({ ...goodTimings, total_attempts: 3 }));
    assert.ok(text(badges[0]).includes('125'), 'a retried-but-successful row keeps its reading');
});

test('图/视频/音频/向量/审核/重排端点与连通性测试行整块不出 TPS', () => {
    for (const endpoint of ['images', 'images_generations', 'images_async', 'videos', 'videos_poll', 'audio_speech', 'audio_tts', 'audio_transcriptions', 'embeddings', 'edits', 'moderations', 'rerank']) {
        const { badges } = renderOne(sampleLog({ ...goodTimings, request_endpoint: endpoint, request_path: '/v1/images/generations' }));
        assert.equal(badges.length, 0, `${endpoint} must not show a token/s reading`);
    }
    const { badges } = renderOne(sampleLog({ ...goodTimings, request_endpoint: 'model_test_chat', request_path: '/v1/test' }));
    assert.equal(badges.length, 0, 'connectivity test rows stay out of the TPS story');
});

test('旧行/未知端点沿用旧口径：仍然给徽标 + 原因，而不是少一个指标', () => {
    const { badges } = renderOne(sampleLog({ output_tokens: 500, use_time: 5000, ftut: 0, request_endpoint: '', request_path: '' }));
    assert.equal(badges.length, 1, 'legacy row without an endpoint keeps the badge');
    assert.ok(text(badges[0]).includes('—'), 'legacy row explains itself instead of disappearing');
});

test('缓冲/部分窗口不冒充速度：几种真实拒算形态都只出「—」', () => {
    const cases = {
        buffered: { output_tokens: 5874, use_time: 24133, ftut: 24132 },
        partial: { output_tokens: 500, use_time: 10000, ftut: 9000 },
        blowup: { output_tokens: 5000, use_time: 3000, ftut: 1000 },
        noTimings: { output_tokens: 500, use_time: 5000, ftut: 0 },
    };
    for (const [name, timings] of Object.entries(cases)) {
        const { badges } = renderOne(sampleLog(timings));
        assert.equal(badges.length, 1, `${name}: badge present`);
        assert.equal(/\d/.test(text(badges[0])), false, `${name}: no fabricated speed`);
    }
});

test('估算用量在详情里点明分子是估算值', () => {
    const estimated = renderOne(sampleLog({ ...goodTimings, usage_source: 'local_estimate' }));
    assert.ok(text(findElement(estimated.tree, (node) => node.type === 'Popover' && text(node).includes('tpsHintGen'))).includes('tpsEstimateCaveat'), 'local_estimate gets the caveat');

    const reported = renderOne(sampleLog({ ...goodTimings, usage_source: 'upstream_usage' }));
    assert.equal(text(reported.tree).includes('tpsEstimateCaveat'), false, 'upstream-reported usage has no caveat');
});

test('徽标现在是原生 button：键盘/触摸可展开，且不误开整卡', () => {
    // 真浏览器的 Tab 顺序 / Enter 触发 click 这一层仍待实测（这里只验静态结构）：
    // 断言的是「不再手工造 key 处理器」，不是「浏览器已经测过」。
    const { tree, badges } = renderOne(sampleLog({ ...goodTimings, output_tokens: 73 }));
    const badge = badges[0];
    // 语义来自原生元素：不再给 span 手工按 role/tabIndex/keyDown 补一套。
    assert.equal(badge.props.role, undefined, 'no hand-rolled role="button" on the badge');
    assert.equal(badge.props.tabIndex, undefined, 'no hand-rolled tabIndex on the badge');
    assert.equal(badge.props.onKeyDown, undefined, 'no hand-rolled key handler on the badge');

    const trigger = findElement(tree, (node) => node.type === 'button' && String(node.props.title ?? '').includes('tps'));
    assert.ok(trigger, 'the popover trigger is a native <button>');
    assert.equal(trigger.props.type, 'button', 'type="button": never submits an enclosing form');

    let clickBubbled = 1;
    trigger.props.onClick({ stopPropagation() { clickBubbled = 0; } });
    assert.equal(clickBubbled, 0, 'the click is stopped before reaching the card');

    // Enter/Space 由浏览器转成 click：keydown 只拦冒泡，不自己 preventDefault + click()。
    const calls = { preventDefault: 0, stopPropagation: 0, click: 0 };
    trigger.props.onKeyDown({
        key: 'Enter',
        preventDefault: () => { calls.preventDefault += 1; },
        stopPropagation: () => { calls.stopPropagation += 1; },
        currentTarget: { click: () => { calls.click += 1; } },
    });
    assert.deepEqual(calls, { preventDefault: 0, stopPropagation: 1, click: 0 }, 'keydown only stops bubbling to the card; activation stays native');

    assert.ok(String(trigger.props.title).includes('tpsUnavailableHint'), 'hover text also carries the explanation, never only a number');
    assert.ok(findElement(tree, (node) => node.type === 'PopoverContent'), 'popover content exists for touch/keyboard users');
});
