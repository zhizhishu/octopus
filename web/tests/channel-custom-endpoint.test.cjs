/* eslint-disable @typescript-eslint/no-require-imports */

'use strict';

// Run from web/: node --test tests/channel-custom-endpoint.test.cjs
//
// 钉住「自定义渠道(6) 的测试接口」这条改动的四条硬约束：
//   1. 接口选项的 label 一律英文（Custom (full URL) / Chat / … / Decision），
//      中文只留在选项之外的说明文字里；
//   2. type 6 默认落到 'custom'（配置里那个完整网址），其它类型的默认值不变；
//   3. 'Custom (full URL)' 只在 type 6 渲染——别的渠道选到它就是一条走不通的端点；
//   4. type 6 的手动刷新可用（错误原文由既有 toast 露出），自动同步仍然关着。
//
// harness 与 channel-test-ui.test.cjs 同源：轻量 hook 实现 + 组件桩，
// 无浏览器、无上游调用、无额外依赖。
const test = require('node:test');
const assert = require('node:assert/strict');
const { load } = require('./load-typescript.cjs');

// 枚举值与 src/api/endpoints/channel.ts:22-31 的真实定义逐一对齐。
const ChannelType = { OpenAIChat: 0, OpenAIResponse: 1, Anthropic: 2, Gemini: 3, OpenAIEmbedding: 5, CustomOpenAIChat: 6, SystemOne: 7 };
const aliases = load('src/lib/model-aliases.ts');
const helpers = load('src/lib/channel-test.ts', { '@/api/endpoints/channel': { ChannelType } });
const capabilities = load('src/lib/channel-capabilities.ts');
const {
    MODEL_TEST_ENDPOINTS,
    MODEL_TEST_ENDPOINT_LABELS,
    channelTypeForTestEndpoint,
    defaultModelTestEndpointForChannel,
    modelTestEndpointsForChannel,
} = helpers;

