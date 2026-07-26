/**
 * 日志「人话」翻译字典
 *
 * 把网关日志里的工程取值（error_code / error_strategy / usage_source / usage_reason /
 * session_source 等）翻成不懂技术的管理员也能看懂的简体中文。
 *
 * 取值来源（只读参考，未改动）：
 *   - internal/model/log.go            error_code / usage_source / usage_missing_reason 常量
 *   - internal/relay/upstream_error.go upstreamErrorCode() / 各 localRelayError 的 code & strategy
 *   - internal/relay/relay.go          流式超时/错误的 code
 *   - internal/relay/images.go         图片流超时的 code
 *   - internal/relay/route_session_key.go  session_source
 *   - internal/model/setting.go        public_code 默认值 service_busy
 *
 * 注意：这里只做「展示层格式化」，不影响任何后端逻辑。
 */

import type { RelayLog } from '@/api/endpoints/log';

/** 错误码 -> 人话 */
const ERROR_CODE_LABELS: Record<string, string> = {
    // ---- 上游（你接的那个渠道/供应商）相关 ----
    octopus_upstream_unavailable: '上游服务不可用',
    octopus_upstream_rate_limited: '上游限流（请求太频繁被挡）',
    octopus_upstream_auth_failed: '上游拒绝认证（渠道密钥失效或没权限）',
    octopus_upstream_bad_request: '上游说请求有问题',
    octopus_upstream_non_2xx: '上游返回了异常状态',
    octopus_upstream_stream_timeout: '上游响应中途卡住超时',
    octopus_upstream_stream_error: '上游响应中途出错',
    // public_code 默认值（对外脱敏后的码，仍属上游繁忙类）
    service_busy: '服务繁忙（上游暂时没空）',

    // ---- 本网关侧（你的配置/调度）相关 ----
    octopus_no_available_channel: '没有可用渠道',
    octopus_channel_circuit_open: '渠道被熔断（连续失败被暂时停用）',
    octopus_all_channels_failed: '所有渠道都失败了',

    // ---- 客户端（发请求那一方）相关 ----
    octopus_client_timeout: '客户端等太久超时了',
    octopus_client_canceled: '客户端自己取消了请求',

    // ---- 本地预校验（请求还没真正发出去）----
    client_empty_request: '客户端发来了空请求',
    cursor_empty_probe: 'Cursor 的空探测请求（正常现象）',

    // ---- 上游供应商原样透出的码（上游 JSON 里的 code / type，开放集，只收高频）----
    // 后端 internal/relay/upstream_error.go 会把上游返回的 error.code/type 原样塞进 error_code，
    // 不在此表里的会走兜底美化，不再裸下划线糊脸。
    get_channel_failed: '上游无可用渠道',
    insufficient_quota: '上游额度不足',
    rate_limit_exceeded: '上游限流（请求太频繁）',
    invalid_request_error: '上游说请求格式有问题',
    invalid_api_key: '上游密钥无效',
    authentication_error: '上游认证失败',
    permission_error: '上游无此权限',
    model_not_found: '上游没有这个模型',
    context_length_exceeded: '上下文太长超限',
    overloaded_error: '上游过载',
    server_error: '上游服务器出错',
    billing_hard_limit_reached: '上游账户欠费/达上限',
};

/** usage_source -> 人话（用量数据从哪来的） */
const USAGE_SOURCE_LABELS: Record<string, string> = {
    upstream_usage: '上游回报',
    no_usage: '无用量',
    local_validation: '本地校验',
    local_estimate: '本地估算',
};

/** usage_missing_reason -> 人话（为什么没有用量数据） */
const USAGE_REASON_LABELS: Record<string, string> = {
    client_aborted: '客户端中断',
    no_internal_response: '上游没返回内容',
    upstream_usage_missing: '上游未报用量',
    zero_usage_reported: '用量为零',
    local_validation: '本地校验（未真正请求上游）',
    opaque_response: '响应无法解析用量',
};

