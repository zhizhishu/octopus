'use strict';

// Run from web: node --test tests/model-audit-v6.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { readFileSync } = require('node:fs');
const path = require('node:path');
const { setImmediate } = require('node:timers/promises');
const { load } = require('./load-typescript.cjs');
const { createRuntime, findAll, findElement, squash, text, widgets, baselineEntryFixture } = require('./hlwy-harness.cjs');

/**
 * 检测页 v6：三栏（检测 / 任务 / 鹈鹕测试）。
 *
 * 这里守四件事：
 *  1. 估次数按每个模型自己的计划加，Claude 多出来的签名不被平均掉；
 *  2. 任务卡片的小方块只用后端给的进度，读不到写「等待」；
 *  3. 鹈鹕图放在 sandbox="allow-scripts"（不带 allow-same-origin）+ CSP 的隔离框里；
 *  4. 记录、鹈鹕图都能删，删之前都先问一句。
 */

const V6 = 'src/components/modules/model-audit/v6.ts';
const v6 = () => load(V6, {});

const planWith = (rows) => ({
    protocol: 'anthropic-messages',
    model: 'm',
    probes: rows.map(([id, requests, applicable = true]) => ({ id, requests, applicable })),
    quick: { ids: [], requests: 0 },
    full: { ids: [], requests: 0 },
});

test('request estimate follows each model plan and skips probes that do not apply', () => {
    const { requestsFor, totalRequests } = v6();
    const claude = planWith([['liveness', 1], ['signature', 2], ['fingerprint', 3]]);
    const other = planWith([['liveness', 1], ['signature', 0, false], ['fingerprint', 3]]);
    assert.equal(requestsFor(claude, ['liveness', 'signature', 'fingerprint']), 6);
    assert.equal(requestsFor(other, ['liveness', 'signature', 'fingerprint']), 4, 'a probe that does not apply costs nothing');
    assert.equal(requestsFor(undefined, ['liveness', 'echo_rewrite']), 2, 'no plan yet: one per check, never a made-up bigger number');
    const plans = { a: claude, b: other };
    assert.equal(
        totalRequests(['a', 'b'], (m) => plans[m], () => ['liveness', 'signature', 'fingerprint']),
        10,
        'two models are summed with their own plans, not first × count',
    );
});

test('models are grouped by the checks they will actually run', () => {
    const { groupModelsByProbes } = v6();
    const groups = groupModelsByProbes(['claude-a', 'gpt-b', 'claude-c', 'none'], (m) =>
        m === 'none' ? [] : m.startsWith('claude') ? ['liveness', 'signature'] : ['liveness'],
    );
    assert.deepEqual(groups, [
        { models: ['claude-a', 'claude-c'], probes: ['liveness', 'signature'] },
        { models: ['gpt-b'], probes: ['liveness'] },
    ]);
});

test('auto baseline: own capture of the same model, then hlwy builtin of the same model, then any builtin', () => {
    const { autoBaselineFor } = v6();
    const builtinOther = baselineEntryFixture({ id: 'b-other', source_kind: 'builtin_hlwy', declared_model: 'deepseek' });
    const builtinSame = baselineEntryFixture({ id: 'b-same', source_kind: 'builtin_hlwy', declared_model: 'GPT-X' });
    const localSame = baselineEntryFixture({ id: 'l-same', source_kind: 'local_capture', declared_model: 'gpt-x' });
    const localBroken = baselineEntryFixture({ id: 'l-bad', source_kind: 'local_capture', declared_model: 'gpt-x', unavailable: true });
    assert.equal(autoBaselineFor('gpt-x', [builtinOther, builtinSame, localSame]).id, 'l-same');
    assert.equal(autoBaselineFor('gpt-x', [builtinOther, builtinSame, localBroken]).id, 'b-same', 'a broken capture is never picked');
    assert.equal(autoBaselineFor('gpt-x', [localBroken, builtinOther]).id, 'b-other');
    assert.equal(autoBaselineFor('gpt-x', [localBroken]), null);
});

