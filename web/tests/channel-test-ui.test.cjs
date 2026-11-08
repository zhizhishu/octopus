'use strict';

// Run from web/: node --test tests/channel-test-ui.test.cjs
// Exercise production React handlers with the lightweight hook harness used by
// the existing dialog tests. No browser, upstream calls, or extra dependencies.
const test = require('node:test');
const assert = require('node:assert/strict');
const { load } = require('./load-typescript.cjs');

const ChannelType = { OpenAIChat: 0, OpenAIResponse: 1, Anthropic: 2, Gemini: 3, OpenAIEmbedding: 5, CustomOpenAIChat: 6, SystemOne: 7 };
const aliases = load('src/lib/model-aliases.ts');
const helpers = load('src/lib/channel-test.ts', { '@/api/endpoints/channel': { ChannelType } });
const capabilities = load('src/lib/channel-capabilities.ts');
const widgets = (names) => Object.fromEntries(names.split(' ').map((name) => [name, name]));
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
    const h = hooks(), writes = [];
    let pending = false, tree;
    const { ChannelForm } = load('src/components/modules/channel/Form.tsx', {
        ...commonMocks,
        react: h.react,
        '@/api/endpoints/channel': {
            ChannelType, KeySelectStrategy: { CostBalanced: 0, Sticky: 1 },
            useFetchModel: () => ({ isPending: false, mutate: noop }),
            useTestChannelConfig: () => ({ isPending: pending, mutate: (request, callbacks) => writes.push({ request, callbacks }) }),
            useTestChannelProxy: () => ({ isPending: false, mutate: noop }),
        },
        '@/api/endpoints/fingerprint-profile': { useFingerprintProfileList: () => ({ data: [{ id: 1, name: 'fixture' }] }) },
        '../navbar/nav-store': { useNavStore: (select) => select({ setActiveItem: noop }) },
        './audit/ModelChipDots': { ...widgets('ModelAuditChipDot ModelAuditChipExpand ModelAuditChipToolbar'), useModelAuditChips: () => [], filterAuditChips: (items) => items },
    });
    const props = {
        formData: initial, onFormDataChange(next) { props.formData = typeof next === 'function' ? next(props.formData) : next; },
        onSubmit: noop, isPending: false, submitText: 'save', pendingText: 'saving',
    };
    return {
        writes, props,
        render() { tree = h.render(() => ChannelForm(props)); return tree; },
        setPending(value) { pending = value; },
        selector: () => find(tree, (node) => node.type === 'Select' && find(node, (child) => child.props?.id === 'channel-test-endpoint').length > 0)[0],
        button: () => find(tree, (node) => node.type === 'Button' && text(node) === 'modelTest')[0],
        tree: () => tree,
    };
}
function savedChannel(overrides = {}) {
    return { ...draft(), id: 1, cloak: { mode: 'auto', profile_id: 1 }, ...overrides };
}
function dialogHarness(initial = savedChannel()) {
    const h = hooks(), writes = [];
    let tree, key;
    let respond = async () => ({ results: [{ success: true, duration_ms: 20 }] });
    const { ChannelTestDialog } = load('src/components/modules/channel/channel-test-dialog.tsx', {
        ...commonMocks, react: h.react,
        '@/api/endpoints/channel': { ChannelType },
        '@/api/endpoints/model': {
            useModelTest: () => ({ mutateAsync: async (request) => { writes.push(request); return respond(request); } }),
            useChannelTestIdentity: () => ({ data: { profile_name: 'fixture', codex: { user_agent: 'codex fixture' }, claude: { user_agent: 'claude fixture' }, generic: { user_agent: 'generic fixture' } } }),
        },
    });
    const props = { channel: initial, open: true, onOpenChange: noop };
    return {
        writes, props,
        respond(fn) { respond = fn; },
        render() {
            const child = ChannelTestDialog(props);
            if (child.key !== key) { h.reset(); key = child.key; }
            tree = h.render(() => child.type(child.props));
            return tree;
        },
        choice: (label) => find(tree, (node) => node.type === 'button' && text(node) === label)[0],
        run: () => find(tree, (node) => node.type === 'Button' && text(node).startsWith('测试选中'))[0],
        tree: () => tree,
    };
}

test('draft form exposes image choice, transmits selected endpoint/prompt/config only on click', () => {
    const h = formHarness(draft({ capabilities: ['embeddings'], openai_embeddings_path: '/custom/embeddings', openai_images_path: 'https://upstream.example/custom/images' }));
    h.render();
    assert.equal(h.selector().props.value, 'openai_chat');
    assert.ok(find(h.selector(), (node) => node.type === 'SelectItem' && text(node) === '图片生成').length);
    h.selector().props.onValueChange('openai_images_generations');
    h.render();
    assert.equal(h.selector().props.value, 'openai_images_generations');
    assert.match(text(h.tree()), /会真实生成1张图片并消耗额度/);
    assert.equal(h.writes.length, 0);
    h.button().props.onClick();
    const { request } = h.writes[0];
    assert.equal(request.endpoint, 'openai_images_generations');
    assert.equal(request.test_upstream_endpoint, true);
    assert.equal(request.prompt, 'a cute cat');
    assert.doesNotMatch(request.prompt, /算式|\+/);
    assert.equal(request.channel.openai_images_path, 'https://upstream.example/custom/images');
    assert.equal(request.channel.openai_embeddings_path, '/custom/embeddings');
    assert.deepEqual(request.channel.capabilities, ['embeddings']);
});

