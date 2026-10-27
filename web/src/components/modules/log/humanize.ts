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
    octopus_route_unresolved: '没有可用渠道（请求没发出去）',
    octopus_channel_circuit_open: '渠道被熔断（连续失败被暂时停用）',
    octopus_all_channels_failed: '所有渠道都失败了',

    // ---- 客户端（发请求那一方）相关 ----
    octopus_client_timeout: '客户端等太久超时了',
    octopus_client_canceled: '客户端自己取消了请求',

    // ---- 本地预校验（请求还没真正发出去）----
    client_empty_request: '客户端发来了空请求',
    cursor_empty_probe: 'Cursor 的空探测请求（正常现象）',
    // 路由前置拒绝（上游未被触达）：网关侧给出的两个新码
    octopus_request_unparsable: '请求体读不出来（超体积上限或格式不对）',
    octopus_model_not_supported: '模型不在该 API Key 的允许清单里',

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
    model_test_embeddings: '连通性测试（向量）',
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

/**
 * 从 error_strategy 里取 `reason=xxx`。后端把「选路为什么没成」编码在这个分号串里
 * （如 `local_route_selection;reason=no_candidates;upstream_forwarded=false`），
 * 只有 reason 的部分值对运维有意义，其余是内部标记。
 */
function strategyReason(strategy: string | undefined): string {
    const raw = strategy?.trim();
    if (!raw) return '';
    for (const part of raw.split(';')) {
        const [key, value] = part.split('=');
        if (key?.trim().toLowerCase() === 'reason') return value?.trim().toLowerCase() ?? '';
    }
    return '';
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
 * 客户端识别: 把调用端 User-Agent 解析成人类可读的客户端名。
 *
 * 服务端只存原始 UA(对齐 new-api/axonhub 的做法), 映射在前端做——客户端生态
 * 漂移时改这里即可, 不动库表。识别不出的 UA 完整展示原样字符串, 不猜；换行交给卡片。
 * 顺序按「最具体的在前」: claude-cli 必须先于泛 claude, codex_cli_rs 先于 codex。
 *
 * 关于「不猜」的一个例外（必须说明，否则看起来像自相矛盾）：识别不出时仍展示原文，
 * 但**先剥掉其中的网址**。理由：不少 CLI 的 UA 形如
 * `SomeClient/1.0.0 (https://github.com/org/repo)`，原文照搬会让一个内部客户端的
 * 名字在日志里变成一个指向外部仓库的链接——既误导（它并不是可点的资源链接），
 * 也把第三方身份带进了自己的面板。所以保留「哪个客户端」，只切掉「它的主页在哪」。
 *
 * 注意：**已知 UA 不重命名、不缩短**。用户要求识别不出的完整展示，就是为了能一眼
 * 看出是哪个客户端、哪个版本；把它折叠成 `DSH` 反而丢了信息，也让排障变难。
 */
const CLIENT_LABELS: Array<[pattern: RegExp, label: string]> = [
    [/^claude-cli\//i, 'Claude CLI'],
    [/^codex_cli_rs|^codex_cli|^codex-tui|^codex_exec|^codex\//i, 'Codex CLI'],
    [/^codex/i, 'Codex'],
    [/^claude/i, 'Claude'],
    [/cursor/i, 'Cursor'],
    [/cherrystudio|^cherry/i, 'Cherry Studio'],
    [/continue\//i, 'Continue'],
    [/^cline/i, 'Cline'],
    [/^roo/i, 'Roo Code'],
    [/windsurf/i, 'Windsurf'],
    [/^dsh\//i, 'DSH'],
    [/^pi\//i, 'Pi'],
    [/vscode|^code\//i, 'VS Code'],
    [/^mozilla\//i, '浏览器'],
    [/python-requests|^python\//i, 'Python'],
    [/^node|^axios|^undici/i, 'Node.js'],
    [/^go-http-client/i, 'Go'],
    [/^okhttp|^java\//i, 'Java'],
    [/^curl\//i, 'curl'],
    [/^wget/i, 'wget'],
];

/**
 * 匹配 UA / 备注里夹带的网址：http(s)://... 或裸露的 github.com/... 等。
 *
 * `\+?` 不是可有可无的：RFC 9110 的注释语法里，括号内以 `+` 开头表示"附加信息"，
 * 真实 UA 大量写成 `Client/1.0 (+https://github.com/org/repo)`。少了这一个字符，
 * 剥完网址会留下一个孤零零的 `(+`，客户端名变成 `deepseek-harness/0.1.5-rc.2 (+`。
 */
const UA_URL_PATTERN = /\s*\(?\s*\+?\s*(?:https?:\/\/|www\.)[^\s)]+\s*\)?|\s*\(?\s*\+?\s*(?:github|gitlab|gitee)\.com\/[^\s)]+\s*\)?/gi;

/**
 * 切掉字符串里的网址，并收干净残留的空括号和多余空格。
 * 只用于展示，不参与任何判定，也不回写数据库。
 */
export function stripUrls(value: string | undefined): string | undefined {
    const raw = value?.trim();
    if (!raw || !UA_URL_PATTERN.test(raw)) {
        // 必须重置 lastIndex：上面用了 /g 的正则，test 会推进它。
        UA_URL_PATTERN.lastIndex = 0;
        return raw || undefined;
    }
    UA_URL_PATTERN.lastIndex = 0;
    const cleaned = raw
        .replace(UA_URL_PATTERN, ' ')
        .replace(/\(\s*\)/g, ' ')   // 网址被挖走后留下的空括号
        .replace(/\s+/g, ' ')
        .replace(/\s*([,;:])\s*$/g, '')  // 剥完后尾随的分隔符
        .trim();
    // 整串只有网址（如备注里只填了个链接）时，宁可显示空也不要露出外部地址。
    return cleaned || undefined;
}

export function humanizeClient(ua: string | undefined): string | undefined {
    const raw = ua?.trim();
    if (!raw) return undefined;
    for (const [pattern, label] of CLIENT_LABELS) {
        if (pattern.test(raw)) return label;
    }
    // 认不出来就照实展示，但先剥掉网址，避免客户端名变成一个外部链接。
    return stripUrls(raw);
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
        // Pinpoint which channels failed and which rescued the request.
        const attempts = log.attempts ?? [];
        const failedNames = new Set<string>();
        let rescueName = '';
        for (const a of attempts) {
            const name = a.channel_name ?? '';
            if (!name) continue;
            if (a.status === 'failed') failedNames.add(name);
            else if (a.status === 'success') rescueName = name;
        }
        if (failedNames.size > 0 && rescueName) {
            const failedList = [...failedNames].join('、');
            return {
                kind: 'warn',
                text: `⚠️ 渠道「${failedList}」失败 → 自动切换到「${rescueName}」成功`,
            };
        }
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
    if (code === 'octopus_route_unresolved') {
        const reason = strategyReason(log.error_strategy);
        if (reason === 'no_candidates') {
            return {
                kind: 'config',
                text: '❌ 失败：这个模型的分组里一条可用渠道都没有，请求根本没发出去。这不是上游的问题，也不会自己好——去「渠道」把这个模型挂到已启用的渠道上。',
            };
        }
        if (reason === 'route_unconfigured') {
            return {
                kind: 'config',
                text: '❌ 失败：这个模型没能匹配到任何分组，请求根本没发出去。检查模型名是否写对，以及「方案」里的模型路由是否指向了目标渠道。',
            };
        }
        return {
            kind: 'config',
            text: '❌ 失败：没有可用渠道，请求根本没发出去。这属于配置问题，等着不会自己好。',
        };
    }
    if (code === 'octopus_model_not_supported') {
        return {
            kind: 'config',
            text: '❌ 失败：这个模型不在当前 API Key 的允许清单里，请求根本没发出去。去「API Key」把模型加进 supported_models，或换个 key。',
        };
    }
    if (code === 'octopus_request_unparsable') {
        return {
            kind: 'client',
            text: '❌ 失败：网关没能读出客户端请求体（超过体积上限或格式不对），请求根本没发出去。',
        };
    }
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

/**
 * 上游自报模型名里的「地域 / 命名空间」前缀（如 cn:、global:）。这类前缀不是供应商，
 * 展示时不据此猜供应商真身。
 */
const REGION_NAMESPACE_HINTS = new Set([
    'cn', 'chn', 'global', 'intl', 'overseas', 'us', 'eu', 'asia', 'apac', 'hk', 'sg', 'jp', 'kr', 'mainland',
]);

/** 判断 vendor hint 是否为地域/命名空间前缀（是则不该当供应商展示）。 */
export function isRegionNamespaceHint(hint: string | undefined): boolean {
    const value = hint?.trim().toLowerCase() ?? '';
    if (!value) return false;
    if (REGION_NAMESPACE_HINTS.has(value)) return true;
    return /^(cn|us|eu|asia|apac|global|intl|overseas)[-_]/i.test(value);
}

/**
 * 只归一 cn: 这一种命名空间前缀（严格等价用）。刻意不做任意 ':' 切分——那会掩盖版本号等
 * 真实差异（例如 "model:beta" 与 "model" 并非同一模型）。
 */
function stripCnNamespace(value: string): string {
    return value.trim().replace(/^cn:/i, '').trim();
}

export type EchoTone = 'none' | 'trusted' | 'equivalent' | 'cross-family' | 'alias-variant' | 'name-mismatch';

export interface EchoComparison {
    tone: EchoTone;
    /** 是否需要渲染「from → to」对照（两侧显示名都非空且不同才渲染）。 */
    showComparison: boolean;
    fromLabel: string;
    toLabel: string;
    fromRaw: string;
    toRaw: string;
    /** trusted 专用：剔除地域/命名空间前缀后可展示的自报前缀，空串表示不展示。 */
    vendorPrefix: string;
    /** trusted 专用：上游自报的模型显示名（已去前缀）。 */
    modelName: string;
}

/**
 * 把上游回显收敛成一条展示档（只吃后端既有字段，不新增家族表、不冒充真模型）：
 *
 *   - cross-family：后端新 code `cross_family`，或旧口径 `model_family_verdict==='suspect'`
 *     且 `upstream_model_mismatch===false`（两侧家族可识别且无交集）——**唯一红标**；
 *   - trusted：后端 trusted；
 *   - equivalent：`cn:` 前缀严格等价（只归一 cn:），直接显示回显名，不挂「不符」；
 *   - alias-variant：`upstream_mismatch_code==='alias_variant'`；
 *   - name-mismatch：其余 `mismatch===true` 的未知差异（琥珀，from=发出名, to=回显名）；
 *   - none：上游未自报或没差异。
 *
 * 对照一律基于实际上游自报名（`upstream_response_model`）：cross-family 用「请求名 → 回显名」
 * （同名掩盖不了跨家族），其余用「发出名 → 回显名」。
 */
export function describeEchoComparison(
    log: Pick<
        RelayLog,
        | 'request_model_name'
        | 'actual_model_name'
        | 'upstream_response_model'
        | 'upstream_model_mismatch'
        | 'upstream_mismatch_code'
        | 'model_family_verdict'
        | 'model_vendor_hint'
    >,
    display: (value?: string) => string,
): EchoComparison {
    const echoRaw = log.upstream_response_model?.trim() ?? '';
    const actualRaw = log.actual_model_name?.trim() ?? '';
    const requestRaw = log.request_model_name?.trim() ?? '';
    const verdict = log.model_family_verdict?.trim() ?? '';
    const code = log.upstream_mismatch_code?.trim() ?? '';
    const mismatch = log.upstream_model_mismatch;

    const blank = (tone: EchoTone): EchoComparison => ({
        tone,
        showComparison: false,
        fromLabel: '',
        toLabel: '',
        fromRaw: '',
        toRaw: '',
        vendorPrefix: '',
        modelName: '',
    });

    if (!echoRaw) return blank('none');

    const echoLabel = display(echoRaw) || echoRaw;

    // 唯一红标：可确定不同家族。对照方向分两种：
    //   - code=cross_family：actual 与 echo 跨族（request 可能等于 echo）→ 对照 actual→echo；
    //   - 旧口径 suspect && mismatch===false：echo==actual，request 与 echo 跨族 → 对照 request→echo。
    if (code === 'cross_family' || (verdict === 'suspect' && mismatch === false)) {
        const crossByCode = code === 'cross_family';
        const fromRaw = crossByCode ? actualRaw : requestRaw;
        const fromLabel = display(fromRaw) || fromRaw;
        return {
            tone: 'cross-family',
            showComparison: Boolean(fromLabel && echoLabel && fromLabel !== echoLabel),
            fromLabel,
            toLabel: echoLabel,
            fromRaw,
            toRaw: echoRaw,
            vendorPrefix: '',
            modelName: '',
        };
    }

    if (verdict === 'trusted') {
        const vendorHint = log.model_vendor_hint?.trim() ?? '';
        const bare = vendorHint && (echoRaw.startsWith(`${vendorHint}/`) || echoRaw.startsWith(`${vendorHint}:`))
            ? echoRaw.slice(vendorHint.length + 1)
            : echoRaw;
        return {
            tone: 'trusted',
            showComparison: false,
            fromLabel: '',
            toLabel: echoLabel,
            fromRaw: '',
            toRaw: echoRaw,
            vendorPrefix: isRegionNamespaceHint(vendorHint) ? '' : vendorHint,
            modelName: display(bare) || bare,
        };
    }

    // cn: 前缀严格等价：旧行/未分档的行也直接显回显名，不挂琥珀「不符」。
    const cnEquivalent = actualRaw !== ''
        && stripCnNamespace(echoRaw).toLowerCase() === stripCnNamespace(actualRaw).toLowerCase();
    if (cnEquivalent) {
        return {
            tone: 'equivalent',
            showComparison: false,
            fromLabel: '',
            toLabel: echoLabel,
            fromRaw: actualRaw,
            toRaw: echoRaw,
            vendorPrefix: '',
            modelName: '',
        };
    }

    const actualLabel = display(actualRaw) || actualRaw;

    if (code === 'alias_variant') {
        return {
            tone: 'alias-variant',
            showComparison: Boolean(actualLabel && echoLabel && actualLabel !== echoLabel),
            fromLabel: actualLabel,
            toLabel: echoLabel,
            fromRaw: actualRaw,
            toRaw: echoRaw,
            vendorPrefix: '',
            modelName: '',
        };
    }

    if (mismatch === true) {
        return {
            tone: 'name-mismatch',
            showComparison: Boolean(actualLabel && echoLabel && actualLabel !== echoLabel),
            fromLabel: actualLabel,
            toLabel: echoLabel,
            fromRaw: actualRaw,
            toRaw: echoRaw,
            vendorPrefix: '',
            modelName: '',
        };
    }

    return blank('none');
}
