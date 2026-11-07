'use strict';

// Run from web/: node --test tests/hlwy-mode.test.cjs

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');
const {
    baselineEntryFixture,
    createRuntime,
    findAll,
    findElement,
    snapshotFixture,
    squash,
    text,
    widgets,
} = require('./hlwy-harness.cjs');

/**
 * 对照模式的设置区（HlwyModePanel）。
 *
 * 三条硬要求在这里落地：
 *  1. 计划读不到就**没有**任何冒充价格的数字（不显示「约 N 次」）。
 *  2. 面板里没有第二个「开始」按钮——提交动作只留在页面底部。
 *  3. 挂载时不许发请求：进这个页面不等于开始花钱。
 */

function panel(overrides = {}) {
    const { catalog, catalogResponse, settingsData = [], settingsLoading = false, ...propsOverrides } = overrides;
    const runtime = createRuntime();
    const state = {
        catalog: catalog ?? { baselines: [baselineEntryFixture()] },
        settings: settingsData,
        settingsLoading,
        imported: [],
        writes: [],
        setOptions: [],
        writeFails: false,
        invalidated: [],
        toasts: [],
        refetches: 0,
    };

    const mocks = {
        react: runtime.react,
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/common/Toast': {
            toast: {
                success: (...args) => state.toasts.push(['success', ...args]),
                error: (...args) => state.toasts.push(['error', ...args]),
            },
        },
        '@/lib/utils': { cn: (...args) => args.filter(Boolean).join(' ') },
        '@/lib/audit-budget': {
            settingBool: (raw, fallback) => (raw === 'true' ? true : raw === 'false' ? false : fallback),
        },
        // 计划缓存靠这个客户端刷新：开关一改，'hlwy-plan' 那批查询必须作废，
        // 否则服务端已经 enabled=true，页面上还挂着上一次的 enabled=false。
        '@tanstack/react-query': {
            useQueryClient: () => ({
                invalidateQueries: (arg) => state.invalidated.push(arg),
            }),
        },
        '@/api/endpoints/setting': {
            useSettingList: () => ({ data: state.settings, isLoading: state.settingsLoading }),
            useSetSetting: () => ({
                mutate: (payload, options) => {
                    state.writes.push(payload);
                    state.setOptions.push(options);
                    if (state.writeFails) {
                        options?.onError?.(new Error('boom'));
                        return;
                    }
                    options?.onSuccess?.();
                },
                isPending: false,
            }),
        },
        '@/api/endpoints/model-audit': {
            useHlwyBaselines: () =>
                catalogResponse
                    ? catalogResponse(state)
                    : {
                          data: state.catalog,
                          isLoading: false,
                          error: null,
                          refetch: () => {
                              state.refetches += 1;
                          },
                      },
            useImportHlwyBaseline: () => ({
                mutate: (payload, options) => state.imported.push({ payload, options }),
                isPending: false,
            }),
        },
    };

    const { HlwyModePanel } = load('src/components/modules/model-audit/HlwyModePanel.tsx', mocks);
    const calls = { baseline: [], samples: [] };
    const props = {
        baselineId: 'hlwy:base@aaa111',
        onBaselineChange: (id) => calls.baseline.push(id),
        samples: 100,
        onSamplesChange: (n) => calls.samples.push(n),
        plan: null,
        planLoading: false,
        planError: '',
        ...propsOverrides,
    };
    const tree = runtime.render(HlwyModePanel, props);
    return { tree, state, calls, runtime, props, render: () => runtime.render(HlwyModePanel, props) };
}

const PLAN = {
    kind: 'hlwy_compare',
    enabled: true,
    samples: 100,
    max_calls: 200,
    timeout_seconds: 1300,
    sample_timeout_seconds: 60,
    protocol_id: 'hlwy-number355-v1',
    baseline: baselineEntryFixture(),
    conditions_note: '题目与参数按基准冻结',
};

test('a missing plan shows no invented call count', () => {
    const { tree } = panel({ plan: null, planLoading: false });
    const body = squash(tree);
    assert.match(body, /选好参考后，会显示调用次数和预计时限/);
    assert.equal(body.includes('最多调用'), false, 'no plan means no price promise, not a guessed number');
    assert.equal(/\d+ 次真实调用/.test(body), false);
});