test('same-mounted form retains choice on ordinary edits but resets choice/results on type change', () => {
    const h = formHarness();
    h.render();
    h.selector().props.onValueChange('openai_images_generations');
    h.render();
    h.props.formData = { ...h.props.formData, name: 'renamed' };
    h.render();
    assert.equal(h.selector().props.value, 'openai_images_generations');
    h.button().props.onClick();
    h.writes[0].callbacks.onSuccess({ results: [{ success: true, response_preview: 'IMAGE_PREVIEW' }] });
    h.render();
    assert.match(text(h.tree()), /IMAGE_PREVIEW/);
    h.props.formData = { ...h.props.formData, type: 2 };
    h.render();
    assert.equal(h.selector().props.value, 'anthropic_messages');
    assert.doesNotMatch(text(h.tree()), /IMAGE_PREVIEW/);
    h.button().props.onClick();
    assert.equal(h.writes[1].request.endpoint, 'anthropic_messages');
    assert.match(h.writes[1].request.prompt, /算式/);
    h.setPending(true); h.render();
    assert.equal(h.selector().props.disabled, true);
    assert.ok(find(h.tree(), (node) => node.type === 'fieldset' && node.props.disabled).length);
});

test('custom image prefix field is editable and does not append generations in the frontend', () => {
    const h = formHarness(draft({ type: 6 }));
    h.render();
    const field = find(h.tree(), (node) => node.props?.id === 'channel-openai-images-path')[0];
    assert.equal(field.props.placeholder, 'https://upstream.example/v1/images');
    field.props.onChange({ target: { value: ' https://upstream.example/custom/images ' } });
    h.render();
    h.selector().props.onValueChange('openai_images_generations');
    h.render(); h.button().props.onClick();
    assert.equal(h.writes[0].request.channel.openai_images_path, 'https://upstream.example/custom/images');
});

test('dialog mixed tests send per-endpoint prompts and show sanitized image evidence', async () => {
    const h = dialogHarness(savedChannel({ type: 1 }));
    h.respond(async () => ({ results: [{ success: true, duration_ms: 20, upstream_path: 'https://upstream.example/v1/images/generations?token=private', response_preview: 'IMAGE_OK https://upstream.example/result.png token=private' }] }));
    h.render(); h.choice('图片生成').props.onClick(); h.render();
    assert.equal(h.writes.length, 0);
    await h.run().props.onClick();
    h.render();
    const image = h.writes.find((request) => request.endpoint === 'openai_images_generations');
    const response = h.writes.find((request) => request.endpoint === 'openai_responses');
    assert.equal(image.prompt, 'a cute cat');
    assert.equal(image.stream, false);
    assert.match(response.prompt, /算式/);
    assert.equal(response.stream, true);
    assert.ok(h.writes.every((request) => request.test_upstream_endpoint === true));
    assert.match(text(h.tree()), /实际路径：\/v1\/images\/generations/);
    assert.match(text(h.tree()), /IMAGE_OK/);
    assert.doesNotMatch(text(h.tree()), /upstream\.example|private/);
});

test('same-mounted dialog keeps reopen choice but resets on type and id changes; selections lock during run', async () => {
    const h = dialogHarness(savedChannel({ type: 1 }));
    h.render(); h.choice('Responses').props.onClick(); h.choice('图片生成').props.onClick(); h.render();
    await h.run().props.onClick(); h.render();
    assert.ok(find(h.tree(), (node) => node.type === 'TableRow').length > 0);
    h.props.open = false; h.render(); h.props.open = true; h.render();
    assert.equal(h.choice('图片生成').props['aria-pressed'], true);
    h.props.channel = { ...h.props.channel, type: 2 }; h.render();
    assert.equal(h.choice('Claude Messages').props['aria-pressed'], true);
    assert.equal(h.choice('图片生成').props['aria-pressed'], false);
    assert.equal(find(h.tree(), (node) => node.type === 'TableRow').length, 0);
    const running = h.run().props.onClick(); h.render();
    assert.equal(h.choice('Chat').props.disabled, true);
    assert.equal(h.choice('sample-model').props.disabled, true);
    await running;
    assert.equal(h.writes.at(-1).endpoint, 'anthropic_messages');
    h.props.channel = { ...h.props.channel, id: 2 }; h.render();
    assert.equal(find(h.tree(), (node) => node.type === 'TableRow').length, 0);
});

test('switching Responses channel to Claude uses Claude test identity and sends chosen endpoint', async () => {
    const h = dialogHarness(savedChannel({ type: 1 }));
    h.render(); h.choice('Responses').props.onClick(); h.choice('Claude Messages').props.onClick(); h.render();
    assert.match(text(h.tree()), /claude fixture/);
    assert.doesNotMatch(text(h.tree()), /codex fixture/);
    await h.run().props.onClick();
    assert.equal(h.writes[0].endpoint, 'anthropic_messages');
    assert.equal(h.writes[0].test_upstream_endpoint, true);
});
