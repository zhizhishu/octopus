'use strict';

// Run from web: node --test tests/hlwy-page.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { setImmediate } = require('node:timers/promises');
const { load } = require('./load-typescript.cjs');
const {
    comparisonFixture,
    countsFixture,
    createRuntime,
    findAll,
    findElement,
    matchFixture,
    reportFixture,
    snapshotFixture,
    squash,
    text,
    baselineEntryFixture,
    translate,
    widgets,
} = require('./hlwy-harness.cjs');

/**
 * 检测页上的「基准对照」模式。
 *
 * 这一页只允许存在一个提交动作：切模式换的是按钮的含义（disabled/onClick/文案），
 * 不是再挂一个按钮出来。所以下面每条断言都在数"屏幕上有没有第二个提交入口"，
 * 以及"没读到计划时它敢不敢让用户按"。
 */

const PLAN_OK = {
    kind: 'hlwy_compare',
    enabled: true,
    samples: 100,
    max_calls: 100,
    timeout_seconds: 1300,
    sample_timeout_seconds: 60,
    protocol_id: 'hlwy-number355-v1',
    baseline: { id: 'hlwy:base@abc123456789', label: '社区基准', source_kind: 'builtin_hlwy' },
    conditions_note: '',
};

function page(options = {}) {
    const runtime = createRuntime();
    const state = {
        channels: options.channels ?? [{ raw: { id: 7, name: 'c', enabled: true, type: 2, selected_models: ['gpt-test'] } }],
        settings: options.settings ?? [],
        plan: options.plan ?? { data: PLAN_OK, error: undefined, isLoading: false },
        enqueued: [],
        auditEnqueued: [],
        planCalls: [],
        panelProps: [],
        cardProps: [],
        queueProps: [],
        toasts: [],
    };
    const toast = {
        success: (...a) => state.toasts.push(['success', ...a]),
        error: (...a) => state.toasts.push(['error', ...a]),
        warning: (...a) => state.toasts.push(['warning', ...a]),
        info: (...a) => state.toasts.push(['info', ...a]),
    };
    const { ModelAudit } = load('src/components/modules/model-audit/index.tsx', {
        react: runtime.react,
        'lucide-react': new Proxy({}, { get: (_, key) => key }),
        'next-intl': { useTranslations: () => translate },
        '@/api/endpoints/channel': {
            ChannelType: { Chat: 0, Anthropic: 2 },
            useChannelList: () => ({ data: state.channels, isLoading: false }),
        },
        '@/api/endpoints/model-audit': {
            AUDIT_PROBE_IDS: ['liveness', 'identity', 'glitch', 'token_delta', 'echo_rewrite', 'context_canary', 'signature'],
            AUDIT_OPT_IN_PROBE_IDS: ['tokenizer_id', 'tokenizer_baseline'],
            AUDIT_STANDARD_PROBE_IDS: ['liveness', 'identity', 'glitch', 'token_delta', 'echo_rewrite', 'context_canary', 'signature'],
            useLogAnomalies: () => ({ data: undefined }),
            useSaveTokenizerBaseline: () => ({ isPending: false, mutateAsync: () => Promise.resolve({}) }),
            useRunModelAudit: () => ({ mutateAsync: () => Promise.resolve({}) }),
            useEnqueueAudit: () => ({
                isPending: false,
                mutateAsync: (vars) => {
                    state.auditEnqueued.push(vars);
                    return Promise.resolve({ job_id: 1 });
                },
            }),
            useAuditPlan: () => ({
                data: {
                    protocol: 'openai-chat',
                    model: 'gpt-test',
                    probes: [],
                    quick: { ids: ['liveness'], requests: 3 },
                    full: { ids: ['liveness', 'identity'], requests: 10 },
                },
            }),
            useAuditPlans: (channelId, models) => models.map((model) => ({
                data: {
                    protocol: 'openai-chat',
                    model,
                    probes: [],
                    quick: { ids: ['liveness'], requests: 3 },
                    full: { ids: ['liveness', 'identity'], requests: 10 },
                },
            })),
            useAuditBudget: () => ({ data: { used: 37, limit: 200, remaining: 163 } }),
            useAuditJobs: () => ({ data: [] }),
            useScheduledAudit: () => ({ data: undefined }),
            // hlwy 侧：每个 hook 都给能观测的替身，不发请求。
            useHlwyBaselines: () => ({ data: { baselines: options.baselines ?? [] }, isLoading: false }),
            useHlwyPlan: (input) => {
                state.planCalls.push(input);
                return { data: state.plan.data, error: state.plan.error, isLoading: state.plan.isLoading === true };
            },
            useEnqueueHlwyCompare: () => ({
                isPending: false,
                mutateAsync: (vars) => {
                    state.enqueued.push(vars);
                    return Promise.resolve({ job_id: 9 });
                },
            }),
            useRecompareHlwy: () => ({ isPending: false, mutate: () => {} }),
            useSaveHlwyBaselineFromResult: () => ({ isPending: false, mutate: () => {} }),
        },
        '@/api/endpoints/setting': {
            useSettingList: () => ({ data: state.settings }),
            useSetSetting: () => ({ mutate: () => {}, isPending: false }),
            SettingKey: { LogPassiveProbeEnabled: 'log_passive_probe_enabled' },
        },
        '@/components/common/PageWrapper': widgets('PageWrapper'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/dialog': widgets('Dialog DialogContent DialogDescription DialogHeader DialogTitle'),
        '@/components/common/Toast': { toast },
        '@/lib/utils': { cn: (...parts) => parts.filter(Boolean).join(' ') },
        '@/lib/audit-highlights': { highlightRows: () => [], provenanceView: () => null, TONE_DOT_CLASS: {} },
        '@/lib/audit-budget': {
            settingBool: (value, fallback) => (value === 'true' ? true : value === 'false' ? false : fallback),
        },
        // 生产代码先 map 成裸渠道（index.tsx 的 channels = channelsData.map(item => item.raw)），
        // 再把裸渠道交给 getSelectedChannelModels。替身必须按同一个入参读，否则
        // activeModel 恒为空，所有「有计划就该能提交」的断言都会假失败。
        '@/components/modules/channel/channel-utils': { getSelectedChannelModels: (channel) => channel?.selected_models ?? [] },
        '@/lib/channel-test': { sanitizeChannelTestError: (value) => value },
        './FingerprintCard': { FingerprintCard: () => null },
        './ProvenanceCard': { ProvenanceCard: () => null },
        './BackgroundAuditPanel': { BackgroundAuditPanel: () => null },
        // 「任务」页自己的渲染由 model-audit-queue.test.cjs / v6 测试覆盖；这里只要拿到
        // 它收到的 onOpenReport，好走「从记录打开报告」这条路。
        './TasksView': {
            TasksView: (props) => {
                state.queueProps.push(props);
                return 'TasksView';
            },
        },
        './PelicanTest': { PelicanTest: () => 'PelicanTest' },
        './MultiPicker': widgets('MultiPicker'),
        './BaselineComparisonCard': {
            BaselineComparisonCard: (props) => {
                state.cardProps.push(props);
                return 'BaselineComparisonCard';
            },
        },
        // 面板自己的渲染由 hlwy-mode.test.cjs 覆盖；这里只要拿到它收到的 props，
        // 好从"用户选了基准"这一步往下走。
        './HlwyModePanel': {
            HlwyModePanel: (props) => {
                state.panelProps.push(props);
                return 'HlwyModePanel';
            },
        },
    });
    return {
        state,
        toast,
        render() {
            return runtime.render(ModelAudit, {});
        },
        /**
         * 打开数字习惯：「查多细」选自定义 → 勾「数字习惯」→ 点「换」展开基准面板。
         * 每一步都只改本地状态，所以每步之后重新 render 拿新树。页面上有手机/桌面两份，点任意一份即可。
         */
        chooseHlwy(tree) {
            findElement(tree, (node) => node.type === 'button' && text(node).includes('自定义') && node.props['aria-pressed'] !== undefined).props.onClick();
            tree = this.render();
            findElement(tree, (node) => node.type === 'input' && node.props['aria-label'] === '数字习惯').props.onChange({ target: { checked: true } });
            tree = this.render();
            findElement(tree, (node) => node.type === 'button' && squash(node) === '换 ▾').props.onClick();
            return this.render();
        },
        /** 自定义里除「数字习惯」以外的勾全部取消：只交数字习惯这一种任务。 */
        onlyHlwy(tree) {
            for (const node of findAll(tree, (n) => n.type === 'input' && n.props.type === 'checkbox' && n.props['aria-label'] !== '数字习惯')) {
                node.props.onChange({ target: { checked: false } });
            }
            return this.render();
        },
        panel() {
            return state.panelProps[state.panelProps.length - 1];
        },
        card() {
            return state.cardProps[state.cardProps.length - 1];
        },
        queue() {
            return state.queueProps[state.queueProps.length - 1];
        },
    };
}

// 页面上唯一的提交按钮：「开始检测 · N 个模型 · 约 X 次」。
const submitButton = (tree) =>
    findElement(tree, (node) => node.type === 'Button' && squash(node).startsWith('开始检测'));

function submitButtons(tree) {
    return findAll(tree, (node) => node.type === 'Button' && squash(node).startsWith('开始检测'));
}

test('mounting the page never submits and never reads a comparison plan', () => {
    const p = page();
    const tree = p.render();
    assert.equal(p.state.enqueued.length, 0);
    assert.equal(p.state.auditEnqueued.length, 0);
    assert.equal(p.state.toasts.length, 0, 'no toast, no silent action');
    assert.equal(p.state.planCalls.at(-1).enabled, false, 'no plan request while not in comparison mode');
});

test('turning on 数字习惯 keeps the one submit button and only then reads a plan', () => {
    const p = page();
    let tree = p.render();
    const before = submitButtons(tree).length;
    assert.ok(before >= 1, 'there is a submit action');
    assert.equal(p.state.planCalls.at(-1).enabled, false, 'no plan request before the box is ticked');

    tree = p.chooseHlwy(tree);
    assert.equal(submitButtons(tree).length, before, 'no second submit button appears');
    assert.equal(findAll(tree, (node) => squash(node) === '开始采样').length, 0, 'the old separate sampling button is gone');
    assert.equal(p.state.planCalls.at(-1).enabled, true, 'the plan is read only after ticking 数字习惯');
    assert.equal(p.state.enqueued.length, 0, 'ticking submits nothing');
    assert.equal(p.state.auditEnqueued.length, 0);
});

test('without a readable plan the button stays off and says why', () => {
    const p = page({ plan: { data: undefined, error: new Error('boom'), isLoading: false } });
    let tree = p.render();
    tree = p.chooseHlwy(tree);

    const button = submitButton(tree);
    assert.equal(button.props.disabled, true, 'no plan, no promise, no submit');
    const body = squash(tree);
    assert.match(body, /计划没读出来：boom/);
    // 没读到计划就没有调用上限可写：不拿旧数字冒充「最多调用 N 次」。
    assert.match(body, /数字习惯的调用上限要等计划读出来才知道/);
    assert.equal(/最多调用\d+次/.test(body), false, 'no invented call cap is shown');
});

test('a loaded plan shows its call cap and enables the button', () => {
    const p = page({
        plan: { data: PLAN_OK, error: undefined, isLoading: false },
        settings: [{ key: 'model_audit_hlwy_enabled', value: 'true' }],
    });
    let tree = p.render();
    tree = p.chooseHlwy(tree);
    p.panel().onBaselineChange('hlwy:base@abc123456789');
    tree = p.render();

    const button = submitButton(tree);
    assert.equal(button.props.disabled, false);
    assert.match(squash(tree), /最多调用100次，会消耗渠道额度/);
});

test('the comparison submission carries only the comparison fields', async () => {
    const p = page({
        plan: { data: PLAN_OK, error: undefined, isLoading: false },
        settings: [{ key: 'model_audit_hlwy_enabled', value: 'true' }],
    });
    let tree = p.render();
    tree = p.chooseHlwy(tree);
    tree = p.onlyHlwy(tree);

    // 没有能用的基准（目录是空的、也没手动选）就不能提交：这不是能靠默认值猜的东西。
    assert.equal(submitButton(tree).props.disabled, true);
    assert.match(squash(tree), /先选一份基准/);

    p.panel().onBaselineChange('hlwy:base@abc123456789');
    tree = p.render();
    p.panel().onSamplesChange(200);
    tree = p.render();

    submitButton(tree).props.onClick();
    await setImmediate();

    assert.equal(p.state.enqueued.length, 1, 'one click = one submission');
    assert.equal(p.state.auditEnqueued.length, 0, 'nothing else was ticked, so no probe job');
    const payload = p.state.enqueued[0];
    assert.deepEqual(Object.keys(payload).sort(), ['channel_id', 'hlwy', 'kind', 'models']);
    assert.equal(payload.channel_id, 7);
    assert.equal(payload.kind, 'hlwy_compare');
    assert.deepEqual(payload.models, ['gpt-test'], 'one channel, one model per round');
    assert.deepEqual(Object.keys(payload.hlwy).sort(), ['baseline_id', 'protocol_id', 'samples']);
    assert.equal(payload.hlwy.samples, 200);
    assert.equal(payload.hlwy.baseline_id, 'hlwy:base@abc123456789');
    assert.equal(payload.hlwy.protocol_id, 'hlwy-number355-v1');
    assert.equal('probes' in payload, false, 'a comparison is not a probe run');
    assert.equal('log_id' in payload.hlwy, false, 'and it is not a log backfill either');
    assert.equal(p.state.toasts.length, 1);
    assert.equal(p.state.toasts[0][0], 'success');
});

test('数字习惯 with other checks ticked submits two kinds of job, each with its own fields', async () => {
    const p = page({
        plan: { data: PLAN_OK, error: undefined, isLoading: false },
        settings: [{ key: 'model_audit_hlwy_enabled', value: 'true' }],
        baselines: [baselineEntryFixture({ id: 'hlwy:auto@1', source_kind: 'builtin_hlwy', declared_model: 'gpt-test' })],
    });
    let tree = p.render();
    tree = p.chooseHlwy(tree);
    // 自动挑基准：同模型的内置件，不用手动选。
    assert.equal(p.panel().baselineId, 'hlwy:auto@1');
    submitButton(tree).props.onClick();
    await setImmediate();
    assert.equal(p.state.auditEnqueued.length, 1, 'the ticked probes go in as one probe job');
    assert.equal('hlwy' in p.state.auditEnqueued[0], false);
    assert.equal(p.state.enqueued.length, 1, 'and 数字习惯 as its own comparison job');
    assert.equal(p.state.enqueued[0].hlwy.baseline_id, 'hlwy:auto@1');
});

test('the feature flag off blocks only new submissions, and says so', () => {
    const p = page({ plan: { data: PLAN_OK, error: undefined, isLoading: false } });
    let tree = p.render();
    tree = p.chooseHlwy(tree);
    p.panel().onBaselineChange('hlwy:base@abc123456789');
    tree = p.render();

    const button = submitButton(tree);
    assert.equal(button.props.disabled, true);
    assert.match(squash(tree), /基准对照已在设置里关闭/);
    assert.equal(p.state.enqueued.length, 0);
});

test('the plan request follows the chosen baseline and sample count', () => {
    const p = page({ plan: { data: PLAN_OK, error: undefined, isLoading: false } });
    p.chooseHlwy(p.render());
    p.panel().onBaselineChange('hlwy:base@abc123456789');
    p.render();
    p.panel().onSamplesChange(200);
    p.render();

    const last = p.state.planCalls.at(-1);
    assert.equal(last.channelId, 7);
    assert.equal(last.model, 'gpt-test');
    assert.equal(last.samples, 200);
    assert.equal(last.baselineId, 'hlwy:base@abc123456789');
    assert.equal(last.enabled, true);
});

test('a picked baseline that the server now rejects blocks submission', () => {
    // 选的时候目录里还是好的，等计划回来时这份件已被标成校验没过：
    // 这时按钮要关掉并说明原因，而不是让服务端去拒一次。
    const p = page({
        plan: { data: PLAN_OK, error: undefined, isLoading: false },
        settings: [{ key: 'model_audit_hlwy_enabled', value: 'true' }],
        baselines: [
            baselineEntryFixture({
                id: 'hlwy:base@abc123456789',
                unavailable: true,
                reason_code: 'hash_mismatch',
            }),
        ],
    });
    let tree = p.render();
    tree = p.chooseHlwy(tree);
    p.panel().onBaselineChange('hlwy:base@abc123456789');
    tree = p.render();

    assert.equal(submitButton(tree).props.disabled, true);
    assert.match(squash(tree), /选中的基准现在不能用/);
    assert.equal(p.state.enqueued.length, 0);
});

test('rerunning refills the form from the stored report and enqueues nothing', () => {
    const stored = reportFixture({
        baseline_comparison: comparisonFixture({
            counts: countsFixture({ planned_calls: 200, started_slots: 200, valid: 200 }),
            baseline_snapshots: [snapshotFixture()],
            matches: [matchFixture()],
            chosen_baseline_id: 'hlwy:base@aaa111',
        }),
    });
    const p = page({
        plan: { data: PLAN_OK, error: undefined, isLoading: false },
        settings: [{ key: 'model_audit_hlwy_enabled', value: 'true' }],
        channels: [
            { raw: { id: 7, name: 'c', enabled: true, type: 2, selected_models: ['gpt-test'] } },
            { raw: { id: 9, name: 'ch-9', enabled: true, type: 2, selected_models: ['stored-model'] } },
        ],
    });
    let tree = p.render();
    // 报告从「记录」里打开：队列只交出一份 JSON 字符串，页面现解析。
    findElement(tree, (node) => node.type === 'button' && text(node).trim() === '任务').props.onClick();
    tree = p.render();
    p.queue().onOpenReport({
        id: 41,
        channel_id: 9,
        channel_name: 'ch-9',
        model: 'stored-model',
        upstream_model: 'stored-model',
        status: 'done',
        duration_ms: 1000,
        finished_at: '2026-02-03T00:00:00Z',
        report: JSON.stringify(stored),
    });
    tree = p.render();
    // 报告卡是替身，判「报告打开了」要看它收到的 view，而不是看页面上的字。
    assert.equal(p.card()?.view?.chosen?.percent, '86.50%', 'the stored report is on screen');
    // 卡片还知道自己属于哪一条结果：换一份报告，卡片里临时选的基准不能跟过来。
    assert.equal(p.card()?.reportKey, 41, 'the card is tied to this result, not to "whatever is open"');

    const cardsBefore = p.state.cardProps.length;
    p.card().onRerun();
    tree = p.render();

    // 渠道、型号、基准、样本量都跟着回来：只填基准会让下一轮跑在错的渠道上。
    const selectValues = findAll(tree, (node) => node.type === 'SearchableSelect').map((node) => node.props.value);
    assert.ok(selectValues.includes('9'), 'the channel from the report is restored');
    const pickedModels = findAll(tree, (node) => node.type === 'MultiPicker').map((node) => node.props.value);
    assert.ok(pickedModels.some((value) => value.length === 1 && value[0] === 'stored-model'), 'so is the model, and only that one');
    assert.equal(p.panel().baselineId, 'hlwy:base@aaa111');
    assert.equal(p.panel().samples, 200);
    // 回到提交页，但一轮都没发：再跑一轮要花真钱，得用户自己按。
    assert.equal(p.state.enqueued.length, 0);
    assert.equal(p.state.auditEnqueued.length, 0);
    assert.ok(
        p.state.toasts.some((entry) => String(entry[1]).includes('参数已按上一轮填好')),
        'the user is told the form was refilled, not that a run started'
    );
    // 报告收起来了：旧的结论不该继续挂在"这一次"下面。
    assert.equal(p.state.cardProps.length, cardsBefore, 'the report card is gone after rerun');
});

test('a rerun whose original channel is gone is blocked, not quietly moved elsewhere', () => {
    const stored = reportFixture({
        baseline_comparison: comparisonFixture({
            counts: countsFixture({ planned_calls: 100, started_slots: 100, valid: 100 }),
            baseline_snapshots: [snapshotFixture()],
            matches: [matchFixture()],
            chosen_baseline_id: 'hlwy:base@aaa111',
        }),
    });
    const p = page({
        settings: [{ key: 'model_audit_hlwy_enabled', value: 'true' }],
        // 报告来自渠道 #9，但那个渠道已经被删掉了，列表里只剩 #7。
        channels: [{ raw: { id: 7, name: 'ch-7', enabled: true, type: 2, selected_models: ['gpt-test'] } }],
    });
    let tree = p.render();
    findElement(tree, (node) => node.type === 'button' && text(node).trim() === '任务').props.onClick();
    tree = p.render();
    p.queue().onOpenReport({
        id: 42,
        channel_id: 9,
        channel_name: 'ch-9',
        model: 'stored-model',
        upstream_model: 'stored-model',
        status: 'done',
        duration_ms: 1000,
        finished_at: '2026-02-03T00:00:00Z',
        report: JSON.stringify(stored),
    });
    tree = p.render();
    p.card().onRerun();
    tree = p.render();

    // 渠道选择器只能落到现存渠道上，所以光看选择器「有值」发现不了这件事。
    assert.equal(p.panel().baselineId, 'hlwy:base@aaa111', 'the rest of the form is still refilled');
    assert.equal(submitButton(tree).props.disabled, true, 'do not spend money on a channel nobody chose');
    assert.match(squash(tree), /上一轮用的渠道（#9）已经不在渠道列表里了，请重新选一个渠道再提交/);
    assert.equal(p.state.enqueued.length, 0);

    // 用户自己重新选了渠道，这条拦阻才解除。
    findAll(tree, (node) => node.type === 'SearchableSelect').find((node) => node.props.value === '7')
        .props.onValueChange('7');
    tree = p.render();
    assert.equal(submitButton(tree).props.disabled, false, 'an explicitly chosen channel unblocks it');
    assert.equal(squash(tree).includes('已经不在渠道列表里了'), false);
});
