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