const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));
const ENDPOINT_LABELS = new Set(MODEL_TEST_ENDPOINTS.map((endpoint) => endpoint.label));
const noop = () => {};
function find(node, predicate, found = []) {
    if (Array.isArray(node)) { node.forEach((child) => find(child, predicate, found)); return found; }
    if (!node || typeof node !== 'object') return found;
    if (predicate(node)) found.push(node);
    find(node.props?.children, predicate, found);
    return found;
}
function text(node) {
    if (node == null || typeof node === 'boolean') return '';
    if (typeof node !== 'object') return String(node);
    return Array.isArray(node) ? node.map(text).join('') : text(node.props?.children);
}
function hooks() {
    let slots = [], cursor = 0, effects = [];
    const react = {
        useState(initial) {
            const index = cursor++;
            if (!slots[index]) slots[index] = { value: typeof initial === 'function' ? initial() : initial };
            const slot = slots[index];
            return [slot.value, (next) => { slot.value = typeof next === 'function' ? next(slot.value) : next; }];
        },
        useRef(initial) { return react.useState(() => ({ current: initial }))[0]; },
        useMemo(fn) { return fn(); },
        useEffect(fn, deps) {
            const index = cursor++;
            if (!slots[index] || !deps || deps.some((value, i) => !Object.is(value, slots[index].deps[i]))) {
                slots[index] = { deps };
                effects.push(fn);
            }
        },
    };
    react.useLayoutEffect = react.useEffect;
    return {
        react,
        reset() { slots = []; },
        render(fn) {
            cursor = 0; effects = [];
            let tree = fn();
            for (const effect of effects) effect();
            cursor = 0; effects = [];
            tree = fn();
            for (const effect of effects) effect();
            return tree;
        },
    };
}
const commonMocks = {
    'next-intl': { useTranslations: () => (key) => key },
    'lucide-react': new Proxy({}, { get: (_, name) => name }),
    '@/components/common/Toast': { toast: { success: noop, error: noop, warning: noop } },
    '@/components/ui/button': widgets('Button'),
    '@/components/ui/input': widgets('Input'),
    '@/components/ui/switch': widgets('Switch'),
    '@/components/ui/select': widgets('Select SelectContent SelectItem SelectTrigger SelectValue'),
    '@/components/ui/badge': widgets('Badge'),
    '@/components/ui/accordion': widgets('Accordion AccordionContent AccordionItem AccordionTrigger'),
    '@/components/ui/dialog': widgets('Dialog DialogContent DialogHeader DialogTitle'),
    '@/components/ui/table': widgets('Table TableBody TableCell TableHead TableHeader TableRow'),
    '@/components/animate-ui/components/animate/tooltip': widgets('Tooltip TooltipContent TooltipProvider TooltipTrigger'),
    '@/lib/utils': { cn: (...values) => values.filter(Boolean).join(' ') },
    '@/lib/model-aliases': aliases,
    '@/lib/channel-test': helpers,
    '@/lib/channel-capabilities': capabilities,
    '@/hooks/useIsMobile': { useIsMobile: () => false },
};
function draft(overrides = {}) {
    return {
        name: 'test channel', type: 0, priority: 0, max_concurrent: 0, rpm_limit: 0,
        race_mode: false, race_key_concurrency: 2, race_delay_ms: 0, key_select_strategy: 0,
        disable_circuit_breaker: false, base_urls: [{ url: 'https://upstream.example', delay: 0 }],
        custom_header: [{ header_key: '', header_value: '' }], cloak_mode: 'auto', cloak_profile_id: 1,
        cloak_shape: '', channel_proxy: '', param_override: '', keys: [{ enabled: true, channel_key: 'fixture-key' }],
        model: 'sample-model', custom_model: '', discovered_models: [], selected_models: ['sample-model'],
        anthropic_context_1m: false, thinking_to_content: false, capabilities: [], enabled: true,
        proxy: false, auto_sync: false, match_regex: '', openai_chat_path: '', openai_models_path: '',
        openai_embeddings_path: '', openai_images_path: '', ...overrides,
    };
}
function formHarness(initial = draft()) {
    const h = hooks();
    let tree;
    const fetchCalls = [];
    const toastCalls = [];
    const { ChannelForm } = load('src/components/modules/channel/Form.tsx', {
        ...commonMocks,
        react: h.react,
        '@/components/common/Toast': { toast: { success: noop, warning: noop, error: (message, options) => toastCalls.push({ message, options }) } },
        '@/api/endpoints/channel': {
            ChannelType, KeySelectStrategy: { CostBalanced: 0, Sticky: 1 },
            useFetchModel: () => ({ isPending: false, mutate: (payload, callbacks) => fetchCalls.push({ payload, callbacks }) }),
            useTestChannelConfig: () => ({ isPending: false, mutate: noop }),
            useTestChannelProxy: () => ({ isPending: false, mutate: noop }),
            useChannelRPMs: () => ({ data: undefined }),
            channelRPMOf: () => 0,
        },
        '@/api/endpoints/fingerprint-profile': { useFingerprintProfileList: () => ({ data: [{ id: 1, name: 'fixture' }] }) },
        '../navbar/nav-store': { useNavStore: (select) => select({ setActiveItem: noop }) },
        './audit/ModelChipDots': { ...widgets('ModelAuditChipDot ModelAuditChipExpand ModelAuditChipToolbar'), useModelAuditChips: () => [], filterAuditChips: (items) => items },
    });
    const props = {
        formData: initial,
        onFormDataChange(next) { props.formData = typeof next === 'function' ? next(props.formData) : next; },
        onSubmit: noop, isPending: false, submitText: 'save', pendingText: 'saving',
    };
    return {
        props,
        fetchCalls,
        toastCalls,
        render() { tree = h.render(() => ChannelForm(props)); return tree; },
        selector: () => find(tree, (node) => node.type === 'Select' && find(node, (child) => child.props?.id === 'channel-test-endpoint').length > 0)[0],
        refresh: () => find(tree, (node) => node.type === 'Button' && text(node) === 'modelRefresh')[0],
        // 只取「测试接口」那个 Select 里的选项，不把其它下拉框的条目算进来。
        options: () => find(
            find(tree, (node) => node.type === 'Select' && find(node, (child) => child.props?.id === 'channel-test-endpoint').length > 0)[0],
            (node) => node.type === 'SelectItem',
        ).map((node) => text(node)),
        tree: () => tree,
    };
}
function savedChannel(overrides = {}) {
    return { ...draft(), id: 1, cloak: { mode: 'auto', profile_id: 1 }, ...overrides };
}
function dialogHarness(initial = savedChannel()) {
    const h = hooks();
    let tree, key;
    const { ChannelTestDialog } = load('src/components/modules/channel/channel-test-dialog.tsx', {
        ...commonMocks,
        react: h.react,
        '@/api/endpoints/channel': { ChannelType },
        '@/api/endpoints/model': {
            useModelTest: () => ({ mutateAsync: async () => ({ results: [] }) }),
            useChannelTestIdentity: () => ({ data: { profile_name: 'fixture', codex: { user_agent: 'codex fixture' }, claude: { user_agent: 'claude fixture' }, generic: { user_agent: 'generic fixture' } } }),
        },
    });
    const props = { channel: initial, open: true, onOpenChange: noop };
    return {
        props,
        render() {
            const child = ChannelTestDialog(props);
            if (child.key !== key) { h.reset(); key = child.key; }
            tree = h.render(() => child.type(child.props));
            return tree;
        },
        pill: (label) => find(tree, (node) => node.type === 'button' && text(node) === label)[0],
        // 只取接口胶囊：模型胶囊同样带 aria-pressed，不能混进来。
        pressed: () => find(
            tree,
            (node) => node.type === 'button'
                && (node.props ?? {})['aria-pressed'] === true
                && ENDPOINT_LABELS.has(text(node)),
        ).map((node) => text(node)),
        tree: () => tree,
    };
}

