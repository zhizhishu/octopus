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
    // 4 = 退役的 Volcengine（火山那条线已砍掉），编号永久空着。
    OpenAIEmbedding: 5,
    CustomOpenAIChat: 6,
    SystemOne: 7,
};

const { defaultModelTestEndpointForChannel, shouldForceChannelTestStream, channelTestIdentityFor } =
    load('src/lib/channel-test.ts', {
        '@/api/endpoints/channel': { ChannelType },
        // 用真实语义的迷你替身：gpt-5.5 / [1m] / context-1m 命中。
        './model-aliases': { isStreamRequiredModel: (m) => /^gpt-5\.5(?:$|[-_:])/i.test((m || '').trim()) || /\[1m\]|context-1m/i.test(m || '') },
    });

/**
 * 渠道测试的默认端点必须对上该渠道的**真实出站协议**。
 *
 * 事故面（历史）：出站固定是 /responses 的渠道如果落进 default 分支按
 * openai_chat 测，测的就是这条渠道永远不会走的路径——测出来的绿是假的。
 *
 * 这类"测了一条不存在的路"的假绿比红更危险：它把问题藏起来。
 */

test('各渠道类型对上的端点与实际出站一致', () => {
    const cases = [
        [ChannelType.OpenAIChat, 'openai_chat'],
        [ChannelType.OpenAIResponse, 'openai_responses'],
        [ChannelType.Anthropic, 'anthropic_messages'],
        [ChannelType.Gemini, 'gemini_generate_content'],
        [ChannelType.OpenAIEmbedding, 'openai_embeddings'],
        [ChannelType.CustomOpenAIChat, 'openai_chat'],
        [ChannelType.SystemOne, 'systemone'],
    ];
    for (const [type, want] of cases) {
        assert.equal(defaultModelTestEndpointForChannel(type), want, `channel type ${type}`);
    }
});

/** 退役的 4 号位：没有类型、没有出厂适配器，前端也不该给它任何特殊待遇。 */
test('退役的 4 号位按未知类型处理', () => {
    assert.equal(defaultModelTestEndpointForChannel(4), 'openai_chat');
    assert.equal(channelTestIdentityFor('openai_responses', 4), 'generic');
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

/**
 * P2-2 反例 1：模型名不该锁流式。
 *
 * runner.go:911-914 明确"显式给了 stream 就听它的"；:959 的
 * `if !streamExplicit { stream = true }` 只是**未指定时默认流式**，客户端仍可传
 * false。原来 here 按模型名锁死，等于把一个后端允许的选项藏起来。
 */
test('模型名不会锁死流式开关（后端允许非流式）', () => {
    for (const m of ['gpt-5.5', 'opus[1m]', 'some-context-1m-model']) {
        assert.equal(
            shouldForceChannelTestStream({
                models: [m],
                endpoint: 'openai_chat',
                channelType: ChannelType.OpenAIChat,
            }),
            false,
            `模型 ${m} 不该锁流式`,
        );
    }
});

/**
 * P2-2 反例 2：Anthropic + 1M 但 cloak 关掉，不该锁流式。
 *
 * 1M helper（runner.go:1240-1254）的流式来自 cloak 门（调用点是
 * `if shouldApplyChannelCloak(...)`），不来自 1M 本身。
 */
test('cloak 关掉的 Anthropic 渠道即使开 1M 也不锁流式', () => {
    assert.equal(
        shouldForceChannelTestStream({
            endpoint: 'anthropic_messages',
            channelType: ChannelType.Anthropic,
            cloakMode: 'never',
            anthropicContext1M: true,
        }),
        false,
    );
    // cloak 生效时才锁。
    assert.equal(
        shouldForceChannelTestStream({
            endpoint: 'anthropic_messages',
            channelType: ChannelType.Anthropic,
            cloakMode: 'auto',
            anthropicContext1M: true,
        }),
        true,
    );
});

/**
 * P2-1 反例：非 Chat 族渠道上的残留 cloak.shape 必须被忽略。
 *
 * header_defaults.go:210 只有 OpenAIChat / CustomOpenAIChat 进
 * `switch chatChannelShape(channel)`。Form.tsx:766 换类型时会保留原 cloak_shape，
 * 所以"配过 codex 的 Chat 渠道改成 Gemini"这条路径是真实可达的。
 */
test('非 Chat 族渠道的残留 shape 不生效', () => {
    const stale = { mode: 'auto', shape: 'codex' };
    assert.equal(channelTestIdentityFor('openai_responses', ChannelType.OpenAIEmbedding, stale), 'generic');
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.Gemini, stale), 'generic');
    assert.equal(channelTestIdentityFor('openai_embeddings', ChannelType.OpenAIEmbedding, stale), 'generic');
    // 但 Chat 族上 shape 必须生效（否则修过头了）。
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.OpenAIChat, stale), 'codex');
    assert.equal(channelTestIdentityFor('openai_chat', ChannelType.CustomOpenAIChat, stale), 'codex');
});

