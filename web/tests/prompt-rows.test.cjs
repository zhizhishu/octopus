const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { routePromptRows, planRouteRows, matchesPromptSearch, sortChannelsStable } = load('src/components/modules/prompt/prompt-rows.ts');

const target = (request_model, channel_id, upstream_model, prompt = '', mode, promptEnabled) => ({
    request_model, channel_id, upstream_model, priority: 1, weight: 1, enabled: true,
    system_prompt_override: prompt, prompt_override_mode: mode, prompt_override_enabled: promptEnabled,
});

test('模型映射提示词一条规则一行，计数按规则算', () => {
    const plans = [
        {
            id: 1, slug: 'vip', display_name: 'VIP',
            route_targets: [
                target('gpt-4o', 11, 'gpt-4o', '只用中文', 'before', true),
                target('gpt-4o', 12, 'gpt-4o-2024', '只用中文', 'before', true),
                target('gpt-4o', 13, 'gpt-4o', '只用中文', 'before', true),
                target('claude', 11, 'claude-sonnet', ''),
                target('gemini', 15, 'gemini-pro', '补一句', 'after', false),
            ],
        },
        {
            id: 2, slug: 'svip', display_name: 'SVIP',
            route_targets: [
                target('gpt-4o', 11, 'gpt-4o', '简短', 'replace_system'),
                target('deepseek', 14, 'deepseek-chat', '  '),
            ],
        },
    ];
    const rows = routePromptRows(plans);
    assert.equal(rows.length, 3, '3 个目标同一规则 + gemini 1 条 + 另一方案 1 条 = 3 行');
    assert.deepEqual(rows.map((r) => [r.plan.id, r.requestModel, r.targetCount, r.upstreamModels, r.mode, r.enabled]), [
        [1, 'gpt-4o', 3, ['gpt-4o', 'gpt-4o-2024'], 'before', true],
        // 关着的也列出来（标「已关闭」），文字保留
        [1, 'gemini', 1, ['gemini-pro'], 'after', false],
        // 旧的 replace_system、没有开关字段：有文字算「开 + 前面」（后端同样按它处理）
        [2, 'gpt-4o', 1, ['gpt-4o'], 'before', true],
    ]);
});

test('没有 route_targets 或都没写提示词时为空', () => {
    assert.deepEqual(routePromptRows([{ id: 1, route_targets: [] }, { id: 2 }]), []);
});

test('渠道按 id 升序，与接口返回顺序无关，且不改原数组', () => {
    const a = [{ id: 3 }, { id: 1 }, { id: 2 }];
    const b = [{ id: 2 }, { id: 3 }, { id: 1 }];
    assert.deepEqual(sortChannelsStable(a).map((c) => c.id), [1, 2, 3]);
    assert.deepEqual(sortChannelsStable(b).map((c) => c.id), [1, 2, 3]);
    assert.deepEqual(a.map((c) => c.id), [3, 1, 2]);
});

test('planRouteRows：一个方案的所有路由都列出来（没写提示词的也在，方便在提示词页补）', () => {
    const plan = {
        id: 1, slug: 'vip', route_targets: [
            target('gpt-4o', 11, 'gpt-4o', '只用中文', 'after', true),
            target('GPT-4o', 12, 'gpt-4o-2024', '只用中文', 'after', true),
            target('claude', 11, 'claude-sonnet', ''),
            target('', 11, 'x', ''),
        ],
    };
    const rows = planRouteRows(plan);
    assert.deepEqual(rows.map((r) => [r.requestModel, r.targetCount, r.upstreamModels, r.mode, r.enabled, r.text]), [
        ['gpt-4o', 2, ['gpt-4o', 'gpt-4o-2024'], 'after', true, '只用中文'],
        ['claude', 1, ['claude-sonnet'], 'before', false, ''],
    ]);
});

test('matchesPromptSearch：每个词都要命中某个字段，不分大小写', () => {
    assert.equal(matchesPromptSearch('', 'a'), true);
    assert.equal(matchesPromptSearch('VIP gpt', 'vip', 'gpt-4o'), true);
    assert.equal(matchesPromptSearch('vip claude', 'vip', 'gpt-4o'), false);
    assert.equal(matchesPromptSearch('中文', null, undefined, '只用中文回答'), true);
    assert.equal(matchesPromptSearch('#3', 'Codex', '#3'), true);
});