test('task blocks come from live progress, hlwy ledger, or say 等待', () => {
    const { taskBlocks, queueOrder } = v6();
    const live = taskBlocks({
        id: 1, status: 'running', kind: 'audit', probes: 'liveness,fingerprint,tokenizer_id',
        live: {
            result_id: 9, model: 'm', sent: 5, planned: 16, started_at: '',
            probes: [
                { id: 'liveness', status: 'done', sent: 1, planned: 1 },
                { id: 'fingerprint', status: 'running', sent: 2, planned: 3 },
                { id: 'tokenizer_id', status: 'waiting', sent: 0, planned: 12 },
            ],
        },
    });
    assert.deepEqual(live.map((b) => [b.label, b.state, b.detail]), [
        ['连通', 'done', '✓ 1/1'],
        ['取数指纹', 'running', '进行中 2/3'],
        ['分词器', 'waiting', '等待'],
    ]);
    const hlwy = taskBlocks({ id: 2, status: 'running', kind: 'hlwy_compare', hlwy_summary: { counts: { planned_calls: 100, started_slots: 60, valid: 58 } } });
    assert.deepEqual(hlwy.map((b) => [b.label, b.state, b.detail]), [['数字习惯', 'running', '采样 60 / 100']]);
    const queued = taskBlocks({ id: 3, status: 'queued', kind: 'audit', probes: 'liveness,echo_rewrite' });
    assert.ok(queued.every((b) => b.state === 'waiting' && b.detail === '等待'), 'nothing is invented for a queued job');

    const order = queueOrder([
        { id: 5, status: 'queued' }, { id: 3, status: 'running' }, { id: 4, status: 'queued' }, { id: 2, status: 'done' },
    ]);
    assert.deepEqual(order.map((o) => [o.job.id, o.position]), [[3, 0], [4, 1], [5, 2]]);
});

test('creative frames get a no-network CSP and the pelican prompt is the original one', () => {
    const { creativeSrcDoc, PELICAN_PROMPT, CREATIVE_FRAME_CSP, creativeFailureText } = v6();
    assert.equal(PELICAN_PROMPT, 'Generate an SVG of a pelican riding a bicycle');
    const svg = creativeSrcDoc({ format: 'svg', content: '<svg><circle r="1"/></svg>' });
    assert.ok(svg.includes(CREATIVE_FRAME_CSP) && svg.includes('<svg><circle r="1"/></svg>'));
    assert.match(CREATIVE_FRAME_CSP, /default-src 'none'/);
    const page = creativeSrcDoc({ format: 'html', content: '<!doctype html><html><head><title>x</title></head><body>hi</body></html>' });
    assert.ok(page.indexOf('Content-Security-Policy') < page.indexOf('<title>'), 'the CSP goes first inside <head>');
    assert.equal(creativeFailureText({ status: 'invalid', kind: 'pelican', error: '' }), '没返回合法 SVG');
    assert.equal(creativeFailureText({ status: 'timeout', kind: 'pelican', error: '超时（180 秒没出完）' }), '超时（180 秒没出完）');
});

test('the creative iframe never gets allow-same-origin', () => {
    const src = readFileSync(path.join(__dirname, '..', 'src/components/modules/model-audit/PelicanTest.tsx'), 'utf8');
    assert.match(src, /sandbox="allow-scripts"/);
    assert.equal(/sandbox="[^"]*allow-same-origin/.test(src), false, 'scripts in the frame must not reach our origin');
    assert.match(src, /每个模型只发 1 次 · 在隔离框里显示 · /);
    assert.match(src, /7 天后自动删除，收藏的保留/);
});

/* ------------------------------------------------------------------ 鹈鹕测试页 */

