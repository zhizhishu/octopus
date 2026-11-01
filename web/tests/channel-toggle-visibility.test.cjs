'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');

const { channelToggleVisibility, channelCapabilityOnCount } = load('src/lib/channel-capabilities.ts');

// 类型编号：0 OpenAIChat · 1 Responses · 2 Anthropic · 3 Gemini · 5 Embedding · 6 Custom · 7 SystemOne
const CASES = {
    0: { context1m: false, thinkingToContent: true, embeddings: true, embeddingsAnthropicNote: false, embeddingsPathOnly: false },
    1: { context1m: false, thinkingToContent: true, embeddings: true, embeddingsAnthropicNote: false, embeddingsPathOnly: false },
    2: { context1m: true, thinkingToContent: true, embeddings: true, embeddingsAnthropicNote: true, embeddingsPathOnly: false },
    3: { context1m: false, thinkingToContent: true, embeddings: true, embeddingsAnthropicNote: false, embeddingsPathOnly: false },
    5: { context1m: false, thinkingToContent: false, embeddings: false, embeddingsAnthropicNote: false, embeddingsPathOnly: true },
    6: { context1m: false, thinkingToContent: true, embeddings: true, embeddingsAnthropicNote: false, embeddingsPathOnly: false },
    7: { context1m: false, thinkingToContent: false, embeddings: false, embeddingsAnthropicNote: false, embeddingsPathOnly: false },
};

for (const [type, want] of Object.entries(CASES)) {
    test(`渠道类型 ${type} 的能力开关显示规则`, () => {
        assert.deepStrictEqual({ ...channelToggleVisibility(Number(type)) }, want);
    });
}

test('手机端「已开启 N 项」只数当前类型看得见的开关', () => {
    const allOn = { anthropic_context_1m: true, thinking_to_content: true, capabilities: ['embeddings'] };
    assert.equal(channelCapabilityOnCount({ type: 2, ...allOn }), 3);
    assert.equal(channelCapabilityOnCount({ type: 0, ...allOn }), 2); // 1M 在非 Anthropic 上看不见，不算
    assert.equal(channelCapabilityOnCount({ type: 5, ...allOn }), 0); // 嵌入渠道：两个开关都隐藏
    assert.equal(channelCapabilityOnCount({ type: 7, ...allOn }), 0);
    assert.equal(channelCapabilityOnCount({ type: 2 }), 0);
    assert.equal(channelCapabilityOnCount({ type: 0, thinking_to_content: true, capabilities: ['images'] }), 1);
});
