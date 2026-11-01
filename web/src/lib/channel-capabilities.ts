/**
 * 渠道能力位（capabilities）的前端小工具。
 *
 * 后端 model.Channel.Capabilities 是 []string，当前只有一个合法取值
 * 'embeddings'；空 = 只接聊天（后端会退回按渠道类型判定，存量渠道零影响）。
 *
 * 前端只用一个勾选框驱动它，但**数组一律按集合操作**：勾上 = 追加
 * 'embeddings'，取消 = 只删 'embeddings'，其它未来能力位（images / rerank …）
 * 原样保留。
 */

export const CAPABILITY_EMBEDDINGS = 'embeddings';

export function hasChannelCapability(
    capabilities: readonly string[] | undefined,
    capability: string,
): boolean {
    return (capabilities ?? []).includes(capability);
}

/**
 * 勾选框 → 新的 capabilities 数组。
 *
 * 刻意不写成 `enabled ? [capability] : []`：整体替换会连同将来并肩存在的
 * images / rerank 一起抹掉，等于用一个勾选框偷偷清空别的能力位。
 */
export function setChannelCapability(
    capabilities: readonly string[] | undefined,
    capability: string,
    enabled: boolean,
): string[] {
    const kept = (capabilities ?? []).filter((c) => c !== capability);
    return enabled ? [...kept, capability] : kept;
}

/**
 * 创建提交载荷里的 capabilities 字段。
 *
 * 空数组 / 未定义时**一个字段都不发**：没填过能力位的存量渠道，请求形状必须与
 * 改动前一致，多带一个 "capabilities: []" 就是行为变化。
 */
export function channelCapabilitiesPayload(
    capabilities: readonly string[] | undefined,
): { capabilities?: string[] } {
    const caps = [...(capabilities ?? [])];
    return caps.length > 0 ? { capabilities: caps } : {};
}

/**
 * 编辑提交载荷里的 capabilities 字段（patch 语义，只发变化）。
 *
 * 与创建不同：这里**允许发空数组** —— 那是"取消勾选"的唯一表达方式，不发就清不掉。
 */
export function channelCapabilitiesUpdate(
    current: readonly string[] | undefined,
    next: readonly string[] | undefined,
): { capabilities?: string[] } {
    const cur = [...(current ?? [])];
    const nxt = [...(next ?? [])];
    const unchanged = cur.length === nxt.length && cur.every((c, i) => c === nxt[i]);
    return unchanged ? {} : { capabilities: nxt };
}

/**
 * 渠道表单里三个能力开关在哪些渠道类型上显示（2026-10-31 需求决定，见 internal/model/channel_toggles.md）。
 *
 * 类型编号与后端 OutboundType / 前端 ChannelType 逐位对应（这里写数字是为了让本文件
 * 保持无 @/ 导入，node 测试能直接加载）：
 *   0 OpenAIChat · 1 OpenAIResponse · 2 Anthropic · 3 Gemini · 5 OpenAIEmbedding ·
 *   6 CustomOpenAIChat · 7 SystemOne
 *
 * - Claude 1M：只有 Anthropic（只有它出站带 anthropic-beta）。
 * - 思考内容放进正文：所有聊天类渠道；嵌入渠道(5)和决策渠道(7)不出聊天回复，隐藏。
 * - 也接嵌入请求：聊天类渠道显示；嵌入渠道(5)本来就只做嵌入（后端「类型默认 OR 勾选」），
 *   决策渠道(7)只服务 /v1/systemone，都隐藏。Anthropic 显示，但附一句提示——官方
 *   Anthropic 没有嵌入接口，只有上游中转也提供时才有用。
 * - 嵌入渠道(5)不显示开关，但「嵌入接口路径」照常单独显示（火山这类要填 /api/v3/embeddings），
 *   否则隐藏开关会顺带把它的路径逃生口藏掉。
 */
const CHANNEL_TYPE_ANTHROPIC = 2;
const CHANNEL_TYPE_OPENAI_EMBEDDING = 5;
const CHANNEL_TYPE_SYSTEM_ONE = 7;

export type ChannelToggleVisibility = {
    context1m: boolean;
    thinkingToContent: boolean;
    embeddings: boolean;
    embeddingsAnthropicNote: boolean;
    /** 不显示开关、只显示「嵌入接口路径」输入框（嵌入渠道本来就接嵌入）。 */
    embeddingsPathOnly: boolean;
};

export function channelToggleVisibility(channelType: number): ChannelToggleVisibility {
    const noChatReply = channelType === CHANNEL_TYPE_OPENAI_EMBEDDING || channelType === CHANNEL_TYPE_SYSTEM_ONE;
    const isAnthropic = channelType === CHANNEL_TYPE_ANTHROPIC;
    return {
        context1m: isAnthropic,
        thinkingToContent: !noChatReply,
        embeddings: !noChatReply,
        embeddingsAnthropicNote: isAnthropic,
        embeddingsPathOnly: channelType === CHANNEL_TYPE_OPENAI_EMBEDDING,
    };
}

/** 手机端「模型能力」分组摘要里「已开启 N 项」：只数当前类型上看得见、且开着的开关。 */
export function channelCapabilityOnCount(input: {
    type: number;
    anthropic_context_1m?: boolean;
    thinking_to_content?: boolean;
    capabilities?: readonly string[];
}): number {
    const v = channelToggleVisibility(input.type);
    return [
        v.context1m && !!input.anthropic_context_1m,
        v.thinkingToContent && !!input.thinking_to_content,
        v.embeddings && hasChannelCapability(input.capabilities, CAPABILITY_EMBEDDINGS),
    ].filter(Boolean).length;
}