function pelicanPage(options = {}) {
    const runtime = createRuntime();
    const state = { created: [], pinned: [], deleted: [], cleared: 0, toasts: [] };
    const toast = new Proxy({}, { get: (_, kind) => (...a) => state.toasts.push([kind, ...a]) });
    const items = options.items ?? [];
    const { PelicanTest } = load('src/components/modules/model-audit/PelicanTest.tsx', {
        react: runtime.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogHeader DialogTitle'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/common/Toast': { toast },
        '@/lib/utils': { cn: (...p) => p.filter(Boolean).join(' ') },
        '@/components/modules/channel/channel-utils': { getSelectedChannelModels: (ch) => ch.selected_models ?? [] },
        './MultiPicker': widgets('MultiPicker'),
        '@/api/endpoints/model-audit': {
            useCreativeRounds: () => ({ data: options.rounds ?? [] }),
            useCreativeRound: () => ({ data: items, isLoading: false }),
            useAuditBudget: () => ({ data: options.budget ?? { used: 0, limit: 200, remaining: 200 } }),
            useCreateCreativeRound: () => ({ isPending: false, mutate: (vars, cb) => { state.created.push(vars); cb?.onSuccess?.({ round_id: 'r1', ids: [1] }); } }),
            usePinCreative: () => ({ isPending: false, mutate: (vars) => state.pinned.push(vars) }),
            useDeleteCreative: () => ({ isPending: false, mutate: (id) => state.deleted.push(id) }),
            useClearUnpinnedCreatives: () => ({ isPending: false, mutate: () => { state.cleared += 1; } }),
        },
    });
    const channels = options.channels ?? [
        { id: 3, name: 'mistral', enabled: true, type: 0, selected_models: ['deepseek-v4.1-flash', 'zai-glm-5-3'] },
        { id: 4, name: 'emb', enabled: true, type: 5, selected_models: ['text-embedding'] },
    ];
    return { state, render: () => runtime.render(PelicanTest, { channels }) };
}

const creative = (over) => ({
    id: 1, round_id: 'r1', channel_id: 3, channel_name: 'mistral', model: 'deepseek-v4.1-flash', kind: 'pelican',
    prompt: 'Generate an SVG of a pelican riding a bicycle', format: 'svg', content: '<svg></svg>', raw: '```svg\n<svg></svg>\n```',
    status: 'done', error: '', duration_ms: 18000, input_tokens: 20, output_tokens: 1900, pinned: false,
    created_at: new Date().toISOString(), ...over,
});

test('pelican: picks only chat models, costs one request each, and sends the parsed targets', () => {
    const p = pelicanPage();
    let tree = p.render();
    const picker = findElement(tree, (n) => n.type === 'MultiPicker');
    assert.deepEqual(picker.props.options.map((o) => o.value), ['3::deepseek-v4.1-flash', '3::zai-glm-5-3'], 'embedding channels are not offered');
    const start = () => findElement(p.render(), (n) => n.type === 'Button' && squash(n).startsWith('一起出图'));
    assert.equal(start().props.disabled, true, 'nothing picked, nothing to send');
    picker.props.onChange(['3::deepseek-v4.1-flash', '3::zai-glm-5-3']);
    tree = p.render();
    assert.match(squash(start()), /一起出图 · 2 次请求/);
    start().props.onClick();
    assert.deepEqual(p.state.created, [{
        kind: 'pelican',
        prompt: undefined,
        targets: [{ channel_id: 3, model: 'deepseek-v4.1-flash' }, { channel_id: 3, model: 'zai-glm-5-3' }],
    }]);
});

test('pelican: a round that does not fit today budget is not sent', () => {
    const p = pelicanPage({ budget: { used: 199, limit: 200, remaining: 1 } });
    const tree = p.render();
    findElement(tree, (n) => n.type === 'MultiPicker').props.onChange(['3::deepseek-v4.1-flash', '3::zai-glm-5-3']);
    const body = squash(p.render());
    assert.match(body, /今天的检测额度只剩 1 次，不够这一轮的 2 次/);
    assert.equal(findElement(p.render(), (n) => n.type === 'Button' && squash(n).startsWith('一起出图')).props.disabled, true);
});