/**
 * 图片端点：界面不许再宣称「强制流式 + codex 指纹」。
 *
 * 读码核实的后端事实：
 *   runner.go:674     图片端点在能力位/形状逻辑**之前**就转给 tryImageChannel
 *   image_channel.go:112  `stream := false` 写死，**不读** request.Stream
 *   relay/images.go:1012  User-Agent 只套 genericUAForChannel，无任何 codex 分支
 *
 * 也就是说一条 Responses(codex) 渠道测图片端点时，上游实际收到的是**非流式的
 * 通用 UA 请求**。而前端这两条判据原来只看渠道类型 → 界面锁死流式开关、还标
 * "codex CLI"，纯撒谎；顺带把"图片其实怎么发"这件事藏了起来。
 *
 * 别名清单对齐 runner.go:535：openai_images_generations / images /
 * images_generations / openai_images，且 normalizeEndpoint 会小写、去空白、
 * 把 '-' 和 '/' 换成 '_'（runner.go:499-506）。
 */
const IMAGE_ENDPOINTS = ['openai_images_generations', 'images', 'images_generations', 'openai_images'];

test('图片端点不锁流式，哪怕渠道是 Responses(codex) 形状', () => {
    for (const endpoint of IMAGE_ENDPOINTS) {
        assert.equal(
            shouldForceChannelTestStream({ endpoint, channelType: ChannelType.OpenAIResponse }),
            false,
            `${endpoint} 不该锁流式`,
        );
        // 别名的大小写 / 连字符 / 空白写法后端也认，前端不能只认一种拼法。
        assert.equal(
            shouldForceChannelTestStream({ endpoint: `  ${endpoint.toUpperCase().replace(/_/g, '-')} `, channelType: ChannelType.OpenAIResponse }),
            false,
            `${endpoint} 的变体拼法不该锁流式`,
        );
        // 图片分支先于渠道类型判据，所以别的渠道类型上同样不锁。
        assert.equal(shouldForceChannelTestStream({ endpoint, channelType: ChannelType.Anthropic }), false);
        // 老调用方（不给渠道类型）也不锁。
        assert.equal(shouldForceChannelTestStream({ endpoint }), false);
    }
});

test('图片端点的身份是通用 UA，不是 codex / claude', () => {
    for (const endpoint of IMAGE_ENDPOINTS) {
        // relay/images.go:1012 只套 genericUAForChannel —— 与渠道类型无关。
        assert.equal(channelTestIdentityFor(endpoint, ChannelType.OpenAIResponse), 'generic', `${endpoint} @ Responses`);
        assert.equal(channelTestIdentityFor(endpoint, ChannelType.Anthropic), 'generic', `${endpoint} @ Anthropic`);
        // cloak.shape=codex 也不生效：图片转发路径根本不读 shape。
        assert.equal(channelTestIdentityFor(endpoint, ChannelType.OpenAIChat, { mode: 'auto', shape: 'codex' }), 'generic', `${endpoint} @ Chat+codex shape`);
    }
});

/**
 * 防回归：同一个渠道上换个端点，结论必须跟着端点变。
 *
 * 这条同时钉住"逐端点算"这件事：如果哪天又退回"整批套一个渠道级值"，
 * 下面两组断言必然有一组红。
 */
test('同一 Responses 渠道：responses 端点仍恒流式 + codex，图片端点不锁 + generic', () => {
    const channel = { channelType: ChannelType.OpenAIResponse, cloakMode: 'auto' };
    assert.equal(shouldForceChannelTestStream({ ...channel, endpoint: 'openai_responses' }), true);
    assert.equal(channelTestIdentityFor('openai_responses', channel.channelType), 'codex');
    for (const endpoint of IMAGE_ENDPOINTS) {
        assert.equal(shouldForceChannelTestStream({ ...channel, endpoint }), false, `${endpoint} 混测时不该被整批锁`);
        assert.equal(channelTestIdentityFor(endpoint, channel.channelType), 'generic');
    }
});