/** 英文标签是硬要求：选项文本里不许出现中日韩字符，也不许回退成中文词。 */
test('endpoint option labels are English-only and cover the agreed set', () => {
    const want = {
        custom: 'Custom (full URL)',
        openai_chat: 'Chat',
        openai_responses: 'Responses',
        anthropic_messages: 'Claude Messages',
        gemini_generate_content: 'Gemini',
        openai_embeddings: 'Embeddings',
        openai_images_generations: 'Image Generation',
        systemone: 'Decision',
    };
    assert.deepEqual(Object.fromEntries(MODEL_TEST_ENDPOINTS.map((endpoint) => [endpoint.value, endpoint.label])), want);
    assert.deepEqual({ ...MODEL_TEST_ENDPOINT_LABELS }, want);
    for (const { value, label } of MODEL_TEST_ENDPOINTS) {
        assert.match(label, /^[\x20-\x7e]+$/, `${value} 的 label 必须是纯英文（无 CJK）`);
    }
});

/** type 6 的默认接口是 'custom'；其它类型（含退役的 4 号位）的默认值一个都不许动。 */
test('custom channel defaults to the custom endpoint, other types unchanged', () => {
    const cases = [
        [ChannelType.OpenAIChat, 'openai_chat'],
        [ChannelType.OpenAIResponse, 'openai_responses'],
        [ChannelType.Anthropic, 'anthropic_messages'],
        [ChannelType.Gemini, 'gemini_generate_content'],
        [ChannelType.OpenAIEmbedding, 'openai_embeddings'],
        [ChannelType.CustomOpenAIChat, 'custom'],
        [ChannelType.SystemOne, 'systemone'],
        [4, 'openai_chat'],
    ];
    for (const [type, want] of cases) assert.equal(defaultModelTestEndpointForChannel(type), want, `channel type ${type}`);
});