test('pelican: cards render in a sandboxed frame, failures show why, and deletes ask first', () => {
    const p = pelicanPage({
        rounds: [{ round_id: 'r1', kind: 'pelican', prompt: 'x', created_at: new Date().toISOString(), total: 3, active: 0, pinned: 0 }],
        items: [
            creative({}),
            creative({ id: 2, model: 'gemini', status: 'invalid', format: '', content: '', raw: 'I cannot draw.' }),
            creative({ id: 3, model: 'slow', status: 'timeout', format: '', content: '', raw: '', error: '超时（180 秒没出完）' }),
        ],
    });
    let tree = p.render();
    const frames = findAll(tree, (n) => n.type === 'iframe');
    assert.equal(frames.length, 1, 'only the good one is drawn');
    assert.equal(frames[0].props.sandbox, 'allow-scripts');
    assert.match(frames[0].props.srcDoc, /Content-Security-Policy/);
    const body = squash(tree);
    assert.match(body, /没返回合法 SVG/);
    assert.match(body, /看原文/);
    assert.match(body, /超时（180 秒没出完）/);
    assert.match(body, /18 秒 · 1,920 token/);

    findElement(tree, (n) => n.type === 'button' && squash(n).endsWith('收藏')).props.onClick();
    assert.deepEqual(p.state.pinned, [{ id: 1, pinned: true }]);

    findAll(tree, (n) => n.type === 'button' && n.props['aria-label'] === '删掉这张')[0].props.onClick();
    assert.deepEqual(p.state.deleted, [], 'the first click only asks');
    tree = p.render();
    findElement(tree, (n) => n.type === 'Button' && squash(n) === '删除').props.onClick();
    assert.deepEqual(p.state.deleted, [1]);

    findElement(tree, (n) => n.type === 'Button' && squash(n) === '清空未收藏').props.onClick();
    tree = p.render();
    assert.match(squash(tree), /收藏的和还在出的不动/);
    assert.equal(p.state.cleared, 0);
    findElement(tree, (n) => n.type === 'Button' && squash(n) === '确定清空').props.onClick();
    assert.equal(p.state.cleared, 1);
});

/* ------------------------------------------------------------------ 任务页 */

test('tasks: running card shows request ledger and blocks, queued ones say where they stand', () => {
    const runtime = createRuntime();
    const cancelled = [];
    const jobs = [
        {
            id: 11, channel_name: 'mistral', status: 'running', total: 1, finished: 0, probes: 'liveness,fingerprint', trigger: 'manual',
            preset: '自定义', created_at: new Date().toISOString(), planned_requests: 4, sent_requests: 2,
            live: { result_id: 1, model: 'zai-glm-5-3', sent: 2, planned: 4, started_at: '', probes: [
                { id: 'liveness', status: 'done', sent: 1, planned: 1 },
                { id: 'fingerprint', status: 'running', sent: 1, planned: 3 },
            ] },
        },
        { id: 12, channel_name: 'workbuddy', status: 'queued', total: 1, finished: 0, probes: 'liveness', trigger: 'manual', created_at: new Date().toISOString() },
        { id: 10, channel_name: 'old', status: 'done', total: 1, finished: 1, probes: 'liveness', trigger: 'schedule', created_at: new Date().toISOString() },
    ];
    const { TasksView } = load('src/components/modules/model-audit/TasksView.tsx', {
        react: runtime.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        '@/components/ui/button': widgets('Button'),
        '@/lib/utils': { cn: (...p) => p.filter(Boolean).join(' ') },
        '@/api/endpoints/model-audit': {
            useAuditJobs: () => ({ data: jobs }),
            useCancelAuditJob: () => ({ isPending: false, variables: undefined, mutate: (id) => cancelled.push(id) }),
        },
        './Queue': { ModelAuditQueue: (props) => ({ type: 'Records', props: { hideActive: props.hideActive, children: null } }) },
    });
    const tree = runtime.render(TasksView, {});
    const body = squash(tree);
    assert.match(body, /进行中 · 2/);
    assert.match(body, /mistral · zai-glm-5-3/);
    assert.match(body, /2 \/ 4 次/);
    assert.match(body, /连通✓ 1\/1/);
    assert.match(body, /取数指纹进行中 1\/3/);
    assert.match(body, /排第 1，前面的跑完就开始/);
    assert.equal(body.includes('old'), false, 'finished jobs live in the records table, not as cards');
    assert.equal(findElement(tree, (n) => n.type === 'Records').props.hideActive, true);
    findAll(tree, (n) => n.type === 'Button' && squash(n) === '取消')[1].props.onClick();
    assert.deepEqual(cancelled, [12], 'cancel targets the job on that card');
});