test('a plan that failed to load says so and still shows no count', () => {
    const { tree } = panel({ plan: null, planError: '渠道不存在' });
    const body = squash(tree);
    assert.match(body, /暂时无法读取采样安排：渠道不存在/);
    assert.equal(body.includes('最多调用'), false);
});

test('a loaded plan shows the cap, both timeouts and the protocol', () => {
    const { tree } = panel({ plan: PLAN });
    const body = squash(tree);
    assert.equal(body.includes('会消耗渠道额度'), false, 'the single cost warning belongs beside submit');
    const settings = findElement(tree, (node) => node.type === 'details' && text(node).includes('采样设置'));
    assert.match(squash(settings), /单次超时：60 秒/);
    assert.match(squash(settings), /整轮超时：1300 秒/);
    assert.match(squash(settings), /本次题目协议：hlwy-number355-v1/);
    assert.match(squash(settings), /题目与参数按基准冻结/);
});

test('the panel never owns a submit button', () => {
    const { tree } = panel({ plan: PLAN });
    assert.equal(findAll(tree, (node) => text(node).includes('开始采样')).length, 0);
    assert.equal(findAll(tree, (node) => text(node).includes('开始检测')).length, 0);
});

test('sample count buttons are a two-way choice and start at 100', () => {
    const { tree, calls } = panel({ plan: PLAN, samples: 100 });
    const buttons = findAll(tree, (node) => node.type === 'button');
    assert.equal(buttons.length, 2);
    assert.equal(buttons[0].props['aria-pressed'], true);
    assert.equal(buttons[1].props['aria-pressed'], false);
    assert.match(text(buttons[1]), /200 次/);
    buttons[1].props.onClick();
    assert.deepEqual(calls.samples, [200]);
});

test('baseline metadata is on the page, not behind a hover', () => {
    const { tree } = panel({ plan: PLAN });
    const body = squash(tree);
    assert.match(body, /来源：hlwy 社区基准/);
    assert.match(body, /采集日期：2026-01-02/);
    assert.match(body, /有效样本：200 个/);
    assert.match(body, /声明模型：some-model/);
    assert.match(body, /接口协议：hlwy-number355-v1/);
    // 接口协议和题目协议分开写：把两者合成一项会让等题版本被读成请求协议。
    assert.match(body, /参考题目协议：未知/);
    const select = findElement(tree, (node) => node.type === 'SearchableSelect');
    assert.equal(select.props.options[0].label, '社区基准 A');
    assert.match(select.props.options[0].keywords, /2026-01-02/);
    assert.match(select.props.options[0].keywords, /200/);
    assert.equal(select.props.className, 'w-full min-w-0 max-w-full');
});

test('a baseline without a collection date says 日期未知', () => {
    const runtime = createRuntime();
    const mocks = {
        react: runtime.react,
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/common/Toast': { toast: { success: () => {}, error: () => {} } },
        '@/lib/utils': { cn: (...args) => args.filter(Boolean).join(' ') },
        '@/lib/audit-budget': { settingBool: (raw, fallback) => (raw === 'true' ? true : raw === 'false' ? false : fallback) },
        '@tanstack/react-query': { useQueryClient: () => ({ invalidateQueries: () => {} }) },
        '@/api/endpoints/setting': { useSettingList: () => ({ data: [] }), useSetSetting: () => ({ mutate: () => {} }) },
        '@/api/endpoints/model-audit': {
            useHlwyBaselines: () => ({
                data: { baselines: [baselineEntryFixture({ source_kind: 'imported_hlwy', collected_at: null })] },
            }),
            useImportHlwyBaseline: () => ({ mutate: () => {}, isPending: false }),
        },
    };
    const { HlwyModePanel } = load('src/components/modules/model-audit/HlwyModePanel.tsx', mocks);
    const tree = runtime.render(HlwyModePanel, {
        baselineId: 'hlwy:base@aaa111',
        onBaselineChange: () => {},
        samples: 100,
        onSamplesChange: () => {},
        plan: null,
        planLoading: false,
        planError: '',
    });
    const body = squash(tree);
    assert.match(body, /来源：导入的旧文件/);
    assert.match(body, /采集日期：日期未知/);
});