/** 'Custom (full URL)' 只属于 type 6：别的渠道不该看到一条选了就报错的端点。 */
test('custom endpoint option is offered to type 6 only, every other endpoint stays available', () => {
    const custom = modelTestEndpointsForChannel(ChannelType.CustomOpenAIChat).map((endpoint) => endpoint.value);
    assert.ok(custom.includes('custom'));
    assert.equal(custom.length, MODEL_TEST_ENDPOINTS.length);
    for (const type of [0, 1, 2, 3, 5, 7, 4]) {
        const values = modelTestEndpointsForChannel(type).map((endpoint) => endpoint.value);
        assert.ok(!values.includes('custom'), `channel type ${type} 不该出现 custom`);
        // 其它端点仍然全部可用。
        assert.equal(values.length, MODEL_TEST_ENDPOINTS.length - 1, `channel type ${type} 少了端点`);
        for (const endpoint of ['openai_chat', 'openai_responses', 'anthropic_messages', 'gemini_generate_content', 'openai_embeddings', 'openai_images_generations', 'systemone']) {
            assert.ok(values.includes(endpoint), `channel type ${type} 少了 ${endpoint}`);
        }
    }
});

/** 身份/流式跟着所选端点走：type 6 只有 custom 是原样地址，其余走同源标准路径。 */
test('type 6 keeps its own type for custom and mirrors the protocol for the rest', () => {
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'custom'), ChannelType.CustomOpenAIChat);
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'openai_chat'), ChannelType.OpenAIChat);
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'openai_responses'), ChannelType.OpenAIResponse);
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'anthropic_messages'), ChannelType.Anthropic);
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'gemini_generate_content'), ChannelType.Gemini);
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'openai_embeddings'), ChannelType.OpenAIEmbedding);
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'systemone'), ChannelType.SystemOne);
    // 图片走 relay 的同源标准路径，按 chat 形状算。
    assert.equal(channelTypeForTestEndpoint(ChannelType.CustomOpenAIChat, 'openai_images_generations'), ChannelType.OpenAIChat);
});

/** 编辑表单：type 6 默认 'Custom (full URL)'，普通渠道看不到这个选项。 */
test('edit form defaults a custom channel to Custom (full URL) and hides it elsewhere', () => {
    const custom = formHarness(draft({ type: 6 }));
    custom.render();
    assert.equal(custom.selector().props.value, 'custom');
    assert.ok(custom.options().includes('Custom (full URL)'));

    const chat = formHarness(draft({ type: 0 }));
    chat.render();
    assert.equal(chat.selector().props.value, 'openai_chat');
    assert.ok(!chat.options().includes('Custom (full URL)'));
    // 其它端点照旧可选（英文标签）。
    for (const label of ['Chat', 'Responses', 'Claude Messages', 'Gemini', 'Embeddings', 'Image Generation', 'Decision']) {
        assert.ok(chat.options().includes(label), `缺少选项 ${label}`);
    }
});

/** 选择稳定：普通编辑重渲染不清空选择；换渠道类型才回到该类型的默认值。 */
test('edit form keeps an explicit choice across rerenders and resets on type change', () => {
    const h = formHarness(draft({ type: 6 }));
    h.render();
    h.selector().props.onValueChange('openai_images_generations');
    h.render();
    h.props.formData = { ...h.props.formData, name: 'renamed' };
    h.render();
    assert.equal(h.selector().props.value, 'openai_images_generations');

    h.props.formData = { ...h.props.formData, type: 1 };
    h.render();
    assert.equal(h.selector().props.value, 'openai_responses');
});

/** 手动刷新：type 6 可点，自动同步仍关着；type 7 仍然整块禁用。 */
test('manual refresh is enabled for custom channels while auto-sync stays off', () => {
    const custom = formHarness(draft({ type: 6 }));
    custom.render();
    assert.equal(custom.refresh().props.disabled, false, 'type 6 手动刷新必须可点');
    // auto-sync 那一行仍显示「自定义渠道不支持自动同步」的说明（autoSyncHint 不出现）。
    assert.match(text(custom.tree()), /typeCustomOpenAIChatHint/);
    assert.doesNotMatch(text(custom.tree()), /autoSyncHint/);

    const chat = formHarness(draft({ type: 0 }));
    chat.render();
    assert.equal(chat.refresh().props.disabled, false);
    assert.match(text(chat.tree()), /autoSyncHint/);

    const decision = formHarness(draft({ type: 7 }));
    decision.render();
    assert.equal(decision.refresh().props.disabled, true, 'type 7 没有模型列表地址，刷新仍禁用');
    assert.doesNotMatch(text(decision.tree()), /autoSyncHint/);
});

