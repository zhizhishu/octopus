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

const { defaultModelTestEndpointForChannel, shouldForceChannelTestStream, channelTestIdentityFor } =
    load('src/lib/channel-test.ts', {
        '@/api/endpoints/channel': { ChannelType },
        './model-aliases': { isStreamRequiredModel: () => false },
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

/**
 * 火山渠道选 responses 端点，**不等于**它是 codex 形状。
 *
 * 后端判据是渠道类型不是端点名：
 *   runner.go:763       强制流式只匹配 OutboundTypeOpenAIResponse
 *   runner.go:1156-1161 指纹 switch 里 Volcengine 落 default → false
 *   header_defaults.go  Volcengine 走通用 UA
 *
 * 前端只看端点会导致：火山渠道的流式开关被锁死（测不了非流式），身份还标成
 * "codex CLI" —— 而实际出站是通用 UA。界面在撒谎，且藏掉了一种可测模式。
 */
test('火山渠道选 responses 端点不锁流式', () => {
    // 后端 runner.go:763 只对 OutboundTypeOpenAIResponse 强制流式。
    // 火山是 OutboundTypeVolcengine，不强制 —— 它的非流式模式可测。
    assert.equal(
        shouldForceChannelTestStream({ endpoint: 'openai_responses', channelType: ChannelType.Volcengine }),
        false,
    );
});

test('只有 Responses 渠道类型锁流式', () => {
    assert.equal(
        shouldForceChannelTestStream({ endpoint: 'openai_responses', channelType: ChannelType.OpenAIResponse }),
        true,
    );
    // 普通 Chat 渠道选 responses 端点**不**锁流式：后端也不给它强制流式。
    // cloak.shape=codex 同理（shape 只换身份头，不改流式）。
    assert.equal(
        shouldForceChannelTestStream({ endpoint: 'openai_responses', channelType: ChannelType.OpenAIChat }),
        false,
    );
    assert.equal(
        shouldForceChannelTestStream({
            endpoint: 'openai_responses',
            channelType: ChannelType.OpenAIChat,
            cloakShape: 'codex',
        }),
        false,
    );
});

test('Anthropic 渠道按 cloak 决定是否锁流式', () => {
    assert.equal(
        shouldForceChannelTestStream({ endpoint: 'anthropic_messages', channelType: ChannelType.Anthropic, cloakMode: 'auto' }),
        true,
    );
    assert.equal(
        shouldForceChannelTestStream({ endpoint: 'anthropic_messages', channelType: ChannelType.Anthropic, cloakMode: 'never' }),
        false,
    );
});

test('渠道类型未知时保持旧行为，不误伤', () => {
    assert.equal(shouldForceChannelTestStream({ endpoint: 'openai_responses' }), true);
});

/**
 * 身份显示必须镜像后端 header_defaults.go:78-247，而不是"端点名对应身份"。
 */
test('火山渠道的身份是通用 UA，不是 codex CLI', () => {
    // 火山出站是 /responses 形状，但身份走 header_defaults 的 default 分支 = 通用 UA。
    assert.equal(channelTestIdentityFor('openai_responses', ChannelType.Volcengine), 'generic');
});

test('协议本身就是 CLI 的渠道，身份不可选', () => {
    assert.equal(channelTestIdentityFor('openai_responses', ChannelType.OpenAIResponse), 'codex');
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.Anthropic), 'claude');
});

test('Chat 渠道的身份由 cloak.shape 显式 opt-in', () => {
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.OpenAIChat), 'generic');
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.OpenAIChat, { shape: 'codex' }), 'codex');
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.OpenAIChat, { shape: 'claude' }), 'claude');
    // shape 大小写/空白要能容忍（后端用 ToLower+TrimSpace）。
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.OpenAIChat, { shape: '  Codex ' }), 'codex');
});

test('cloak 关掉时强制通用身份，优先于一切', () => {
    // header_defaults.go:81-90：cloak 关了会剥掉 codex 头并强制通用 UA。
    assert.equal(channelTestIdentityFor('openai_responses', ChannelType.OpenAIResponse, { mode: 'never' }), 'generic');
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.OpenAIChat, { mode: 'never', shape: 'codex' }), 'generic');
    // auto / always 仍按正常判据。
    assert.equal(channelTestIdentityFor('openai_responses', ChannelType.OpenAIResponse, { mode: 'auto' }), 'codex');
});