test('a corrupt local baseline is greyed out with a reason and does not block the healthy ones', () => {
    const broken = baselineEntryFixture({
        id: 'hlwy:bad@zzz999',
        label: '坏掉的本地件',
        source_kind: 'local_capture',
        unavailable: true,
        reason_code: 'counts_mismatch',
    });
    const { tree } = panel({ catalog: { baselines: [baselineEntryFixture(), broken] } });
    const options = findElement(tree, (node) => node.type === 'SearchableSelect').props.options;
    const brokenOption = options.find((option) => option.value === 'hlwy:bad@zzz999');
    assert.equal(brokenOption.disabled, true, 'a corrupt entry cannot be picked');
    assert.equal(brokenOption.label, '坏掉的本地件');
    assert.equal(
        options.find((option) => option.value === 'hlwy:base@aaa111').disabled,
        false,
        'the healthy builtin stays pickable: one bad local file must not take the catalog down'
    );

    const body = squash(tree);
    assert.match(body, /1 份可用（另 1 份暂不可用）/);
    assert.match(body, /校验没通过/, 'the reason is on the page, not only in a tooltip');
});

test('selecting a baseline that turned out unusable says why instead of looking fine', () => {
    const broken = baselineEntryFixture({
        id: 'hlwy:bad@zzz999',
        label: '坏掉的本地件',
        source_kind: 'local_capture',
        unavailable: true,
        reason_code: 'counts_mismatch',
    });
    const { tree } = panel({ catalog: { baselines: [baselineEntryFixture(), broken] }, baselineId: 'hlwy:bad@zzz999' });
    const body = squash(tree);
    assert.match(body, /选中的「坏掉的本地件」现在不能用：计数和原始数字对不上/);
    assert.match(body, /换一份基准再提交/);
});

test('the submit switch is off by default and writes the literal setting key', () => {
    const { tree, state } = panel({ plan: PLAN });
    const body = squash(tree);
    assert.match(body, /已关闭提交/, 'default off: this mode costs money');
    assert.match(body, /历史记录、导入旧基准、换基准重算照常可用/);

    const toggle = findElement(tree, (node) => node.type === 'Switch');
    assert.equal(toggle.props.checked, false);
    toggle.props.onCheckedChange(true);
    assert.deepEqual(state.writes, [{ key: 'model_audit_hlwy_enabled', value: 'true' }]);
});

test('flipping the switch drops the cached plan, so the button is not left greyed out', () => {
    // 实测过的坑：开关关着时读到的是 enabled=false 的计划，打开开关以后只刷新了
    // settings，计划缓存还是那一份，按钮就一直按不动——直到切焦点或者键变了才重读。
    const { tree, state } = panel({ plan: PLAN });
    assert.deepEqual(state.invalidated, [], 'nothing is invalidated before the switch is touched');

    findElement(tree, (node) => node.type === 'Switch').props.onCheckedChange(true);
    assert.deepEqual(state.invalidated, [{ queryKey: ['model-audit', 'hlwy-plan'] }]);
    assert.deepEqual(
        state.toasts.map((entry) => entry[0]),
        ['success'],
        'the existing confirmation is still shown'
    );
});

test('a failed write leaves the cached plan alone', () => {
    // 写失败时按钮的状态没变，此时刷计划只会白跑一次请求、还会把「为什么按不动」
    // 的现场擦掉。
    const { tree, state } = panel({ plan: PLAN });
    state.writeFails = true;

    findElement(tree, (node) => node.type === 'Switch').props.onCheckedChange(true);
    assert.deepEqual(state.invalidated, [], 'no invalidation on error');
    assert.deepEqual(state.toasts.map((entry) => entry[0]), ['error']);
});

test('an enabled flag is read from the same key', () => {
    const runtime = createRuntime();
    const mocks = {
        react: runtime.react,
        '@tanstack/react-query': { useQueryClient: () => ({ invalidateQueries: () => {} }) },
        '@/components/ui/button': widgets('Button'),
        '@/components/ui/switch': widgets('Switch'),
        '@/components/ui/searchable-select': widgets('SearchableSelect'),
        '@/components/common/Toast': { toast: { success: () => {}, error: () => {} } },
        '@/lib/utils': { cn: (...args) => args.filter(Boolean).join(' ') },
        '@/lib/audit-budget': { settingBool: (raw, fallback) => (raw === 'true' ? true : raw === 'false' ? false : fallback) },
        '@/api/endpoints/setting': {
            useSettingList: () => ({ data: [{ key: 'model_audit_hlwy_enabled', value: 'true' }] }),
            useSetSetting: () => ({ mutate: () => {} }),
        },
        '@/api/endpoints/model-audit': {
            useHlwyBaselines: () => ({ data: { baselines: [] } }),
            useImportHlwyBaseline: () => ({ mutate: () => {}, isPending: false }),
        },
    };
    const { HlwyModePanel } = load('src/components/modules/model-audit/HlwyModePanel.tsx', mocks);
    const tree = runtime.render(HlwyModePanel, {
        baselineId: '',
        onBaselineChange: () => {},
        samples: 200,
        onSamplesChange: () => {},
        plan: null,
        planLoading: false,
        planError: '',
    });
    const body = squash(tree);
    assert.match(body, /允许提交/);
    assert.equal(body.includes('提交开关关着'), false);
    assert.equal(findElement(tree, (node) => node.type === 'Switch').props.checked, true);
    // 没有基准时仍然把「还没选」写出来，不拿第一份顶替。
    assert.match(body, /来源：还没选/);
});