/**
 * 手动刷新是真发请求的路径：type 6 必须把配置里的完整网址和两条路径字段原样
 * 带出去，后端拒绝时错误原文要能从既有 toast 弹出来。
 * 这里直接点按钮、抓 mutate 入参并驱动 onError，不靠读源码断言。
 */
test('custom refresh sends the full URL plus path fields and surfaces the backend error', async () => {
    const h = formHarness(draft({
        type: 6,
        base_urls: [{ url: 'https://upstream.example/v1/chat/completions', delay: 0 }],
        openai_chat_path: 'https://upstream.example/v1/chat/completions',
        openai_models_path: 'https://upstream.example/v1/models',
        openai_images_path: 'https://upstream.example/v1/images/generations',
        openai_embeddings_path: 'https://upstream.example/v1/embeddings',
    }));
    h.render();
    assert.equal(h.refresh().props.disabled, false);
    await h.refresh().props.onClick();

    assert.equal(h.fetchCalls.length, 1, '点一次刷新只发一次请求');
    const { payload, callbacks } = h.fetchCalls[0];
    assert.equal(payload.type, 6);
    // 完整网址原样带出（type 6 不做后缀剥离），模型地址等路径字段一个不丢。
    assert.equal(payload.base_urls[0].url, 'https://upstream.example/v1/chat/completions');
    assert.equal(payload.openai_chat_path, 'https://upstream.example/v1/chat/completions');
    assert.equal(payload.openai_models_path, 'https://upstream.example/v1/models');
    assert.deepEqual(payload.keys, [{ enabled: true, channel_key: 'fixture-key' }]);

    // 后端拒绝（例如没配标准模型地址）时，原文走既有 toast.error，界面不吞错。
    callbacks.onError(new Error('this channel does not auto-sync a model list; enter model names by hand'));
    assert.equal(h.toastCalls.length, 1);
    assert.equal(h.toastCalls[0].message, 'modelRefreshFailed');
    assert.match(h.toastCalls[0].options.description, /does not auto-sync a model list/);
});

/** 测试对话框：type 6 默认只选 'Custom (full URL)'，普通渠道没有这颗胶囊。 */
test('test dialog defaults a custom channel to the custom pill', () => {
    const h = dialogHarness(savedChannel({ type: 6 }));
    h.render();
    assert.deepEqual(h.pressed(), ['Custom (full URL)']);
    assert.match(text(h.tree()), /当前渠道类型：Custom \(full URL\)/);

    const chat = dialogHarness(savedChannel({ type: 1 }));
    chat.render();
    assert.equal(chat.pill('Custom (full URL)'), undefined);
    assert.deepEqual(chat.pressed(), ['Responses']);
});

/**
 * 同一次挂载里显式选择要留住：关掉再开、区块重渲染都不许悄悄改回默认值
 * （这是渠道测试对话框原有的契约，本次改动不动它）。
 */
test('test dialog keeps an explicit choice across reopen and rerenders', () => {
    const h = dialogHarness(savedChannel({ type: 6 }));
    h.render();
    h.pill('Chat').props.onClick();
    h.render();
    assert.deepEqual(h.pressed().sort(), ['Chat', 'Custom (full URL)']);

    h.props.open = false;
    h.render();
    h.props.open = true;
    h.render();
    assert.deepEqual(h.pressed().sort(), ['Chat', 'Custom (full URL)']);

    // 换渠道类型才回到新类型的默认端点（wrapper 用 id:type 作 key）。
    h.props.channel = { ...h.props.channel, type: 2 };
    h.render();
    assert.deepEqual(h.pressed(), ['Claude Messages']);
});