/** session_source -> 人话（会话粘连是怎么定的） */
const SESSION_SOURCE_LABELS: Record<string, string> = {
    'octopus:request_fingerprint': '按请求指纹自动识别',
};

/** endpoint / 接口码 -> 人话（请求走的是哪类接口） */
const ENDPOINT_LABELS: Record<string, string> = {
    // ---- 正常业务接口 ----
    chat: '对话补全',
    chat_completions: '对话补全',
    responses: 'Responses 接口',
    messages: 'Messages 接口',
    gemini: 'Gemini 生成',
    gemini_generate_content: 'Gemini 生成',
    embeddings: '向量嵌入',
    // ---- 连通性测试（model test）----
    model_test: '连通性测试',
    model_test_chat: '连通性测试（对话）',
    model_test_responses: '连通性测试（Responses）',
    model_test_anthropic_messages: '连通性测试（Messages）',
    model_test_gemini: '连通性测试（Gemini）',
};

function lookup(map: Record<string, string>, raw: string | undefined): string | undefined {
    const key = raw?.trim();
    if (!key) return undefined;
    return map[key];
}

/**
 * 英文机器码（snake_case / kebab-case）退化成稍微像人话的样子：下划线/连字符换空格。
 * 仅作兜底——查得到字典的永远优先用字典中文，不把生 `model_not_found` 直接糊脸。
 */
function prettifyRawCode(raw: string): string {
    return raw.replace(/[_-]+/g, ' ').trim();
}

/** 是不是「连通性测试」类日志（model_test / model_test_xxx）。 */
export function isModelTestEndpoint(endpoint: string | undefined): boolean {
    return (endpoint?.trim() ?? '').startsWith('model_test');
}

/** endpoint 码翻人话；model_test 前缀统一归「连通性测试」，查不到就轻量美化。 */
export function humanizeEndpoint(endpoint: string | undefined): string | undefined {
    const key = endpoint?.trim();
    if (!key) return undefined;
    if (ENDPOINT_LABELS[key]) return ENDPOINT_LABELS[key];
    if (key.startsWith('model_test')) return '连通性测试';
    return prettifyRawCode(key);
}

/** 错误码翻人话；查不到就轻量美化（下划线换空格），不裸糊机器码。 */
export function humanizeErrorCode(code: string | undefined): string | undefined {
    const key = code?.trim();
    if (!key) return undefined;
    return ERROR_CODE_LABELS[key] ?? prettifyRawCode(key);
}

export function humanizeUsageSource(source: string | undefined): string | undefined {
    const raw = source?.trim();
    if (!raw) return undefined;
    return lookup(USAGE_SOURCE_LABELS, raw) ?? prettifyRawCode(raw);
}

export function humanizeUsageReason(reason: string | undefined): string | undefined {
    const raw = reason?.trim();
    if (!raw) return undefined;
    return lookup(USAGE_REASON_LABELS, raw) ?? prettifyRawCode(raw);
}

export function humanizeSessionSource(source: string | undefined): string | undefined {
    const raw = source?.trim();
    if (!raw) return undefined;
    return lookup(SESSION_SOURCE_LABELS, raw) ?? prettifyRawCode(raw);
}

/**
 * 一句话「锅在谁」结论。
 *
 * 判定优先级（从最明确到兜底）：
 *   1. 成功 / 部分成功
 *   2. 客户端自己的问题（取消 / 超时 / 空请求）
 *   3. 本网关配置或调度问题（没渠道 / 熔断 / 全失败）
 *   4. 上游问题（认证失败 / 限流 / 5xx / 繁忙等）
 *   5. 上游说请求本身有问题（400）
 *   6. 实在判不出来的兜底
 */
export interface LogVerdict {
    /** 'success' | 'warn' | 'client' | 'config' | 'upstream' | 'request' | 'unknown' */
    kind: 'success' | 'warn' | 'client' | 'config' | 'upstream' | 'request' | 'unknown';
    /** 给小白看的一句话结论（已含 emoji 前缀）。 */
    text: string;
}