test('mounting the panel submits nothing', () => {
    const { state, tree } = panel({ plan: PLAN });
    assert.deepEqual(state.imported, [], 'no import fired on mount');
    assert.deepEqual(state.writes, [], 'no setting written on mount');
    assert.deepEqual(state.toasts, []);
    // 导入入口在，但只在用户真的选了文件之后才动。
    const input = findElement(tree, (node) => node.type === 'input');
    assert.equal(input.props.type, 'file');
    assert.match(input.props.accept, /json/);
});

test('an oversized file is refused locally, before any request', async () => {
    const { state, tree } = panel({ plan: PLAN });
    const input = findElement(tree, (node) => node.type === 'input');
    const big = { size: 2 * 1024 * 1024, text: async () => '{}' };
    await input.props.onChange({ target: { files: [big], value: 'x' } });
    assert.deepEqual(state.imported, []);
});

test('a catalog that fails to load says why and offers a retry', () => {
    const { tree, state } = panel({
        catalogResponse: (st) => ({
            data: undefined,
            isLoading: false,
            error: new Error('目录服务 502'),
            refetch: () => {
                st.refetches += 1;
            },
        }),
    });
    const body = squash(tree);
    // 读不到目录就说读不到；留一个空选择器会让人以为没有基准。
    assert.match(body, /基准目录没读出来：目录服务 502/);
    assert.equal(body.includes('0 份可选'), false);
    const retry = findElement(tree, (node) => node.type === 'button' && text(node) === '重新读取目录');
    retry.props.onClick();
    assert.equal(state.refetches, 1);
});

test('the submit switch cannot be flipped while the setting is unknown', () => {
    const unknown = panel({ plan: PLAN, settingsData: null });
    const sw = findElement(unknown.tree, (node) => node.type === 'Switch');
    assert.equal(sw.props.disabled, true, 'the write needs the key to be readable first');
    assert.match(squash(unknown.tree), /没读到设置，开关先不能用/);

    const loading = panel({ plan: PLAN, settingsLoading: true });
    assert.equal(findElement(loading.tree, (node) => node.type === 'Switch').props.disabled, true);
    assert.match(squash(loading.tree), /正在读取设置…/);
});

test('import refuses anything that is not a JSON object, before any request', async () => {
    const arrayCase = panel({ plan: PLAN });
    await findElement(arrayCase.tree, (node) => node.type === 'input').props.onChange({
        target: { files: [{ size: 2, text: async () => '[]' }], value: 'x' },
    });
    assert.deepEqual(arrayCase.state.imported, []);
    assert.match(squash(arrayCase.render()), /文件内容不是一个 JSON 对象/);

    const brokenJson = panel({ plan: PLAN });
    await findElement(brokenJson.tree, (node) => node.type === 'input').props.onChange({
        target: { files: [{ size: 9, text: async () => '{ not json' }], value: 'x' },
    });
    assert.deepEqual(brokenJson.state.imported, []);
    assert.match(squash(brokenJson.render()), /这不是有效的 JSON 文件/);
});

test('the server’s own words are repeated verbatim when an import fails', async () => {
    const { tree, state, render } = panel({ plan: PLAN });
    const input = findElement(tree, (node) => node.type === 'input');
    await input.props.onChange({
        target: { files: [{ size: 12, text: async () => '{"rawData":[1]}' }], value: 'x' },
    });
    assert.equal(state.imported.length, 1, 'a JSON object is handed to the endpoint');
    state.imported[0].options.onError(new Error('distribution 之和不是 1（差 1e-3）'));
    assert.match(squash(render()), /distribution 之和不是 1（差 1e-3）/);
});
