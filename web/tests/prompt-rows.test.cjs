const test = require('node:test');
const assert = require('node:assert/strict');

const { load } = require('./load-typescript.cjs');

const { routePromptRows, sortChannelsStable } = load('src/components/modules/prompt/prompt-rows.ts');

const target = (request_model, channel_id, upstream_model, prompt = '', mode) => ({
    request_model, channel_id, upstream_model, priority: 1, weight: 1, enabled: true,
    system_prompt_override: prompt, prompt_override_mode: mode,
});

test('模型映射提示词一条规则一行，计数按规则算', () => {
    const plans = [
        {
            id: 1, slug: 'vip', display_name: 'VIP',
            route_targets: [
                target('gpt-4o', 11, 'gpt-4o', '只用中文', 'append_system'),
                target('gpt-4o', 12, 'gpt-4o-2024', '只用中文', 'append_system'),
                target('gpt-4o', 13, 'gpt-4o', '只用中文', 'append_system'),
                target('claude', 11, 'claude-sonnet', ''),
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
    assert.equal(rows.length, 2, '3 个目标同一规则 + 另一方案 1 条 = 2 行');
    assert.deepEqual(rows.map((r) => [r.plan.id, r.requestModel, r.targetCount, r.upstreamModels, r.mode]), [
        [1, 'gpt-4o', 3, ['gpt-4o', 'gpt-4o-2024'], 'append_system'],
        [2, 'gpt-4o', 1, ['gpt-4o'], 'replace_system'],
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