/* ------------------------------------------------------------------ 检测页 */

test('test tab: multi-model submit splits Claude and non-Claude, shows totals and budget', async () => {
    const runtime = createRuntime();
    const enqueued = [];
    const toasts = [];
    const plan = (model) => ({
        protocol: 'anthropic-messages', model, quick: { ids: [], requests: 0 }, full: { ids: [], requests: 0 },
        probes: [['liveness', 1], ['identity', 1], ['glitch', 1], ['token_delta', 2], ['echo_rewrite', 1], ['context_canary', 1], ['signature', model.startsWith('claude') ? 2 : 0, model.startsWith('claude')]]
            .map(([id, requests, applicable = true]) => ({ id, requests, applicable })),
    });
    const { ModelAudit } = load('src/components/modules/model-audit/index.tsx', {
        react: runtime.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        'next-intl': { useTranslations: () => (k) => k },
        '@/api/endpoints/channel': {
            ChannelType: { Chat: 0, Anthropic: 2 },
            useChannelList: () => ({ data: [{ raw: { id: 2, name: 'gemini_claude', enabled: true, type: 2, selected_models: ['claude-opus-5-5', 'grok-4.7'] } }], isLoading: false }),
        },
        '@/api/endpoints/model-audit': {
            AUDIT_PROBE_IDS: ['liveness', 'identity', 'glitch', 'token_delta', 'echo_rewrite', 'context_canary', 'signature', 'tokenizer_id', 'tokenizer_baseline'],
            AUDIT_OPT_IN_PROBE_IDS: ['tokenizer_id', 'tokenizer_baseline'],
            AUDIT_STANDARD_PROBE_IDS: ['liveness', 'identity', 'glitch', 'token_delta', 'echo_rewrite', 'context_canary', 'signature'],
            useLogAnomalies: () => ({ data: undefined }),
            useSaveTokenizerBaseline: () => ({ isPending: false, mutateAsync: () => Promise.resolve({}) }),
            useEnqueueAudit: () => ({ isPending: false, mutateAsync: (vars) => { enqueued.push(vars); return Promise.resolve({ job_id: enqueued.length }); } }),
            useAuditPlans: (channelId, models) => models.map((m) => ({ data: plan(m) })),
            useAuditBudget: () => ({ data: { used: 37, limit: 200, remaining: 163 } }),
            useAuditJobs: () => ({ data: [{ id: 1, status: 'running' }, { id: 2, status: 'queued' }, { id: 3, status: 'done' }] }),
            useHlwyBaselines: () => ({ data: { baselines: [] }, isLoading: false }),
            useHlwyPlan: () => ({ data: undefined, error: undefined, isLoading: false }),
            useEnqueueHlwyCompare: () => ({ isPending: false, mutateAsync: () => Promise.resolve({ job_id: 9 }) }),
            useRecompareHlwy: () => ({ isPending: false, mutate: () => {} }),
            useSaveHlwyBaselineFromResult: () => ({ isPending: false, mutate: () => {} }),
        },
        '@/api/endpoints/setting': { useSettingList: () => ({ data: [] }), useSetSetting: () => ({ mutate: () => {}, isPending: false }), SettingKey: {} },
        '@/components/common/PageWrapper': widgets('PageWrapper'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogHeader DialogTitle'),
        '@/components/common/Toast': { toast: new Proxy({}, { get: (_, kind) => (...a) => toasts.push([kind, ...a]) }) },
        '@/lib/utils': { cn: (...p) => p.filter(Boolean).join(' ') },
        '@/lib/audit-highlights': { highlightRows: () => [], provenanceView: () => null, TONE_DOT_CLASS: {} },
        '@/lib/audit-budget': { settingBool: (v, f) => (v === 'true' ? true : v === 'false' ? false : f) },
        '@/components/modules/channel/channel-utils': { getSelectedChannelModels: (ch) => ch?.selected_models ?? [] },
        '@/lib/channel-test': { sanitizeChannelTestError: (v) => v },
        './FingerprintCard': { FingerprintCard: () => null },
        './ProvenanceCard': { ProvenanceCard: () => null },
        './BackgroundAuditPanel': { BackgroundAuditPanel: () => 'BackgroundAuditPanel' },
        './BaselineComparisonCard': { BaselineComparisonCard: () => null },
        './HlwyModePanel': { HlwyModePanel: () => null },
        './TasksView': { TasksView: () => 'TasksView' },
        './PelicanTest': { PelicanTest: () => 'PelicanTest' },
        './MultiPicker': widgets('MultiPicker'),
    });
    let tree = runtime.render(ModelAudit, {});
    let body = squash(tree);
    assert.match(body, /任务2 个在跑/, 'the tasks tab carries the running count');
    assert.match(body, /鹈鹕测试/);
    assert.match(body, /今天检测额度还剩 163 \/ 200 次/);
    assert.match(body, /放到后台跑，关掉页面也继续/);
    assert.match(body, /定时体检/);
    assert.match(body, /日常监测/);

    findElement(tree, (n) => n.type === 'MultiPicker').props.onChange(['claude-opus-5-5', 'grok-4.7']);
    tree = runtime.render(ModelAudit, {});
    body = squash(tree);
    // 全面：claude 7 项 9 次，grok 签名不适用 6 项 7 次 → 合计 16 次。
    assert.match(body, /开始检测 · 2 个模型 · 约 16 次/);
    findElement(tree, (n) => n.type === 'Button' && squash(n).startsWith('开始检测')).props.onClick();
    await setImmediate();
    assert.equal(enqueued.length, 2, 'Claude and non-Claude go in as two jobs');
    assert.deepEqual(enqueued[0].models, ['claude-opus-5-5']);
    assert.ok(enqueued[0].probes.includes('signature'));
    assert.deepEqual(enqueued[1].models, ['grok-4.7']);
    assert.equal(enqueued[1].probes.includes('signature'), false);
    assert.ok(toasts.some(([kind]) => kind === 'success'));

    // 三栏切换真的换内容。
    findElement(tree, (n) => n.type === 'button' && squash(n) === '鹈鹕测试').props.onClick();
    assert.match(squash(runtime.render(ModelAudit, {})), /PelicanTest/);
    findElement(tree, (n) => n.type === 'button' && squash(n).startsWith('任务')).props.onClick();
    assert.match(squash(runtime.render(ModelAudit, {})), /TasksView/);
    // 没有探针也会去写页面上的字：确认打开「任务」后检测页的提交按钮不在。
    assert.equal(findElement(runtime.render(ModelAudit, {}), (n) => n.type === 'Button' && squash(n).startsWith('开始检测')), undefined);
    assert.ok(text(tree).length > 0);
});