export function getLogVerdict(
    log: RelayLog,
    severity: 'success' | 'warn' | 'error',
): LogVerdict {
    if (severity === 'success') {
        return { kind: 'success', text: '✅ 成功' };
    }
    if (severity === 'warn') {
        return {
            kind: 'warn',
            text: '⚠️ 最终成功，但中间有渠道尝试失败过（已自动重试到可用渠道）',
        };
    }

    // 连通性测试（model test）失败：直给一句直白结论，不套上游/客户端那套话术。
    if (isModelTestEndpoint(log.request_endpoint) || isModelTestEndpoint(log.request_path)) {
        return {
            kind: 'config',
            text: '❌ 连通性测试失败：这个渠道 + 模型当前打不通，检查渠道密钥、模型名或上游是否正常。',
        };
    }

    const code = log.error_code?.trim() ?? '';
    const status = log.error_status ?? 0;

    // 2. 客户端侧
    if (code === 'octopus_client_canceled') {
        return { kind: 'client', text: '❌ 失败：请求方（客户端）自己取消了，不是网关或上游的问题。' };
    }
    if (code === 'octopus_client_timeout') {
        return { kind: 'client', text: '❌ 失败：请求方（客户端）等待超时主动断开了。可能是上游太慢，也可能是客户端超时设得太短。' };
    }
    if (code === 'client_empty_request') {
        return { kind: 'client', text: '❌ 失败：客户端发来的是空请求，没有内容可处理。' };
    }

    // 3. 本网关配置 / 调度侧
    if (code === 'octopus_no_available_channel') {
        return { kind: 'config', text: '❌ 失败：没有可用渠道。请检查这个模型是否绑定了「已启用」的渠道。这通常是你的配置问题。' };
    }
    if (code === 'octopus_channel_circuit_open') {
        return { kind: 'config', text: '❌ 失败：渠道因连续出错被自动熔断（暂时停用）了。等它冷却恢复，或检查这个渠道的密钥/上游是否有问题。' };
    }
    if (code === 'octopus_all_channels_failed') {
        return { kind: 'config', text: '❌ 失败：能试的渠道全都失败了。多半是上游集体出问题，或这些渠道的密钥/配置都不对，点开下面看每个渠道的失败原因。' };
    }

    // 4. 上游侧
    if (code === 'octopus_upstream_auth_failed' || status === 401 || status === 403) {
        return { kind: 'upstream', text: '❌ 失败：上游拒绝了认证。多半是这个渠道的密钥过期/被封/没权限，需要换或修这个渠道的密钥。' };
    }
    if (code === 'octopus_upstream_rate_limited' || status === 429) {
        return { kind: 'upstream', text: '❌ 失败：被上游限流了（短时间请求太多）。这是上游那边的限制，过一会儿再试或加渠道分流。' };
    }
    if (code === 'octopus_upstream_stream_timeout' || code === 'octopus_upstream_stream_error') {
        return { kind: 'upstream', text: '❌ 失败：上游回复到一半卡住或出错了。这通常是上游不稳，不是你的配置错。' };
    }
    if (
        code === 'octopus_upstream_unavailable' ||
        code === 'service_busy' ||
        code === 'octopus_upstream_non_2xx' ||
        status >= 500
    ) {
        return { kind: 'upstream', text: '❌ 失败：上游渠道繁忙或暂时没有可用账号。这通常是上游的问题，不是你的配置错，过一会儿再试。' };
    }

    // 5. 上游说请求本身有问题
    if (code === 'octopus_upstream_bad_request' || status === 400) {
        return { kind: 'request', text: '❌ 失败：上游认为这条请求本身有问题（比如参数/模型名不对）。检查一下发请求方传的参数。' };
    }

    // 6. 兜底
    return {
        kind: 'unknown',
        text: '❌ 失败：发生了错误。展开下面的「技术详情」把内容发给技术同学排查。',
    };
}
