'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { load } = require('./load-typescript.cjs');

// load-typescript.cjs 只解析相对导入，`@/` 别名要在这里喂替身。枚举值与
// src/api/endpoints/channel.ts:13-21 的真实定义逐一对齐（改那边必须同步改这里）。
const ChannelType = {
    OpenAIChat: 0,
    OpenAIResponse: 1,
    Anthropic: 2,
    Gemini: 3,
    Volcengine: 4,
    OpenAIEmbedding: 5,
    CustomOpenAIChat: 6,
};

const { defaultModelTestEndpointForChannel } = load('src/lib/channel-test.ts', {
    '@/api/endpoints/channel': { ChannelType },
});

/**
 * 渠道测试的默认端点必须对上该渠道的**真实出站协议**。
 *
 * 事故面：火山渠道的出站固定是 /responses（outbound/volcengine/response.go:107
 * `parsedUrl.Path = parsedUrl.Path + "/responses"`），它根本不发
 * /v1/chat/completions。早先它落进 default 分支按 openai_chat 测——测的是这条
 * 渠道永远不会走的路径，测出来的绿是假的，一条已经坏掉的火山渠道也会显示通过。
 *
 * 这类"测了一条不存在的路"的假绿比红更危险：它把问题藏起来。
 */

test('火山渠道按 responses 测，不按 chat 测', () => {
    assert.equal(defaultModelTestEndpointForChannel(ChannelType.Volcengine), 'openai_responses');
});

test('各渠道类型对上的端点与实际出站一致', () => {
    const cases = [
        [ChannelType.OpenAIChat, 'openai_chat'],
        [ChannelType.OpenAIResponse, 'openai_responses'],
        [ChannelType.Anthropic, 'anthropic_messages'],
        [ChannelType.Gemini, 'gemini_generate_content'],
        [ChannelType.Volcengine, 'openai_responses'],
        [ChannelType.OpenAIEmbedding, 'openai_embeddings'],
        [ChannelType.CustomOpenAIChat, 'openai_chat'],
    ];
    for (const [type, want] of cases) {
        assert.equal(defaultModelTestEndpointForChannel(type), want, `channel type ${type}`);
    }
});
