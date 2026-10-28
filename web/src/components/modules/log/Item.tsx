'use client';

import React, { Fragment, useMemo, useState, useEffect, useCallback, type ReactNode } from 'react';
import { Brain, Clock, Cpu, Zap, AlertCircle, ArrowDownToLine, ArrowUpFromLine, DollarSign, ArrowRight, ArrowDown, ArrowUp, Send, MessageSquare, Loader2, RotateCw, ChevronDown, ChevronUp, ChevronLeft, ChevronRight, Pin, KeyRound, Percent, CheckCircle2, XCircle, Eye, Hash, MapPin, User, Gauge, type LucideIcon } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { motion, AnimatePresence } from 'motion/react';
import JsonView from '@uiw/react-json-view';
import { githubDarkTheme } from '@uiw/react-json-view/githubDark';
import { githubLightTheme } from '@uiw/react-json-view/githubLight';
import { useTheme } from 'next-themes';
import { create } from 'zustand';
import { fetchLogById, getRelayLogSeverity, type RelayLog, type ChannelAttempt } from '@/api/endpoints/log';
import {
    getLogVerdict,
    describeEchoComparison,
    humanizeClient,
    stripUrls,
    humanizeErrorCode,
    humanizeUsageSource,
    humanizeUsageReason,
    humanizeEndpoint,
    isModelTestEndpoint,
} from './humanize';
import { getModelIcon } from '@/lib/model-icons';
import { describeThroughput } from './throughput';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
import { describeProbeEvidence } from './probe-evidence';
import { ProbeEvidenceList } from './ProbeEvidenceList';
import { AuditReportPanel } from './AuditReportPanel';
import { ExpandableValue } from './ExpandableValue';
import { marketModelName } from '@/lib/model-aliases';
import { Badge } from '@/components/ui/badge';
import { cn } from '@/lib/utils';
import { CopyIconButton } from '@/components/common/CopyButton';
import { ErrorSafeText, MonoSafeText, SafeText } from '@/components/common/SafeText';
import {
    MorphingDialog,
    MorphingDialogTrigger,
    MorphingDialogContainer,
    MorphingDialogContent,
    MorphingDialogClose,
    MorphingDialogTitle,
    MorphingDialogDescription,
    useMorphingDialog,
} from '@/components/ui/morphing-dialog';
import { useAuthStore } from '@/api/endpoints/user';

/**
 * 敏感信息（API Key 名等）打码开关。默认打码，点筛选栏的眼睛才显形——
 * 截图/录屏给人看时不漏密钥名。state 放这里，筛选栏（index.tsx）控制开关。
 */
interface SensitiveState {
    sensitiveVisible: boolean;
    setSensitiveVisible: (visible: boolean) => void;
}
export const useSensitiveStore = create<SensitiveState>((set) => ({
    sensitiveVisible: false,
    setSensitiveVisible: (sensitiveVisible) => set({ sensitiveVisible }),
}));

/** 打码：不可见时返回 ••••，可见时原样。空值一律返回空串。 */
function maskSensitive(value: string, visible: boolean): string {
    if (!value) return '';
    return visible ? value : '••••';
}

function formatTime(timestamp: number): string {
    const millis = timestamp > 1_000_000_000_000 ? timestamp : timestamp * 1000;
    const date = new Date(millis);
    const now = new Date();
    const sameYear = date.getFullYear() === now.getFullYear();
    const timeZone = Intl.DateTimeFormat().resolvedOptions().timeZone;
    return date.toLocaleString('zh-CN', {
        year: sameYear ? undefined : 'numeric',
        month: '2-digit',
        day: '2-digit',
        hour: '2-digit',
        minute: '2-digit',
        second: '2-digit',
        timeZone,
        timeZoneName: 'short',
    });
}

function formatDuration(ms: number): string {
    return `${(ms / 1000).toFixed(2)}s`;
}

/**
 * 延迟档位 → 文字/图标语义色（快=绿、偏慢=琥珀、慢=红）。首字与总耗时阈值不同。
 * 直接把健康度染在「首字 / 总耗时」这两个数字上，一眼知快慢——取代原来那条没标签、
 * 谁都看不懂的双色 LatencyBar（左右半段各代表一个延迟档，语义太隐晦、又与右边数字重复）。
 */
function latencyTextColor(ms: number, goodMax: number, warnMax: number): string {
    if (!Number.isFinite(ms) || ms <= 0) return 'text-muted-foreground';
    if (ms <= goodMax) return 'text-emerald-600 dark:text-emerald-400';
    if (ms <= warnMax) return 'text-amber-600 dark:text-amber-400';
    return 'text-destructive';
}

function formatCacheRate(rate: number | undefined): string {
    return `${((rate ?? 0) * 100).toFixed((rate ?? 0) > 0 && (rate ?? 0) < 0.01 ? 2 : 1)}%`;
}

function formatTokensSummary(log: RelayLog): string {
    const total = (log.input_tokens ?? 0) + (log.output_tokens ?? 0);
    if (total === 0 && (log.cache_hit_tokens ?? 0) === 0) {
        return 'Tokens: 0';
    }
    const inStr = (log.input_tokens ?? 0).toLocaleString();
    const outStr = (log.output_tokens ?? 0).toLocaleString();
    const cacheTokens = (log.cache_hit_tokens ?? 0).toLocaleString();
    const cacheRate = (log.cache_hit_rate ?? 0) > 0 ? ` · ${formatCacheRate(log.cache_hit_rate)}` : '';
    return `Tokens: ${total.toLocaleString()} (入 ${inStr} / 出 ${outStr} / 缓 ${cacheTokens}${cacheRate})`;
}

function formatAttemptStatus(status: ChannelAttempt['status'], t: (key: string) => string): string {
    switch (status) {
        case 'success':
            return t('success');
        case 'failed':
            return t('failed');
        // skipped / circuit_break 是"没试"和"被暂时挡住"，不是失败——别再糊英文枚举给管理员。
        case 'skipped':
            return t('skipped');
        case 'circuit_break':
            return t('circuitBreak');
        default:
            // 后端将来新增状态时的兜底：下划线换空格，不裸糊机器码。
            return String(status).replace(/_/g, ' ');
    }
}

/**
 * 尝试状态的徽章配色。跳过/熔断不是失败：跳过往往是配置原因（渠道停用、无密钥、类型不兼容），
 * 熔断是会自己恢复的暂时状态。用琥珀色与真失败（红）区分，避免"排序靠后/暂时冷却"被看成"封禁"。
 */
function attemptStatusBadgeClass(status: ChannelAttempt['status']): string {
    if (status === 'success') return 'bg-primary/15 text-primary';
    if (status === 'failed') return 'bg-destructive/15 text-destructive';
    return 'bg-amber-500/15 text-amber-600 dark:text-amber-400';
}

function formatEndpointName(endpoint: string | undefined): string {
    let value = endpoint?.trim() ?? '';
    value = value.replace(/^\/?v1beta\//, '').replace(/^\/?v1\//, '');
    value = value.replace(/^models\//, 'gemini_');
    value = value.replace(':streamGenerateContent', '_stream_generate_content');
    value = value.replace(':generateContent', '_generate_content');

    switch (value) {
        case 'chat':
        case 'chat_completions':
            return 'chat';
        case 'responses':
            return 'responses';
        case 'messages':
            return 'messages';
        case 'embeddings':
            return 'embeddings';
        default:
            return value.replace(/_/g, '/');
    }
}

const LARGE_JSON_CHAR_LIMIT = 20000;
const LARGE_JSON_COLLAPSE_DEPTH = 2;
/**
 * 详情里默认最多渲染多少字符，超出部分先不塞进 DOM。
 *
 * 🔴 关键实测（生产 1.4MB 报错日志，真浏览器测）：
 *   接口返回 112ms、JSON.parse 5ms、对象树只有 452 个节点（折叠后根本不多）——
 *   **慢的不是解析也不是树形组件，而是把这个量级的内容塞进 DOM 做排版**：
 *   换行排版 4768ms、不换行也要 1422ms。而 128KB 只要 31ms、256KB 63ms。
 *
 * 所以正解是"默认只渲染一小段 + 想看全的再点"，而不是换渲染方式。
 * 这条也纠正了一个先前的误判：把大 JSON 改成纯文本反而更慢。
 */
const DETAIL_PREVIEW_CHARS = 128 * 1024;

/** IPv4 vs IPv6 tag for a downstream request IP (IPv6 literals contain ':'). */
function ipFamilyLabel(ip: string): string {
    return ip.includes(':') ? 'IPv6' : 'IPv4';
}

/**
 * 该次转发尝试的出站路由（管理员核对走了直连还是哪个代理）：直连 → 'direct'；
 * 经代理 → 'proxy · socks5 host:port'。代理再跳上游那一跳的真实出口 IP 对本服务
 * 不可见，故只记连到的代理地址（含 SOCKS）。
 */
function attemptRouteLabel(a: Pick<ChannelAttempt, 'proxy_used' | 'proxy_scheme' | 'proxy_target'>): string {
    if (!a.proxy_used) return 'direct';
    const parts: string[] = [];
    if (a.proxy_scheme) parts.push(a.proxy_scheme);
    if (a.proxy_target) parts.push(a.proxy_target);
    return parts.length ? `proxy · ${parts.join(' ')}` : 'proxy';
}

interface RetryBadgeWithPopoverProps {
    channelName: string;
    brandColor: string;
    attempts: ChannelAttempt[];
}

function RetryBadgeWithPopover({ channelName, brandColor, attempts }: RetryBadgeWithPopoverProps) {
    const t = useTranslations('log.card');

    return (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="secondary"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                        // Enter/Space 就地激活本徽标的 Popover（span[role=button] 没有原生按钮行为），
                        // 同时掐断冒泡，避免连带打开整卡详情。
                        e.stopPropagation();
                        if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            e.currentTarget.click();
                        }
                    }}
                    className="min-w-0 max-w-[min(12rem,100%)] cursor-pointer px-1.5 py-0 text-xs"
                    style={{ backgroundColor: `${brandColor}15`, color: brandColor }}
                >
                    <RotateCw className="size-3 mr-1 opacity-80" />
                    <SafeText value={channelName} className="text-xs" />
                </Badge>
            </PopoverTrigger>
            <PopoverContent
                align="start"
                className="flex max-h-[min(24rem,calc(100dvh-2rem))] w-[min(20rem,calc(100vw-1rem))] max-w-[calc(100vw-1rem)] flex-col gap-1 overflow-y-auto rounded-lg border bg-card p-2 shadow-sm"
            >
                <p className="px-1.5 pt-0.5 text-xs font-medium text-foreground">{t('retryDetails')}</p>
                {(() => {
                    // The "final" attempt = the last successful one, else the last attempt —
                    // it is what the top-level channel/result reflects (mirrors the server's
                    // finalChannel). Highlight it and number every attempt so the popover
                    // reads as one request's failover trail (#1 → #2 → … → final), not as
                    // several unrelated logs sharing a page.
                    let finalIdx = attempts.length - 1;
                    for (let i = attempts.length - 1; i >= 0; i--) {
                        if (attempts[i].status === 'success') {
                            finalIdx = i;
                            break;
                        }
                    }
                    return attempts.map((attempt, idx) => (
                        <div key={idx} className="flex flex-col w-full">
                            <div className={cn(
                                "flex items-center gap-2 rounded-md px-2 py-1.5 transition-colors",
                                idx === finalIdx
                                    ? "bg-muted/60 ring-1 ring-primary/40"
                                    : "hover:bg-muted/50"
                            )}>
                                <span className="shrink-0 font-mono text-[10px] text-muted-foreground/70">
                                    #{idx + 1}
                                </span>
                                <Badge
                                    className={cn(
                                        "h-5 shrink-0 px-1.5 text-[10px] font-bold uppercase shadow-none border-0",
                                        attemptStatusBadgeClass(attempt.status)
                                    )}
                                >
                                    {formatAttemptStatus(attempt.status, t)}
                                </Badge>
                                <div className="flex min-w-0 flex-col flex-1">
                                    <SafeText
                                        mode="wrap"
                                        value={attempt.channel_name}
                                        className="text-xs font-semibold text-foreground"
                                    />
                                        <div className="flex items-center gap-1">
                                            {(() => {
                                                const modelName = attempt.model_name || '';
                                                const { Avatar: AttemptAvatar } = getModelIcon(modelName);
                                                return AttemptAvatar ? <AttemptAvatar size={14} className="shrink-0" /> : null;
                                            })()}
                                            <MonoSafeText
                                                mode="wrap"
                                                value={`${marketModelName(attempt.model_name)} - ${formatDuration(attempt.duration)}`}
                                                className="text-[10px] text-muted-foreground"
                                            />
                                        </div>
                                    {attempt.msg && attempt.status !== 'success' && (
                                        <MonoSafeText
                                            mode="wrap"
                                            value={attempt.msg}
                                            className={cn(
                                                "text-[10px]",
                                                attempt.status === 'failed'
                                                    ? "text-destructive/90"
                                                    : "text-amber-600 dark:text-amber-400"
                                            )}
                                        />
                                    )}
                                    {attempt.upstream_path && (
                                        <MonoSafeText
                                            mode="wrap"
                                            value={attempt.upstream_path}
                                            className="text-[10px] text-muted-foreground"
                                        />
                                    )}
                                    {attempt.proxy_used && (
                                        <MonoSafeText
                                            mode="wrap"
                                            value={attemptRouteLabel(attempt)}
                                            className="text-[10px] text-amber-600 dark:text-amber-400"
                                        />
                                    )}
                                </div>
                            </div>
                            {
                                idx < attempts.length - 1 && (
                                    <div className="flex justify-center py-0.5">
                                        <ArrowDown className="size-3 text-muted-foreground/30" />
                                    </div>
                                )
                            }
                        </div>
                    ));
                })()}
            </PopoverContent>
        </Popover>
    );
}

interface LogRouteHeaderProps {
    variant: 'card' | 'detail';
    log: RelayLog;
    attempts: ChannelAttempt[];
    brandColor: string;
    hasMultipleAttempts: boolean;
    StatusIcon: LucideIcon;
    statusLabel: string;
    statusToneClass: string;
    requestEndpointLabel: string;
    endpointTitle: string;
    upstreamPaths: string[];
    upstreamPathTitle: string;
    /** card 专用槽位：A1 的「自动救援」徽标（路由状态，紧跟主状态徽标）。detail 忽略。 */
    autoRescueSlot?: ReactNode;
    /** 下游客户端徽标（含 Popover）：card 在 B1 行末、detail 在模型链后。缺省时不渲染。 */
    clientSlot?: ReactNode;
    /** card 专用槽位：B2 模型检测徽标。无结果时传 null，不造假占位。 */
    auditSlot?: ReactNode;
    /** card 专用槽位：B2 被动（纯本地）摘要徽标。与 auditSlot 同格，不新增轨道。 */
    passiveSlot?: ReactNode;
    /** card 专用槽位：B2 硬事实校验徽标。与 passiveSlot 并列，不新增轨道；无命中传 null。 */
    probeSlot?: ReactNode;
    /** card 专用槽位：B2 上游来源证据徽标。无证据传 null（不出"正常"占位）。 */
    sourceSlot?: ReactNode;
    /** card 专用槽位：C2「查看详情」。detail 忽略。 */
    detailsSlot?: ReactNode;
}

/**
 * 日志「路由头部」：状态 / 接口 / 上游路径徽标 → request_model_name → 渠道（多尝试展开
 * RetryBadgeWithPopover，否则渠道 Badge）→ actual_model_name → 可能的 stream / sticky 徽标。
 * 卡片列表头部与详情弹窗标题渲染同一套元素，只差尺寸与换行，用 variant 收敛：
 * card 走「自然三分区两排」grid（见下方 isCard 分支），detail 标题区是可换行 flex、
 * 但状态徽标被钉进固定首位 span（见 detail 分支注释）——两变体的成功/失败/警告标记
 * 都落在同一个固定槽位，不随文本长度漂移。
 * 两分支复用同一批原子元素，避免同一枚徽标出现两份实现。
 */
function LogRouteHeader({
    variant,
    log,
    attempts,
    brandColor,
    hasMultipleAttempts,
    StatusIcon,
    statusLabel,
    statusToneClass,
    requestEndpointLabel,
    endpointTitle,
    upstreamPaths,
    upstreamPathTitle,
    autoRescueSlot,
    clientSlot,
    auditSlot,
    passiveSlot,
    probeSlot,
    sourceSlot,
    detailsSlot,
}: LogRouteHeaderProps) {
    const t = useTranslations('log.card');
    const isCard = variant === 'card';
    // 卡片里模型名放不下就省略（鼠标悬停看全名），和正式站一样挤在同一行；
    // 弹窗标题仍换行，那里有整行宽度，不需要省略。
    const textMode = isCard ? 'truncate' : 'wrap';
    const requestModelDisplayName = marketModelName(log.request_model_name) || log.request_model_name;
    const actualModelDisplayName = marketModelName(log.actual_model_name) || log.actual_model_name;
    const upstreamEchoName = log.upstream_response_model?.trim() ?? '';
    const upstreamEchoDisplayName = upstreamEchoName
        ? (marketModelName(upstreamEchoName) || upstreamEchoName)
        : '';

    // 原子元素先摊平命名，card（固定列轨道）与 detail（可换行 flex）按各自顺序取用。
    const statusBadge = (
        <Badge
            variant="secondary"
            className={cn(
                "gap-1 border-0 text-xs",
                isCard ? "shrink-0 px-1.5 py-0" : "px-2 py-0.5",
                statusToneClass
            )}
        >
            <StatusIcon className={isCard ? "size-3" : "size-3.5"} />
            {statusLabel}
        </Badge>
    );
    const endpointBadge = requestEndpointLabel ? (
        <Badge
            variant="outline"
            className={cn(
                "min-w-0 shrink text-xs font-mono",
                isCard
                    ? "max-w-[14rem] border-border/70 bg-muted/40 px-1.5 py-0"
                    : "max-w-[16rem] border-border/70 bg-muted/40 px-2 py-0.5"
            )}
            title={endpointTitle}
        >
            <MonoSafeText value={requestEndpointLabel} className="text-xs" />
        </Badge>
    ) : null;
    const upstreamPathBadge = upstreamPaths.length > 0 ? (
        <Badge
            variant="outline"
            className={cn(
                "min-w-0 shrink text-xs font-mono",
                isCard
                    ? "max-w-[14rem] border-border/70 bg-muted/40 px-1.5 py-0"
                    : "max-w-[16rem] border-border/70 bg-muted/40 px-2 py-0.5"
            )}
            title={upstreamPathTitle}
        >
            <MonoSafeText value={upstreamPaths[0]} className="text-xs" />
        </Badge>
    ) : null;
    const requestModelText = (
        <ExpandableValue
            label={t('requestModelLabel')}
            value={log.request_model_name}
            className="font-semibold text-card-foreground"
        >
            <SafeText
                mode={textMode}
                value={requestModelDisplayName}
                title={requestModelDisplayName === log.request_model_name ? undefined : log.request_model_name}
                className="font-semibold text-card-foreground"
            />
        </ExpandableValue>
    );
    /* Channel identity is admin-only: a normal user's log carries no
       channel_name (see RelayLogUserSummary), so the header degrades to
       request model → actual model without ever revealing the upstream. */
    // 渠道拆成两个独立元素：箭头 + 徽标都落在中列的「请求→渠道→发出名」自然组里，
    // 由该组的 flex-wrap 自行摆放（不再靠固定列轨道），重试 Popover 里的 Badge 也是独立元素。
    const channelArrow = log.channel_name ? (
        <ArrowRight className="size-3.5 shrink-0 text-muted-foreground/50" />
    ) : null;
    const channelBadge = log.channel_name ? (
        hasMultipleAttempts ? (
            <RetryBadgeWithPopover
                channelName={log.channel_name}
                brandColor={brandColor}
                attempts={attempts}
            />
        ) : (
            <Badge
                variant="secondary"
                className="min-w-0 max-w-[min(12rem,100%)] px-1.5 py-0 text-xs"
                style={{ backgroundColor: `${brandColor}15`, color: brandColor }}
            >
                <SafeText value={log.channel_name} className="text-xs" />
            </Badge>
        )
    ) : null;
    const channelGroup = log.channel_name ? (<>{channelArrow}{channelBadge}</>) : null;
    const actualModelText = (
        <ExpandableValue
            label={t('actualModelLabel')}
            value={log.actual_model_name}
            className="text-muted-foreground"
        >
            <SafeText
                mode={textMode}
                value={actualModelDisplayName}
                title={actualModelDisplayName === log.actual_model_name ? undefined : log.actual_model_name}
                className="text-muted-foreground"
            />
        </ExpandableValue>
    );
    // 管理员才有渠道名/自报。普通用户摘要故意不下发，这里保持空白，不能用「未自报」冒充。
    const showEcho = Boolean(log.channel_name) || Boolean(upstreamEchoName) || typeof log.upstream_model_mismatch === 'boolean';
    const mappedByUs = Boolean(log.request_model_name && log.actual_model_name && log.request_model_name !== log.actual_model_name);
    // 回显判定收敛成一条展示档（吃后端既有字段，不新增家族表）：
    // cross-family（可确定不同家族，唯一红标）/ trusted / equivalent（cn: 前缀严格等价）/
    // alias-variant / name-mismatch（其余未知差异，琥珀）/ none。
    // 对照取实际回显；跨族 code 比发出名，映射跨族比请求名，其余差异比发出名。
    const echo = describeEchoComparison(log, (value) => marketModelName(value ?? ''));
    const echoTone = echo.tone;
    const familyVerdict = log.model_family_verdict?.trim() ?? '';
    const echoState = !upstreamEchoName ? 'silent' : (echoTone === 'trusted' || echoTone === 'equivalent' || log.upstream_model_mismatch === false) ? 'match' : 'mismatch';
    const echoComparison = echo.showComparison ? (
        <span
            className={cn(
                "inline-flex min-w-0 items-center gap-1 text-xs",
                echoTone === 'cross-family'
                    ? "text-red-600 dark:text-red-300"
                    : echoTone === 'alias-variant'
                        ? "text-muted-foreground"
                        : "text-amber-700 dark:text-amber-300"
            )}
            title={echoTone === 'cross-family'
                ? t('familyReplacedHint', { requested: echo.fromRaw, echo: echo.toRaw })
                : t('echoMismatchHint', { model: echo.toRaw })}
        >
            <SafeText value={echo.fromLabel} mode="wrap" className="min-w-0 text-xs" />
            <ArrowRight className="size-3.5 shrink-0" />
            <SafeText value={echo.toLabel} mode="wrap" className="min-w-0 text-xs font-medium" />
        </span>
    ) : null;
    // B2：渠道列下承接回显证据 + 风险徽标。整组可换行（窄屏/长回显名不裁），检测徽标另占一格。
    const echoGroup = showEcho ? (
        <span
            data-echo-state={echoState}
            data-echo-tone={echoTone}
            data-upstream-echo={upstreamEchoName}
            data-family-verdict={familyVerdict}
            className="inline-flex min-w-0 flex-wrap items-center gap-2"
        >
            {echoTone === 'trusted' ? (
                <SafeText
                    value={echo.vendorPrefix ? `${echo.vendorPrefix}${t('vendorModelSeparator')}${echo.modelName}` : echo.modelName}
                    mode="wrap"
                    title={t('familyTrustedHint')}
                    className="text-xs text-emerald-700 dark:text-emerald-300"
                />
            ) : echoTone === 'equivalent' ? (
                <SafeText
                    value={echo.toLabel || upstreamEchoDisplayName}
                    mode="wrap"
                    title={t('echoConsistent')}
                    className="text-xs text-muted-foreground"
                />
            ) : echoTone === 'cross-family' ? (
                <>
                    {echoComparison}
                    <Badge
                        variant="outline"
                        className={cn(
                            "overflow-visible whitespace-normal break-words border-red-500/40 bg-red-500/10 text-xs text-red-600 dark:text-red-300",
                            isCard ? "px-1.5 py-0" : "px-2 py-0.5"
                        )}
                        title={t('familyReplacedHint', { requested: echo.fromRaw, echo: echo.toRaw })}
                    >
                        {t('familyReplaced')}
                    </Badge>
                </>
            ) : echoTone === 'alias-variant' ? (
                <>
                    {echoComparison}
                    <Badge
                        variant="outline"
                        className={cn(
                            "overflow-visible whitespace-normal break-words border-border/60 bg-muted/30 text-xs font-normal text-muted-foreground",
                            isCard ? "px-1.5 py-0" : "px-2 py-0.5"
                        )}
                        title={t('echoAliasVariantHint', { model: echo.toRaw })}
                    >
                        {t('echoAliasVariant')}
                    </Badge>
                </>
            ) : echoTone === 'name-mismatch' ? (
                <>
                    {!familyVerdict && mappedByUs && (
                        <Badge
                            variant="outline"
                            className={cn(
                                "border-border/60 bg-muted/30 text-xs font-normal text-muted-foreground",
                                isCard ? "px-1.5 py-0" : "px-2 py-0.5"
                            )}
                            title={t('mappedByUs')}
                        >
                            {t('mappedByUs')}
                        </Badge>
                    )}
                    {echoComparison}
                    <Popover>
                        <PopoverTrigger asChild>
                            <Badge
                                variant="outline"
                                role="button"
                                tabIndex={0}
                                onClick={(e) => e.stopPropagation()}
                                onKeyDown={(e) => {
                                    // Enter/Space 就地激活「回显不符」Popover，不误开整卡。
                                    e.stopPropagation();
                                    if (e.key === 'Enter' || e.key === ' ') {
                                        e.preventDefault();
                                        e.currentTarget.click();
                                    }
                                }}
                                className={cn(
                                    "cursor-pointer overflow-visible whitespace-normal break-words border-amber-500/40 bg-amber-500/10 text-xs text-amber-800 dark:text-amber-200",
                                    isCard ? "px-1.5 py-0" : "px-2 py-0.5"
                                )}
                            >
                                {t('echoMismatch')}
                            </Badge>
                        </PopoverTrigger>
                        <PopoverContent align="start" className="w-72 p-3">
                            <p className="text-xs font-medium text-foreground">{t('echoMismatch')}</p>
                            <div className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-[11px]">
                                <span className="text-muted-foreground">{t('sentModel')}</span>
                                <span className="break-all">{echo.fromLabel || echo.fromRaw}</span>
                                <span className="text-muted-foreground">{t('upstreamEcho')}</span>
                                <span className="break-all text-amber-700 dark:text-amber-300">{echo.toLabel || echo.toRaw}</span>
                            </div>
                            {echo.toRaw && (
                                <p className="mt-2 border-t border-border pt-2 text-[11px] leading-5 text-muted-foreground">
                                    {t('echoMismatchHint', { model: echo.toRaw })}
                                </p>
                            )}
                        </PopoverContent>
                    </Popover>
                </>
            ) : upstreamEchoName ? (
                <SafeText value={upstreamEchoDisplayName} mode="wrap" title={t('upstreamEcho')} className="text-xs text-muted-foreground" />
            ) : (
                <span className="min-w-0 truncate text-xs text-muted-foreground/60" title={t('upstreamSilent')}>{t('upstreamSilent')}</span>
            )}
        </span>
    ) : null;
    // 流式/非流式徽标：card 里钉在第 1 排右列，不再跟随回显/客户端内容重新落位。
    const streamBadge = log.is_stream !== undefined ? (
        <Badge
            variant="outline"
            className="shrink-0 text-xs border-border/60 bg-muted/30 px-1.5 py-0"
        >
            {log.is_stream ? t('stream') : t('nonStream')}
        </Badge>
    ) : null;
    const stickyPin = log.attempts?.some(a => a.sticky)
        ? <Pin className="size-3.5 shrink-0 text-amber-500" />
        : null;

    if (isCard) {
        /**
         * 对齐 9d1619b：宽屏三列两行，按书写顺序自动落位，不手写 row-start。
         * 模型名和状态、接口挤在同一行；客户端、回显、查看详情在下一行。
         * 窄屏仍按这六个格子的顺序自然往下排。
         */
        const hasEchoEvidence = Boolean(echoGroup || auditSlot || passiveSlot || probeSlot || sourceSlot);
        return (
            <div
                data-log-header
                className="grid min-w-0 gap-x-4 gap-y-1 text-sm md:grid-cols-[auto_minmax(0,1fr)_auto] md:items-center"
            >
                <div data-log-row="1" data-log-cell="1" className="flex min-w-0 flex-wrap items-center gap-2">
                    <span className="inline-flex shrink-0 items-center">{statusBadge}</span>
                    {autoRescueSlot}
                    {endpointBadge}
                    {upstreamPathBadge}
                </div>
                <div data-log-row="1" data-log-cell="2" className="flex min-w-0 flex-nowrap items-center gap-2 overflow-hidden">
                    {requestModelText}
                    {channelGroup}
                    {actualModelText}
                </div>
                <div data-log-row="1" data-log-cell="3" className="flex min-w-0 flex-nowrap items-center gap-2 md:justify-self-end">
                    {streamBadge}
                    {stickyPin}
                </div>
                <div data-log-row="2" data-log-cell="1" className="flex min-w-0 flex-wrap items-center gap-2">
                    {clientSlot ?? (
                        <Badge
                            variant="outline"
                            className="min-w-0 border-border/60 bg-muted/30 px-1.5 py-0 text-xs font-normal text-muted-foreground"
                        >
                            {t('clientUnknown')}
                        </Badge>
                    )}
                </div>
                <div data-log-row="2" data-log-cell="2" className="flex min-w-0 flex-nowrap items-start gap-x-1.5">
                    {hasEchoEvidence ? <ArrowDown className="size-3.5 shrink-0 text-muted-foreground/40" /> : null}
                    {hasEchoEvidence ? (
                        <span className="flex min-w-0 flex-1 flex-wrap items-start gap-x-1.5 gap-y-1">
                            {echoGroup}
                            {auditSlot ? <span className="min-w-0">{auditSlot}</span> : null}
                            {passiveSlot ? <span className="min-w-0">{passiveSlot}</span> : null}
                            {probeSlot ? <span className="min-w-0">{probeSlot}</span> : null}
                            {sourceSlot ? <span className="min-w-0">{sourceSlot}</span> : null}
                        </span>
                    ) : null}
                </div>
                <div data-log-row="2" data-log-cell="3" className="hidden min-w-0 flex-nowrap items-center gap-1 text-xs text-muted-foreground md:flex md:justify-self-end">
                    {detailsSlot}
                </div>
            </div>
        );
    }

    // detail（弹窗标题）分支：把状态徽标包进一个固定顺序、不换行的首位 span——
    // 成功/失败/警告标记与 card 变体的 A1 首格落在同一个「永远第一、宽度固定」的槽位，
    // 不随后面文本的 flex-wrap 漂移。其余元素保持原有顺序。
    return (
        <>
            <span className="inline-flex shrink-0 items-center order-first">{statusBadge}</span>
            {endpointBadge}
            {upstreamPathBadge}
            {requestModelText}
            {channelGroup}
            {actualModelText}
            {clientSlot}
            {echoGroup}
            {passiveSlot}
            {probeSlot}
            {sourceSlot}
            {streamBadge}
            {stickyPin}
        </>
    );
}

function DetailTile({ icon, label, children }: { icon: ReactNode; label: ReactNode; children: ReactNode }) {
    return (
        <div className="min-w-0 rounded-xl border border-border/70 bg-muted/30 px-3 py-2">
            <div className="flex items-center gap-1.5 text-[11px] text-muted-foreground">
                {icon}
                {label}
            </div>
            <div className="mt-1 min-w-0">
                {children}
            </div>
        </div>
    );
}

function DeferredJsonContent({ content, fallbackText }: { content: string | undefined; fallbackText: string }) {
    const { resolvedTheme } = useTheme();
    const { isOpen } = useMorphingDialog();
    const [shouldRender, setShouldRender] = useState(false);

    // 默认只把前 DETAIL_PREVIEW_CHARS 个字符交给 DOM（实测 128KB≈31ms，
    // 而 1.4MB 换行排版要 4768ms）。想看全的点一下再付这个代价。
    const [expandAll, setExpandAll] = useState(false);
    // 大内容除了「加载全部」，还要能看尾部（流式结尾/错误堆栈在末尾）。
    const [showingTail, setShowingTail] = useState(false);
    const totalChars = content?.length ?? 0;
    const isTruncated = !expandAll && totalChars > DETAIL_PREVIEW_CHARS;
    const shownContent = isTruncated && content
        ? (showingTail ? content.slice(-DETAIL_PREVIEW_CHARS) : content.slice(0, DETAIL_PREVIEW_CHARS))
        : content;

    const parsed = useMemo(() => {
        if (!shownContent) return { isJson: false, data: null };
        if (!isOpen || !shouldRender) return { isJson: false, data: null };
        try {
            const data = JSON.parse(shownContent);
            return data !== null && typeof data === 'object'
                ? { isJson: true, data }
                : { isJson: false, data: shownContent };
        } catch {
            // 截断后的前缀几乎必然不是合法 JSON —— 走原文，这是预期行为。
            return { isJson: false, data: shownContent };
        }
    }, [shownContent, isOpen, shouldRender]);

    const isLargeJson = totalChars > LARGE_JSON_CHAR_LIMIT;
    // 只要是完整 JSON 就走树：树有折叠（实测 1.4MB 展开后仅 452 节点），
    // 比把整段倒进 <pre> 让排版引擎逐行分析便宜得多。早先把「超大 JSON」
    // 推去文本，等于"越大越用慢渲染器"，方向反了。
    const showTree = parsed.isJson && !isTruncated;

    // 内容换了（切到另一条日志）就把"加载全部/强制树形"复位，避免上一条的选择带到下一条。
    useEffect(() => {
        setShowingTail(false);
        setExpandAll(false);
    }, [content]);

    useEffect(() => {
        if (!isOpen) {
            const resetTimer = setTimeout(() => setShouldRender(false), 0);
            return () => clearTimeout(resetTimer);
        }

        // 只让弹窗动画先起一帧，不再空等 300ms——那 300ms 是纯延迟，1KB 的日志也要等。
        const raf = requestAnimationFrame(() => setShouldRender(true));
        return () => cancelAnimationFrame(raf);
    }, [isOpen]);

    if (!isOpen) {
        return null;
    }

    if (!content) {
        return (
            <pre className="max-w-full min-w-0 whitespace-pre-wrap break-words [overflow-wrap:anywhere] p-3 text-xs leading-relaxed text-muted-foreground sm:p-4">
                {fallbackText}
            </pre>
        );
    }

    // 不用 mode="wait"：那会强制等 loading 的 exit 动画（150ms）跑完才渲染内容，
    // 又多一段纯延迟。两段淡入淡出可以直接交叠。
    return (
        <AnimatePresence>
            {!shouldRender ? (
                <motion.div
                    key="loading"
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    exit={{ opacity: 0 }}
                    transition={{ duration: 0.15 }}
                    className="p-4 flex items-center justify-center h-full"
                >
                    <Loader2 className="h-5 w-5 text-muted-foreground animate-spin" />
                </motion.div>
            ) : showTree ? (
                <motion.div
                    key="json"
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    exit={{ opacity: 0 }}
                    transition={{ duration: 0.2 }}
                    className="max-w-full min-w-0 overflow-x-auto p-3 sm:p-4 [&_*]:max-w-full"
                >
                    <JsonView
                        value={parsed.data as object}
                        style={{
                            ...(resolvedTheme === 'dark' ? githubDarkTheme : githubLightTheme),
                            fontSize: '12px',
                            fontFamily: 'ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace',
                            backgroundColor: 'transparent',
                        }}
                        displayDataTypes={false}
                        displayObjectSize={false}
                        collapsed={isLargeJson ? LARGE_JSON_COLLAPSE_DEPTH : false}
                    />
                </motion.div>
            ) : (
                <motion.div
                    key="text"
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    exit={{ opacity: 0 }}
                    transition={{ duration: 0.2 }}
                    className="min-w-0"
                >
                    {/* 截断提示（默认态）：不塞满 DOM 才能秒开，见 DETAIL_PREVIEW_CHARS。 */}
                    {isTruncated || showingTail ? (
                        <div className="sticky top-0 z-10 flex flex-wrap items-center gap-2 border-b border-border bg-muted/60 px-3 py-1.5 text-[11px] text-muted-foreground backdrop-blur sm:px-4">
                            <span>
                                {showingTail ? '显示最后' : '先显示前'} {(DETAIL_PREVIEW_CHARS / 1024).toLocaleString()} KB
                                （共 {totalChars.toLocaleString()} 字符）
                            </span>
                            {/* 流式和错误堆栈真正要看的是尾部，"看最后 128KB"比"加载全部"更常有用，
                                而且零成本（同样只渲染 128KB）。 */}
                            {isTruncated ? (
                                <>
                                    <button
                                        type="button"
                                        onClick={() => { setShowingTail(true); setExpandAll(false); }}
                                        className="rounded border border-border px-1.5 py-0.5 text-foreground hover:bg-muted"
                                    >
                                        看最后 {(DETAIL_PREVIEW_CHARS / 1024).toLocaleString()} KB
                                    </button>
                                    <button
                                        type="button"
                                        onClick={() => { setShowingTail(false); setExpandAll(true); }}
                                        className="rounded border border-border px-1.5 py-0.5 text-foreground hover:bg-muted"
                                    >
                                        加载全部（较慢）
                                    </button>
                                </>
                            ) : (
                                <button
                                    type="button"
                                    onClick={() => { setShowingTail(false); setExpandAll(false); }}
                                    className="rounded border border-border px-1.5 py-0.5 text-foreground hover:bg-muted"
                                >
                                    回到开头
                                </button>
                            )}
                        </div>
                    ) : null}
                    {/* 展开态用 whitespace-pre（不换行、外层横向滚动）：实测同一份 1.4MB
                        换行排版 4768ms、不换行 1422ms。预览态才 wrap，因为量小且要好读。 */}
                    <pre className={cn(
                        'max-w-full min-w-0 p-3 font-mono text-xs leading-relaxed text-muted-foreground sm:p-4',
                        isTruncated ? 'whitespace-pre-wrap break-words [overflow-wrap:anywhere]' : 'whitespace-pre'
                    )}>
                        {shownContent}
                    </pre>
                </motion.div>
            )}
        </AnimatePresence>
    );
}


function LazyLogBodies({ logId, fallbackRequest, fallbackResponse, fallbackUpstreamRequest, requestLabel, responseLabel, upstreamRequestLabel, upstreamRequestHint, tokensLabel, cacheHitLabel, noRequestText, noResponseText, inputTokens, outputTokens, cacheHitTokens, isModelTest }: {
    logId: number;
    fallbackRequest?: string;
    fallbackResponse?: string;
    /** 真实出站请求体（列表/流式已被 Omit，正常来自详情接口）。 */
    fallbackUpstreamRequest?: string;
    upstreamRequestLabel: string;
    upstreamRequestHint: string;
    requestLabel: string;
    responseLabel: string;
    tokensLabel: string;
    cacheHitLabel: string;
    noRequestText: string;
    noResponseText: string;
    inputTokens: number;
    outputTokens: number;
    cacheHitTokens: number;
    isModelTest: boolean;
}) {
    const { isOpen } = useMorphingDialog();
    const [requestContent, setRequestContent] = useState<string | undefined>(fallbackRequest);
    const [responseContent, setResponseContent] = useState<string | undefined>(fallbackResponse);
    // 只有开了 upstream_request_body_log_enabled 的实例才有值；没有就不渲染那一块，
    // 不摆空壳（"没采集"和"采集到空"要能区分）。
    const [upstreamRequestBody, setUpstreamRequestBody] = useState<string | undefined>(fallbackUpstreamRequest);
    const [loading, setLoading] = useState(false);
    const [error, setError] = useState<string | null>(null);
    const loadedRef = React.useRef(false);

    useEffect(() => {
        if (!isOpen || loadedRef.current) return;
        // List rows intentionally omit bodies; fetch full detail once per open.
        if ((requestContent && requestContent.length > 0) || (responseContent && responseContent.length > 0)) {
            loadedRef.current = true;
            return;
        }
        let cancelled = false;
        setLoading(true);
        setError(null);
        fetchLogById(logId)
            .then((full) => {
                if (cancelled) return;
                setRequestContent(full.request_content);
                setResponseContent(full.response_content);
                setUpstreamRequestBody(full.upstream_request_body);
                loadedRef.current = true;
            })
            .catch((err: unknown) => {
                if (cancelled) return;
                setError(err instanceof Error ? err.message : String(err));
            })
            .finally(() => {
                if (!cancelled) setLoading(false);
            });
        return () => { cancelled = true; };
    }, [isOpen, logId, requestContent, responseContent]);

    return (
        <div className="grid grid-cols-1 gap-4 pb-2 md:h-full md:min-h-0 md:grid-cols-2 md:pb-0">
            <div className="flex min-h-[18rem] min-w-0 flex-col overflow-hidden rounded-lg border border-border bg-muted/30 md:min-h-0">
                <div className="flex min-w-0 items-center gap-2 px-3 md:px-4 py-2.5 md:py-3 border-b border-border bg-muted/50 shrink-0">
                    <Send className="size-4 shrink-0 text-muted-foreground" />
                    <span className="truncate text-sm font-medium text-card-foreground">{requestLabel}</span>
                    {!isModelTest && (
                        <Badge variant="secondary" className="ml-auto max-w-[55%] whitespace-normal text-left text-xs">
                            {inputTokens.toLocaleString()} {tokensLabel} · {cacheHitLabel} {cacheHitTokens.toLocaleString()}
                        </Badge>
                    )}
                </div>
                <div className="flex-1 overflow-auto min-h-0">
                    {loading ? (
                        <div className="flex items-center gap-2 p-4 text-xs text-muted-foreground"><Loader2 className="size-3.5 animate-spin" />Loading…</div>
                    ) : error ? (
                        <div className="p-4 text-xs text-destructive">{error}</div>
                    ) : (
                        <DeferredJsonContent content={requestContent} fallbackText={noRequestText} />
                    )}
                </div>
            </div>
            <div className="flex min-h-[18rem] min-w-0 flex-col overflow-hidden rounded-lg border border-border bg-muted/30 md:min-h-0">
                <div className="flex min-w-0 items-center gap-2 px-3 md:px-4 py-2.5 md:py-3 border-b border-border bg-muted/50 shrink-0">
                    <MessageSquare className="size-4 shrink-0 text-muted-foreground" />
                    <span className="truncate text-sm font-medium text-card-foreground">{responseLabel}</span>
                    {!isModelTest && (
                        <Badge variant="secondary" className="ml-auto max-w-[55%] whitespace-normal text-left text-xs">
                            {outputTokens.toLocaleString()} {tokensLabel}
                        </Badge>
                    )}
                </div>
                <div className="flex-1 overflow-auto min-h-0">
                    {loading ? (
                        <div className="flex items-center gap-2 p-4 text-xs text-muted-foreground"><Loader2 className="size-3.5 animate-spin" />Loading…</div>
                    ) : error ? (
                        <div className="p-4 text-xs text-destructive">{error}</div>
                    ) : (
                        <DeferredJsonContent content={responseContent} fallbackText={noResponseText} />
                    )}
                </div>
            </div>
            {/* 真实出站请求体：与上面「请求内容」并列时才能看出"我们以为发的"和
                "实际发的"差在哪（transform 的 defer 会把 Thinking/ReasoningEffort
                还原成客户端原意，请求内容那栏记录的是还原后的样子）。 */}
            {upstreamRequestBody ? (
                <div className="flex min-h-[14rem] min-w-0 flex-col overflow-hidden rounded-lg border border-border bg-muted/30 md:col-span-2 md:min-h-0">
                    <div className="flex min-w-0 items-center gap-2 px-3 md:px-4 py-2.5 md:py-3 border-b border-border bg-muted/50 shrink-0">
                        <ArrowUpFromLine className="size-4 shrink-0 text-muted-foreground" />
                        <span className="truncate text-sm font-medium text-card-foreground">{upstreamRequestLabel}</span>
                        <span className="ml-auto hidden shrink-0 text-[11px] text-muted-foreground sm:block">
                            {upstreamRequestHint}
                        </span>
                    </div>
                    <div className="flex-1 overflow-auto min-h-0">
                        <DeferredJsonContent content={upstreamRequestBody} fallbackText={noRequestText} />
                    </div>
                </div>
            ) : null}
        </div>
    );
}

/**
 * 详情弹窗里的「检测明细」。
 *
 * 为什么这里要自己再取一次详情：`model_audit_report` 和 request/response_content 一样
 * 只在详情接口返回，列表/实时流都被 Omit 掉了（internal/op/log.go 的 contentOmit）。
 * 也就是说卡片上那个 `log` 对象里这一列**永远是 undefined**——直接读它面板永远不会出现，
 * 所以必须走 GET /api/v1/log/{id}（fetchLogById）。取不到就当这行没被检测过，不摆空壳。
 */
function DetailAuditReport({ logId }: { logId: number }) {
    const { isOpen } = useMorphingDialog();
    const [fetched, setFetched] = useState<{ id: number; report?: string } | null>(null);

    useEffect(() => {
        if (!isOpen) return;
        // 同一条日志只取一次；翻到别的日志（id 变了）才重新取。
        if (fetched?.id === logId) return;
        let cancelled = false;
        fetchLogById(logId)
            .then((full) => {
                if (cancelled) return;
                setFetched({ id: logId, report: full.model_audit_report });
            })
            .catch(() => {
                // 详情取不到不该拖垮整个弹窗，其余内容照常显示。
            });
        return () => { cancelled = true; };
    }, [isOpen, logId, fetched]);

    return <AuditReportPanel report={fetched?.id === logId ? fetched.report : undefined} />;
}

type AuditHit = {
    channel_id: number;
    channel_name: string;
    model: string;
    trigger: string;
    verdict: string;
    score: number;
    finding_count: number;
};

type AuditBadge = {
    label: string;
    className: string;
    /** 本行审计字段原样保留，供点按弹层展示（来源/得分/发现数/渠道/模型）。 */
    hit: AuditHit;
};

/**
 * verdict → 徽标文案与配色。语义与检测页一致：
 * none=未见异常、unknown=证据不足、low/medium/high=待复核。
 */
function auditVerdictStyle(verdict: string, t: (key: string) => string): { label: string; className: string } {
    let label: string;
    let className: string;
    switch (verdict) {
        case 'none':
            label = t('auditClean');
            className = 'border-emerald-500/30 bg-emerald-500/10 text-emerald-600 dark:text-emerald-300';
            break;
        case 'unknown':
            label = t('auditInsufficient');
            className = 'border-border/60 bg-muted/30 text-muted-foreground';
            break;
        case 'low':
            label = t('auditLow');
            className = 'border-amber-500/30 bg-amber-500/10 text-amber-600 dark:text-amber-300';
            break;
        case 'medium':
            label = t('auditMedium');
            className = 'border-orange-500/30 bg-orange-500/10 text-orange-600 dark:text-orange-300';
            break;
        default:
            label = t('auditHigh');
            className = 'border-red-500/30 bg-red-500/10 text-red-600 dark:text-red-300';
            break;
    }
    return { label, className };
}

/**
 * 模型检测徽标。只读本行自己的审计字段（model_audit_verdict 等，只有这行真的被检测过才有值，
 * 逐行独立）。本行没有主动结论 → 无徽标：绝不回退到同渠道/同模型的定时快检快照（也不拿
 * 「整个渠道最近表现」冒充「这一条回复被检测过」）。
 * 徽标本身点按弹出详情（Popover，移动端手指可触），不再依赖悬停 title。
 */
function auditBadgeForLog(
    log: RelayLog,
    t: (key: string) => string,
): AuditBadge | undefined {
    // 只有本行真的被检测过才有主动结论；旧行/未检测一律无徽标。
    const rowVerdict = log.model_audit_verdict?.trim() ?? '';
    if (!rowVerdict) return undefined;
    const trigger = log.model_audit_trigger ?? '';
    const style = auditVerdictStyle(rowVerdict, t);
    return {
        label: `${auditSourceLabel(trigger, t)} · ${style.label}`,
        className: style.className,
        hit: {
            channel_id: log.channel,
            channel_name: log.channel_name ?? '',
            model: log.actual_model_name?.trim() || log.request_model_name?.trim() || '',
            trigger,
            verdict: rowVerdict,
            score: log.model_audit_score ?? 0,
            finding_count: log.model_audit_finding_n ?? 0,
        },
    };
}

/**
 * 主动检测来源 → 人话：manual=手动检测、schedule/echo_mismatch=历史自动检测；
 * 未知/未记录来源标「历史来源未记录」，绝不冒充被动结果或当前真伪。
 */
function auditSourceLabel(trigger: string, t: (key: string) => string): string {
    const value = trigger.trim();
    if (value === 'manual') return t('auditSourceManual');
    if (value === 'schedule' || value === 'echo_mismatch') return t('auditSourceSchedule');
    return t('auditSourceUnknown');
}

type PassiveState = 'missing' | 'insufficient' | 'not_applicable';
type PassiveSummary = NonNullable<RelayLog['passive_summary']>;
type PassiveBadge = {
    state: PassiveState;
    label: string;
    summary?: PassiveSummary;
};

/** 被动摘要微标永远是中性色（不风险、不认证），与 model_audit_* 主动字段互不混合。 */
const PASSIVE_NEUTRAL_CLASS = 'border-border/60 bg-muted/30 font-normal text-muted-foreground';

/**
 * 被动（纯本地）摘要徽标：有特征→「证据不足」，范围外→「未评估」，nil→「未分析」。
 * 只读 passive_summary，不拿任何主动字段/快照当样本。
 */
function passiveBadgeForLog(log: RelayLog, t: (key: string) => string): PassiveBadge {
    const summary = log.passive_summary;
    if (!summary || !summary.status) {
        return { state: 'missing', label: t('passiveBadgeMissing') };
    }
    if (summary.status === 'insufficient_evidence') {
        return { state: 'insufficient', label: t('passiveBadgeInsufficient'), summary };
    }
    return { state: 'not_applicable', label: t('passiveBadgeNotApplicable'), summary };
}

export const LogCard = React.memo(function LogCard({
    log,
    detailOpen,
    onDetailOpenChange,
    onNavigate,
    canNavigatePrev = false,
    canNavigateNext = false,
}: {
    log: RelayLog;
    /**
     * 受控的详情弹窗开合。传了就由外部接管——日志页用它实现「左右键连着翻」：
     * 翻页时换 log、弹窗保持打开。不传则回到原来的各自管自己。
     */
    detailOpen?: boolean;
    onDetailOpenChange?: (open: boolean) => void;
    /** 翻到相邻一条。-1 = 上一条（更新），+1 = 下一条（更旧）。 */
    onNavigate?: (direction: -1 | 1) => void;
    canNavigatePrev?: boolean;
    canNavigateNext?: boolean;
}) {
    const t = useTranslations('log.card');
    const canViewDetails = useAuthStore((state) => state.user?.role === 'admin');
    const modelNameToDisplay = log.actual_model_name?.trim() || log.request_model_name?.trim() || '';
    // 检测弹层用的三个显示名（与主行同款归一规则）：发送的模型 / 请求的模型 / 上游自报。
    const actualModelDisplayName = marketModelName(log.actual_model_name) || log.actual_model_name;
    const requestModelDisplayName = marketModelName(log.request_model_name) || log.request_model_name;
    const upstreamEchoDisplayName = useMemo(() => {
        const raw = log.upstream_response_model?.trim() ?? '';
        return raw ? (marketModelName(raw) || raw) : '';
    }, [log.upstream_response_model]);
    const { Avatar: ModelAvatar, color: brandColor } = useMemo(
        () => getModelIcon(modelNameToDisplay),
        [modelNameToDisplay]
    );
    const sensitiveVisible = useSensitiveStore((state) => state.sensitiveVisible);
    const requestAPIKeyName = useMemo(() => log.request_api_key_name?.trim() ?? '', [log.request_api_key_name]);
    const userName = useMemo(() => log.user_name?.trim() ?? '', [log.user_name]);
    const reasoningEffort = useMemo(() => log.reasoning_effort?.trim() ?? '', [log.reasoning_effort]);
    const channelKeyRemark = useMemo(() => log.channel_key_remark?.trim() ?? '', [log.channel_key_remark]);
    const clientLabel = useMemo(() => humanizeClient(log.request_user_agent), [log.request_user_agent]);
    const auditBadge = useMemo(
        () => auditBadgeForLog(log, t),
        [log, t],
    );
    // 本行有无正文的被动摘要时可读；管理员另显示旧行尚未分析的状态。
    const passiveBadge = useMemo(
        () => ((canViewDetails || log.passive_summary) ? passiveBadgeForLog(log, t) : null),
        [log, t, canViewDetails],
    );
    const requestEndpoint = useMemo(() => log.request_endpoint?.trim() ?? '', [log.request_endpoint]);
    const requestPath = useMemo(() => log.request_path?.trim() ?? '', [log.request_path]);
    // 连通性测试日志：费用/缓存/token 这些对它全是噪音，按这个标记隐掉。
    const isModelTest = isModelTestEndpoint(requestEndpoint) || isModelTestEndpoint(requestPath);
    // 生成速度（token/s）。算不出来就是 null，整块不渲染——不拿 0 / -- 占位。
    const tpsReading = useMemo(() => describeThroughput(log), [log]);
    // 列表/详情头部那个徽章显示的是「人话」接口名，不再是裸 endpoint 码。
    const requestEndpointLabel = useMemo(
        () => humanizeEndpoint(formatEndpointName(requestEndpoint || requestPath)) ?? '',
        [requestEndpoint, requestPath]
    );
    // 详情里的接口信息：干净的「中文标签 + 纯值」，不再拼 `endpoint:`/`path:` 裸前缀。
    const endpointLines = useMemo(() => {
        const lines: Array<{ label: string; value: string }> = [];
        if (requestEndpoint) lines.push({ label: '接口类型', value: humanizeEndpoint(requestEndpoint) ?? requestEndpoint });
        if (requestPath && requestPath !== requestEndpoint) lines.push({ label: '请求路径', value: requestPath });
        if (!lines.length && requestEndpointLabel) lines.push({ label: '接口类型', value: requestEndpointLabel });
        return lines;
    }, [requestEndpoint, requestEndpointLabel, requestPath]);
    const endpointTitle = endpointLines.map((line) => `${line.label}：${line.value}`).join('\n') || requestEndpointLabel;
    const attempts = useMemo(() => log.attempts ?? [], [log.attempts]);
    // 上游路径：纯路径值，标签在外面单独给「上游路径」，不再拼 `upstream:` 前缀。
    const upstreamPaths = useMemo(() => {
        const seen = new Set<string>();
        return attempts
            .map((attempt) => attempt.upstream_path?.trim() ?? '')
            .filter((path) => {
                if (!path || seen.has(path)) return false;
                seen.add(path);
                return true;
            });
    }, [attempts]);
    const upstreamPathTitle = upstreamPaths.length ? `上游路径：\n${upstreamPaths.join('\n')}` : '';
    const errorCode = log.error_code?.trim() ?? '';
    const errorStrategy = log.error_strategy?.trim() ?? '';

    const severity = getRelayLogSeverity(log);
    const hasError = severity === 'error';
    const hasPartialFailure = severity === 'warn';
    const hasMultipleAttempts = attempts.length > 1;
    const shouldShowAttempts = attempts.length > 0 && (hasError || hasPartialFailure || hasMultipleAttempts);
    const attemptCount = log.total_attempts || attempts.length || 1;
    const usageSource = log.usage_source?.trim() ?? '';
    const usageMissingReason = log.usage_missing_reason?.trim() ?? '';
    const sessionSource = log.session_source?.trim() ?? '';
    const sessionKey = log.session_key?.trim() ?? '';

    // 人话结论：直给「成没成、锅在谁」
    const verdict = getLogVerdict(log, severity);
    // 错误码翻人话（普通视图只显示这个，原始码进技术详情）
    const humanErrorCode = humanizeErrorCode(errorCode);
    const humanUsageSource = humanizeUsageSource(usageSource);
    const humanUsageReason = humanizeUsageReason(usageMissingReason);
    // 「技术详情」里给高级用户看的原始取值（默认收起）
    const techMeta = [
        log.error_status !== undefined && log.error_status !== null && log.error_status !== 0
            ? { label: 'error_status', value: String(log.error_status) }
            : null,
        errorCode ? { label: 'error_code', value: errorCode } : null,
        errorStrategy ? { label: 'error_strategy', value: errorStrategy } : null,
        sessionSource ? { label: 'session_source', value: sessionSource } : null,
        sessionKey ? { label: 'session_key', value: sessionKey } : null,
        usageSource ? { label: 'usage_source', value: usageSource } : null,
        usageMissingReason ? { label: 'usage_reason', value: usageMissingReason } : null,
    ].filter((item): item is { label: string; value: string } => item !== null);

    const [isDiagnosticExpanded, setIsDiagnosticExpanded] = useState(false);
    const [isTechExpanded, setIsTechExpanded] = useState(false);
    const statusLabel = hasError ? t('failedStatus') : hasPartialFailure ? t('warnStatus') : t('successStatus');
    const StatusIcon = hasError ? XCircle : hasPartialFailure ? AlertCircle : CheckCircle2;
    const statusToneClass = hasError
        ? "bg-destructive/10 text-destructive"
        : hasPartialFailure
            ? "bg-amber-500/10 text-amber-700 dark:text-amber-300"
            : "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400";
    const statusTextClass = hasError
        ? "text-destructive"
        : hasPartialFailure
            ? "text-amber-700 dark:text-amber-300"
            : "text-emerald-600 dark:text-emerald-400";

    /**
     * 卡片头部各槽位（交给 LogRouteHeader 摆进固定列轨道）：
     * A1 自动救援（路由状态，与主状态徽标同格同高）、A2 下游客户端、B2 模型检测、C2 查看详情。
     * 客户端槽位缺失时由 LogRouteHeader 兜底渲染低强调占位；检测徽标无结果就是 null。
     */
    const autoRescueSlot = (hasMultipleAttempts && hasPartialFailure) ? (
        <Badge
            variant="secondary"
            className="shrink-0 gap-1 border-0 bg-amber-500/15 px-1.5 py-0 text-xs text-amber-700 dark:text-amber-300"
        >
            <RotateCw className="size-3 opacity-80" />
            {t('autoRescue')}
        </Badge>
    ) : null;
    // 客户端徽标来自调用端 UA（humanizeClient）。徽标点按/Enter/Space 弹出详情（Popover）——移动端
    // 手指可触、键盘可激活，不依赖悬停。卡片外层是 MorphingDialogTrigger（自带 onClick/onKeyDown），
    // 徽标必须就地掐断事件冒泡，否则会连带打开整卡详情弹窗。
    const clientSlot = clientLabel ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                        // Enter/Space 就地激活 Popover，不误开整卡。
                        e.stopPropagation();
                        if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            e.currentTarget.click();
                        }
                    }}
                    title={clientLabel}
                    className="min-w-0 max-w-full cursor-pointer overflow-visible whitespace-normal break-all border-violet-500/30 bg-violet-500/10 px-1.5 py-0 text-xs text-violet-600 dark:text-violet-300"
                >
                    {clientLabel}
                </Badge>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-72 p-3">
                <p className="text-xs font-medium text-foreground">{t('client')}</p>
                <p className="mt-1 break-all font-mono text-[11px] leading-5 text-muted-foreground">
                    {stripUrls(log.request_user_agent)}
                </p>
            </PopoverContent>
        </Popover>
    ) : null;
    // 检测徽标只来自本行自己的审计字段（本行没被检测过 → null，不拿快照冒充）。
    // 弹层同时给「发送模型 vs 上游自报」的本行证据与来源；同样掐断冒泡，避免误开整卡详情。
    const auditSlot = auditBadge ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                        // Enter/Space 就地激活「模型检测」Popover，不误开整卡。
                        e.stopPropagation();
                        if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            e.currentTarget.click();
                        }
                    }}
                    className={cn('min-w-0 cursor-pointer overflow-visible whitespace-normal px-1.5 py-0 text-xs', auditBadge.className)}
                >
                    {auditBadge.label}
                </Badge>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-80 p-3">
                <div className="space-y-2 text-xs">
                    <p className="font-medium text-foreground">
                        {t('auditPrefix')} · {auditBadge.hit.channel_name || `渠道 ${auditBadge.hit.channel_id}`}
                    </p>
                    <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
                        <span className="text-muted-foreground">{t('auditModel')}</span>
                        <span className="break-all">{auditBadge.hit.model}</span>
                        <span className="text-muted-foreground">{t('auditSource')}</span>
                        <span>{auditSourceLabel(auditBadge.hit.trigger, t)}</span>
                        <span className="text-muted-foreground">{t('auditRow')}</span>
                        <span className="break-all">#{log.id} · {formatTime(log.time)}</span>
                        {typeof auditBadge.hit.score === 'number' && (
                            <>
                                <span className="text-muted-foreground">{t('auditScore')}</span>
                                <span className="tabular-nums">{auditBadge.hit.score}</span>
                            </>
                        )}
                        {typeof auditBadge.hit.finding_count === 'number' && auditBadge.hit.finding_count > 0 && (
                            <>
                                <span className="text-muted-foreground">{t('auditFindings')}</span>
                                <span className="tabular-nums">{auditBadge.hit.finding_count}</span>
                            </>
                        )}
                    </div>
                    {/* 本行证据：发送的模型 vs 上游自报的模型（sub2api 式
                        「被路由到什么模型」——上游自报与发送不符是最直接的
                        造假线索；上游沉默则无法判定）。 */}
                    <div className="border-t border-border pt-2">
                        <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
                            <span className="text-muted-foreground">{t('sentModel')}</span>
                            <span className="break-all">{actualModelDisplayName || requestModelDisplayName}</span>
                            <span className="text-muted-foreground">{t('upstreamDeclared')}</span>
                            {upstreamEchoDisplayName ? (
                                <span className={cn('break-all', log.upstream_model_mismatch === true && 'font-medium text-amber-600 dark:text-amber-400')}>
                                    {upstreamEchoDisplayName}
                                    {log.upstream_model_mismatch === true && (log.upstream_mismatch_code === 'alias_variant' ? ` · ${t('echoAliasVariant')}` : ` · ${t('echoInconsistent')}`)}
                                    {log.upstream_model_mismatch === false && ` · ${t('echoConsistent')}`}
                                </span>
                            ) : (
                                <span className="text-muted-foreground/70">{t('upstreamSilent')}</span>
                            )}
                        </div>
                    </div>
                </div>
            </PopoverContent>
        </Popover>
    ) : null;
    // 硬事实校验徽标：这条日志里"账对不上"的地方。
    //
    // 与被动摘要徽标刻意分开：摘要说的是"回复长什么样"，这里说的是"数字自相矛盾"。
    // 两者证据强度不同，混成一个徽标会让硬问题看着像软观察。
    // 只有真的命中才出徽标——没命中不出"正常"徽标，避免被读成"已验真"。
    // 上游后端来源证据徽标（纯被动、零额外请求）。
    //
    // 与上面两个徽标分开的第三件事：摘要说"回复长什么样"，probes 说"数字自相矛盾"，
    // 这里说"这条日志更可能来自哪种后端"。三者证据来源不同，混在一起会让人
    // 把"看着像"读成"已确认"。
    //
    // 🔴 只在真的拿到证据时才出徽标（三源分全 0 且无信号无返回），而且文案里
    // 不许出现"确认/官方"这类认证措辞——这套逻辑只能证伪。
    const source = log.passive_source;
    const hasSourceEvidence = Boolean(
        source && (
            (source.anthropic_score ?? 0) > 0 ||
            (source.counter_evidence?.length ?? 0) > 0 ||
            (source.signals?.length ?? 0) > 0
        ),
    );
    const sourceSlot = hasSourceEvidence && source ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                        e.stopPropagation();
                        if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            e.currentTarget.click();
                        }
                    }}
                    className={cn('min-w-0 cursor-pointer whitespace-normal px-1.5 py-0 text-xs', PASSIVE_NEUTRAL_CLASS)}
                    title={t('passiveSourceTitle')}
                >
                    {t('passiveSourceTitle')}
                </Badge>
            </PopoverTrigger>
            <PopoverContent
                align="start"
                className="w-72 space-y-2 text-xs"
                onClick={(e) => e.stopPropagation()}
            >
                <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
                    <span className="text-muted-foreground">Anthropic</span>
                    <span className="tabular-nums">{source.anthropic_score ?? 0}</span>
                </div>
                {(source.signals?.length ?? 0) > 0 && (
                    <div className="border-t border-border pt-2">
                        <div className="text-muted-foreground">{t('passiveSourceSignals')}</div>
                        <ul className="mt-1 space-y-0.5">
                            {source.signals!.map((sig) => (
                                <li key={sig} className="break-all font-mono text-[11px]">{sig}</li>
                            ))}
                        </ul>
                    </div>
                )}
                {(source.counter_evidence?.length ?? 0) > 0 && (
                    <div className="border-t border-border pt-2">
                        <div className="text-muted-foreground">{t('passiveSourceCounter')}</div>
                        <ul className="mt-1 space-y-0.5">
                            {source.counter_evidence!.map((item) => (
                                <li key={item} className="break-all font-mono text-[11px]">{item}</li>
                            ))}
                        </ul>
                    </div>
                )}
            </PopoverContent>
        </Popover>
    ) : null;
    const probes = log.passive_probes ?? [];
    const probeSlot = probes.length > 0 ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                        e.stopPropagation();
                        if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            e.currentTarget.click();
                        }
                    }}
                    className={cn(
                        'min-w-0 cursor-pointer overflow-visible whitespace-normal px-1.5 py-0 text-xs',
                        probes.some((p) => p.severity === 'medium' && p.confidence !== 'low')
                            ? 'border-amber-500/40 text-amber-600 dark:text-amber-400'
                            : 'border-border text-muted-foreground'
                    )}
                >
                    账目 {probes.length}
                </Badge>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-96 p-3">
                <div className="space-y-2 text-xs">
                    <p className="font-medium text-foreground">这条日志里对不上的地方</p>
                    <p className="text-[11px] leading-5 text-muted-foreground">
                        以下都是只用这条日志本身算出来的，没有额外请求、没有消耗 token。
                        有命中值得去查；没有命中也不代表模型是真的。
                    </p>
                    <ul className="space-y-2">
                        {probes.map((probe, index) => {
                            // 证据摊成"中文名 + 值 + 这行怎么算的"。摆原始数字而不是只给结论，
                            // 是为了让人自己能复算——复算不了的东西不该叫证据。
                            const rows = describeProbeEvidence(probe.code, probe.evidence);
                            return (
                                <li key={`${probe.code}-${index}`} className="rounded-lg border border-border px-2 py-1.5">
                                    <p className="leading-5">{probe.title}</p>
                                    <p className="mt-0.5 text-[11px] text-muted-foreground">
                                        置信度：{probe.confidence === 'high' ? '高' : probe.confidence === 'medium' ? '中' : probe.confidence === 'low' ? '低' : '未评估（旧数据）'}
                                    </p>
                                    {rows.length > 0 && (
                                        <div className="mt-1.5 space-y-0.5 text-[11px] text-muted-foreground">
                                            {rows.map((row) => (
                                                <div key={row.label} className="flex flex-wrap items-baseline gap-x-1.5">
                                                    <span className="shrink-0">{row.label}</span>
                                                    <span className="font-mono tabular-nums text-foreground">{row.value}</span>
                                                    {row.formula && (
                                                        <span className="text-muted-foreground/70">（{row.formula}）</span>
                                                    )}
                                                </div>
                                            ))}
                                        </div>
                                    )}
                                </li>
                            );
                        })}
                    </ul>
                </div>
            </PopoverContent>
        </Popover>
    ) : null;
    // 被动（纯本地）摘要徽标：中性色，弹层只给版本/粗语言/六项特征与说明，不认证真伪。
    const passiveSlot = passiveBadge ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => {
                        e.stopPropagation();
                        if (e.key === 'Enter' || e.key === ' ') {
                            e.preventDefault();
                            e.currentTarget.click();
                        }
                    }}
                    className={cn('min-w-0 cursor-pointer overflow-visible whitespace-normal px-1.5 py-0 text-xs', PASSIVE_NEUTRAL_CLASS)}
                >
                    {passiveBadge.label}
                </Badge>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-80 p-3">
                <div className="space-y-2 text-xs">
                    <p className="font-medium text-foreground">{t('passiveTitle')} · {passiveBadge.label}</p>
                    <p className="text-[11px] leading-5 text-muted-foreground">{t('passiveExplain')}</p>
                    {passiveBadge.state === 'insufficient' && passiveBadge.summary && (
                        <>
                            <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
                                <span className="text-muted-foreground">{t('passiveStatus')}</span>
                                <span>{t('passiveStatusInsufficient')}</span>
                                <span className="text-muted-foreground">{t('passiveVersion')}</span>
                                <span className="tabular-nums">{passiveBadge.summary.version}</span>
                                {passiveBadge.summary.language && (
                                    <>
                                        <span className="text-muted-foreground">{t('passiveLanguage')}</span>
                                        <span>{passiveBadge.summary.language}</span>
                                    </>
                                )}
                            </div>
                            {passiveBadge.summary.features && (
                                <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 border-t border-border pt-2">
                                    <span className="text-muted-foreground">{t('passiveFeatureChars')}</span>
                                    <span className="tabular-nums">{passiveBadge.summary.features.chars}</span>
                                    <span className="text-muted-foreground">{t('passiveFeatureLines')}</span>
                                    <span className="tabular-nums">{passiveBadge.summary.features.lines}</span>
                                    <span className="text-muted-foreground">{t('passiveFeatureParagraphs')}</span>
                                    <span className="tabular-nums">{passiveBadge.summary.features.paragraphs}</span>
                                    <span className="text-muted-foreground">{t('passiveFeatureCodeBlocks')}</span>
                                    <span className="tabular-nums">{passiveBadge.summary.features.code_blocks}</span>
                                    <span className="text-muted-foreground">{t('passiveFeatureCodeRatio')}</span>
                                    <span className="tabular-nums">{Number(passiveBadge.summary.features.code_ratio).toFixed(2)}</span>
                                    <span className="text-muted-foreground">{t('passiveFeatureMarkdownLines')}</span>
                                    <span className="tabular-nums">{passiveBadge.summary.features.markdown_lines}</span>
                                    {/* v2 新增两项。v1 旧行没有这两个键，此时整行不渲染——
                                        写"否"会把"没观测"说成"观测到没有"。 */}
                                    {typeof passiveBadge.summary.features.upstream_fingerprint_present === 'boolean' && (
                                        <>
                                            <span className="text-muted-foreground">{t('passiveFeatureFingerprint')}</span>
                                            <span>
                                                {passiveBadge.summary.features.upstream_fingerprint_present
                                                    ? t('passiveFingerprintPresent')
                                                    : t('passiveFingerprintAbsent')}
                                            </span>
                                        </>
                                    )}
                                    {passiveBadge.summary.features.service_tier && (
                                        <>
                                            <span className="text-muted-foreground">{t('passiveFeatureServiceTier')}</span>
                                            <span className="font-mono">{passiveBadge.summary.features.service_tier}</span>
                                        </>
                                    )}
                                </div>
                            )}
                        </>
                    )}
                    {passiveBadge.state === 'not_applicable' && passiveBadge.summary && (
                        <div className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1">
                            <span className="text-muted-foreground">{t('passiveStatus')}</span>
                            <span>{t('passiveStatusNotApplicable')}</span>
                            <span className="text-muted-foreground">{t('passiveReason')}</span>
                            <span className="break-all">{passiveBadge.summary.reason}</span>
                        </div>
                    )}
                    {passiveBadge.state === 'missing' && (
                        <p className="text-[11px] leading-5 text-muted-foreground">{t('passiveLegacyHint')}</p>
                    )}
                </div>
            </PopoverContent>
        </Popover>
    ) : null;
    const detailsSlot = canViewDetails ? (
        <>
            <Eye className="size-3.5" />
            <span className="hidden md:inline">{t('openDetails')}</span>
        </>
    ) : null;

    return (
            <MorphingDialog open={detailOpen} onOpenChange={onDetailOpenChange}>
                <MorphingDialogTrigger
                    disabled={!canViewDetails}
                    className={cn(
                        "relative w-full overflow-hidden rounded-lg border bg-card text-left shadow-sm",
                        hasError
                            ? "border-destructive/40"
                            : hasPartialFailure
                                ? "border-amber-500/40"
                                : "border-border",
                    )}
                >
                    {/* 左侧状态色条：失败/重试的行一眼可辨，仅作点缀（不再整卡铺红/琥珀底）。 */}
                    <span
                        aria-hidden
                        className={cn(
                            "absolute inset-y-0 left-0 w-1",
                            hasError ? "bg-destructive" : hasPartialFailure ? "bg-amber-500" : "bg-emerald-500/50",
                        )}
                    />
                    {/* 暖纸精修：裸官方 Logo（auto 列）+ 内容列，桌面 pad16/gap14，手机 pad12/gap10，顶对齐 */}
                    <div
                        // 验收/排障锚点：让真机脚本能把「模型检测」徽标绑定到某一条确定的日志卡，
                        // 而不是全页面按文字找。日志 id 不是敏感字段（详情弹层里本来就显示它）。
                        // 必须挂在真实 DOM 节点上：MorphingDialogTrigger 只透传它解构过的
                        // props，挂在它上面会被静默丢掉、压根渲染不出来。
                        data-log-id={log.id}
                        className="p-3 md:p-4 grid grid-cols-[auto_1fr] gap-2.5 md:gap-3.5 items-start"
                    >
                        {/* Avatar 只接 size（IconAvatar 拿它写 inline 宽高，且 TS 里必填）：外层用 class
                            定 28/36，style 在 IconAvatar 里于 inline 宽高之后合并、可覆盖成 100%；
                            svg 的 width/height 是属性，被 [&_svg]:size-full 覆盖（CSS 胜过展示属性）。 */}
                        <div title={modelNameToDisplay} className="shrink-0 size-7 md:size-9 [&_svg]:size-full">
                            <ModelAvatar size={36} style={{ width: '100%', height: '100%' }} />
                        </div>
                        <div className="min-w-0 flex flex-col gap-2.5 md:gap-3">
                            {/* 路由头部：自然三分区两排 grid（在 LogRouteHeader 的 card 分支里）。
                                第 1 排 = 状态/救援/接口/路径 | 请求模型→渠道→发出名 | 流式·图钉；
                                第 2 排 = 下游客户端 | 回显证据·风险·模型检测 | 查看详情。
                                小屏两列并重排，不把流式/详情独占整行留下大空洞。 */}
                            <LogRouteHeader
                                variant="card"
                                log={log}
                                attempts={attempts}
                                brandColor={brandColor}
                                hasMultipleAttempts={hasMultipleAttempts}
                                StatusIcon={StatusIcon}
                                statusLabel={statusLabel}
                                statusToneClass={statusToneClass}
                                requestEndpointLabel={requestEndpointLabel}
                                endpointTitle={endpointTitle}
                                upstreamPaths={upstreamPaths}
                                upstreamPathTitle={upstreamPathTitle}
                                autoRescueSlot={autoRescueSlot}
                                clientSlot={clientSlot}
                                auditSlot={auditSlot}
                                passiveSlot={passiveSlot}
                                probeSlot={probeSlot}
                                sourceSlot={sourceSlot}
                                detailsSlot={detailsSlot}
                            />

                            {/* 第 2 行：紧凑摘要（组间可换行；关键数字 shrink-0 + nowrap，绝不只剩省略号） */}
                            <div className="flex min-w-0 flex-col gap-2.5 md:gap-3 text-xs tabular-nums text-muted-foreground">
                                <div data-log-row="3" className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">
                                {/* G1 时间与位置（时间/IP 可换行；分隔点与 IP 同组，不孤立） */}
                                <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
                                    <span className="inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap">
                                        <Clock className="size-3.5 shrink-0 text-muted-foreground" />
                                        <span>{formatTime(log.time)}</span>
                                    </span>
                                    {log.request_ip?.trim() && (
                                        <span className="inline-flex min-w-0 items-center gap-1.5">
                                            <span className="shrink-0 text-muted-foreground/60">·</span>
                                            <span className="min-w-0 break-all font-mono" title={log.request_ip.trim()}>{log.request_ip.trim()}</span>
                                        </span>
                                    )}
                                </div>

                                {/* G2 用量与推理（摘要允许换行，不截断） */}
                                {!isModelTest && (
                                    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
                                        <span className="min-w-0 break-words" title={formatTokensSummary(log)}>{formatTokensSummary(log)}</span>
                                        {reasoningEffort && (
                                            <span className="inline-flex shrink-0 items-center gap-1.5 whitespace-nowrap">
                                                <span className="text-muted-foreground/60">·</span>
                                                <Brain className="size-3.5 shrink-0 text-muted-foreground" />
                                                <span>{t('thinking')} {reasoningEffort}</span>
                                            </span>
                                        )}
                                    </div>
                                )}
                                {isModelTest && reasoningEffort && (
                                    <div className="flex min-w-0 shrink-0 items-center gap-1 whitespace-nowrap">
                                        <Brain className="size-3.5 shrink-0 text-muted-foreground" />
                                        <span>{t('thinking')} {reasoningEffort}</span>
                                    </div>
                                )}
                                </div>
                                <div data-log-row="4" className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1">

                                {/* G3 耗时与费用（每个数字小组 shrink-0 + nowrap，关键值绝不只剩省略号） */}
                                <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
                                    <span
                                        className={cn("inline-flex shrink-0 items-center gap-1 whitespace-nowrap font-medium", latencyTextColor(log.ftut, 1500, 4000))}
                                        title={`${t('firstToken')} ${Number.isFinite(log.ftut) && log.ftut > 0 ? formatDuration(log.ftut) : '--'}`}
                                    >
                                        <Zap className="size-3.5 shrink-0" />
                                        <span>{t('firstToken')} {Number.isFinite(log.ftut) && log.ftut > 0 ? formatDuration(log.ftut) : '--'}</span>
                                    </span>
                                    <span className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap">
                                        <span className="text-muted-foreground/60">·</span>
                                        <span
                                            className={cn("inline-flex items-center gap-1 font-medium", latencyTextColor(log.use_time, 5000, 15000))}
                                            title={`${t('totalTime')} ${formatDuration(log.use_time)}`}
                                        >
                                            <Clock className="size-3.5 shrink-0 text-muted-foreground" />
                                            <span>{t('totalTime')} {formatDuration(log.use_time)}</span>
                                        </span>
                                    </span>
                                    {/* 生成速度（token/s）：算不出来就整块不渲染，别留空占位 */}
                                    {tpsReading !== null && (
                                        <span className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap">
                                            <span className="text-muted-foreground/60">·</span>
                                            <span
                                                className="inline-flex items-center gap-1 font-medium tabular-nums"
                                                title={`${t('tps')} ${tpsReading.text} ${t('tpsUnit')} · ${t('tpsHintGen')}`}
                                            >
                                                <Gauge className="size-3.5 shrink-0 text-muted-foreground" />
                                                <span>{t('tps')} {tpsReading.text} {t('tpsUnit')}</span>
                                            </span>
                                        </span>
                                    )}
                                    {!isModelTest && (
                                        <span className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap">
                                            <span className="text-muted-foreground/60">·</span>
                                            <span
                                                className="inline-flex items-center gap-1 font-medium tabular-nums text-emerald-600 dark:text-emerald-400"
                                                title={`$${Number(log.cost || 0).toFixed(6)}`}
                                            >
                                                <DollarSign className="size-3.5 shrink-0 text-emerald-500" />
                                                <span>{Number(log.cost) > 0 ? `$${Number(log.cost).toFixed(6)}` : '$0.000000'}</span>
                                            </span>
                                        </span>
                                    )}
                                </div>

                                {/* G4 身份（分隔点与用户同组，不孤立） */}
                                {(requestAPIKeyName || (canViewDetails && userName)) && (
                                    <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-0.5">
                                        {requestAPIKeyName && (
                                            <span className="inline-flex shrink-0 items-center gap-1 whitespace-nowrap">
                                                <KeyRound className="size-3.5 shrink-0 text-muted-foreground" />
                                                <MonoSafeText value={maskSensitive(requestAPIKeyName, sensitiveVisible)} className="max-w-[140px] truncate" />
                                            </span>
                                        )}
                                        {canViewDetails && userName && (
                                            <span className="inline-flex min-w-0 items-center gap-1">
                                                {requestAPIKeyName && (
                                                    <span className="shrink-0 text-muted-foreground/60">·</span>
                                                )}
                                                <User className="size-3.5 shrink-0 text-muted-foreground" />
                                                <span className="min-w-0 max-w-[120px] truncate" title={userName}>{userName}</span>
                                            </span>
                                        )}
                                    </div>
                                )}
                                </div>
                            </div>

                            {/* 错误/警告叙述框：按用户要求移除（详情弹窗里仍有完整 verdict） */}
                        </div>
                    </div>
                </MorphingDialogTrigger>

                <MorphingDialogContainer>
                    <MorphingDialogContent className="relative flex h-[calc(100dvh-1rem)] w-[calc(100vw-1rem)] max-w-[calc(100vw-1rem)] flex-col overflow-hidden rounded-3xl bg-card px-4 py-4 text-card-foreground md:w-[80vw] md:max-w-6xl md:px-6">
                        <MorphingDialogClose className="top-4 right-5 text-muted-foreground hover:text-foreground transition-colors" />
                        <MorphingDialogTitle className="mb-3 flex min-w-0 flex-wrap items-center gap-2 pr-9 text-sm">
                            <ModelAvatar size={28} />
                            <LogRouteHeader
                                variant="detail"
                                log={log}
                                attempts={attempts}
                                brandColor={brandColor}
                                hasMultipleAttempts={hasMultipleAttempts}
                                StatusIcon={StatusIcon}
                                statusLabel={statusLabel}
                                statusToneClass={statusToneClass}
                                requestEndpointLabel={requestEndpointLabel}
                                endpointTitle={endpointTitle}
                                upstreamPaths={upstreamPaths}
                                upstreamPathTitle={upstreamPathTitle}
                                passiveSlot={passiveSlot}
                                probeSlot={probeSlot}
                                sourceSlot={sourceSlot}
                            />
                            {onNavigate ? (
                                // 键盘能翻，但用户不会自己发现；给两个按钮把能力摆出来。
                                <span className="ml-auto flex shrink-0 items-center gap-1">
                                    <button
                                        type="button"
                                        onClick={() => onNavigate(-1)}
                                        disabled={!canNavigatePrev}
                                        aria-label="上一条（更新）"
                                        title="上一条（← 键）"
                                        className="grid size-8 place-items-center rounded-lg border border-border bg-background text-muted-foreground transition-colors hover:text-foreground disabled:pointer-events-none disabled:opacity-30 max-sm:size-11"
                                    >
                                        <ChevronLeft className="size-4" />
                                    </button>
                                    <button
                                        type="button"
                                        onClick={() => onNavigate(1)}
                                        disabled={!canNavigateNext}
                                        aria-label="下一条（更旧）"
                                        title="下一条（→ 键）"
                                        className="grid size-8 place-items-center rounded-lg border border-border bg-background text-muted-foreground transition-colors hover:text-foreground disabled:pointer-events-none disabled:opacity-30 max-sm:size-11"
                                    >
                                        <ChevronRight className="size-4" />
                                    </button>
                                </span>
                            ) : null}
                        </MorphingDialogTitle>

                        <MorphingDialogDescription className="flex-1 min-h-0 overflow-y-auto">
                            {/* 单一滚动源=本层。正常情况内部 min-h-full + flex-1 布局撑满、不出滚动条；
                                当徽标行/诊断区把纵向空间挤爆时（多行 tiles、展开的错误详情），内容
                                自然下沉超高 → 这里滚动，而不是像旧版 md:overflow-hidden 那样把
                                请求/响应面板和底部信息硬裁掉看不到。 */}
                            {/* md:min-h-full（而非 h-full）：内容装得下时撑满弹窗（flex-1 面板占满剩余高度），
                                装不下时容器随内容长高 → 外层 description 出滚动条。旧版 h-full 把容器高度
                                钉死，纵向不够时 flex 只会把请求/响应面板压到 0 高，底部内容"被遮住"且无法滚动。 */}
                            <div className="flex flex-col gap-4 md:min-h-full">
                                <div className={cn(
                                    "shrink-0 rounded-xl border px-3.5 py-2.5 text-sm font-medium",
                                    verdict.kind === 'success'
                                        ? "border-emerald-500/30 bg-emerald-500/10 text-emerald-700 dark:text-emerald-300"
                                        : verdict.kind === 'warn'
                                            ? "border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300"
                                            : "border-destructive/30 bg-destructive/10 text-destructive",
                                )}>
                                    <SafeText mode="wrap" value={verdict.text} className="block" />
                                </div>
                                <ProbeEvidenceList probes={probes} />
                                {/* 检测过（有 verdict）才拉审计报告。否则这是一次纯白拉：
                                    详情接口会把三份 body 全传一遍——1.4MB 的日志等于拉两遍。 */}
                                {log.model_audit_verdict ? <DetailAuditReport logId={log.id} /> : null}
                                <div className="grid min-w-0 grid-cols-2 gap-2 md:grid-cols-6">
                                    <DetailTile icon={<Hash className="size-3.5" />} label={t('logId')}>
                                        <MonoSafeText mode="wrap" value={String(log.id)} className="block text-xs text-foreground" />
                                    </DetailTile>
                                    {endpointLines.length > 0 && (
                                        <DetailTile icon={<MessageSquare className="size-3.5" />} label={t('endpoint')}>
                                            <div className="flex min-w-0 flex-col gap-1">
                                                {endpointLines.map((line) => (
                                                    <MonoSafeText
                                                        key={line.label}
                                                        mode="wrap"
                                                        value={`${line.label}：${line.value}`}
                                                        className="block text-xs text-foreground"
                                                    />
                                                ))}
                                            </div>
                                        </DetailTile>
                                    )}
                                    {requestAPIKeyName && (
                                        <DetailTile icon={<KeyRound className="size-3.5" />} label="API Key">
                                            <MonoSafeText mode="wrap" value={maskSensitive(requestAPIKeyName, sensitiveVisible)} className="block text-xs text-foreground" />
                                        </DetailTile>
                                    )}
                                    {log.request_user_agent && (
                                        <DetailTile icon={<User className="size-3.5" />} label={t('client')}>
                                            <MonoSafeText mode="wrap" value={stripUrls(log.request_user_agent)} className="block text-xs text-foreground" />
                                        </DetailTile>
                                    )}
                                    {userName && (
                                        <DetailTile icon={<User className="size-3.5" />} label={t('userName')}>
                                            <SafeText mode="wrap" value={userName} className="block text-xs text-foreground" />
                                        </DetailTile>
                                    )}
                                    {channelKeyRemark && (
                                        <DetailTile icon={<KeyRound className="size-3.5" />} label={t('channelKey')}>
                                            <MonoSafeText mode="wrap" value={maskSensitive(channelKeyRemark, sensitiveVisible)} className="block text-xs text-foreground" />
                                        </DetailTile>
                                    )}
                                    {reasoningEffort && (
                                        <DetailTile icon={<Zap className="size-3.5" />} label={t('reasoningEffort')}>
                                            <SafeText mode="wrap" value={reasoningEffort} className="block text-xs font-semibold text-foreground" />
                                        </DetailTile>
                                    )}
                                    {log.upstream_response_model?.trim() && (
                                        <DetailTile icon={<Cpu className="size-3.5" />} label={t('upstreamEcho')}>
                                            <SafeText
                                                mode="wrap"
                                                value={log.upstream_response_model.trim()}
                                                className={cn(
                                                    "block text-xs font-semibold",
                                                    log.upstream_model_mismatch === true
                                                        ? "text-amber-800 dark:text-amber-200"
                                                        : "text-foreground",
                                                )}
                                            />
                                        </DetailTile>
                                    )}
                                    {log.request_ip && (
                                        <DetailTile icon={<MapPin className="size-3.5" />} label={t('requestIP')}>
                                            <div className="flex items-center gap-1.5">
                                                <MonoSafeText mode="wrap" value={log.request_ip} className="block text-xs text-foreground" />
                                                <Badge variant="outline" className="shrink-0 border-border/60 px-1 py-0 text-[10px] text-muted-foreground">
                                                    {ipFamilyLabel(log.request_ip)}
                                                </Badge>
                                            </div>
                                        </DetailTile>
                                    )}
                                    {(() => {
                                        // 出站路由（管理员）：走直连还是哪个代理。取"最终尝试"(最后成功、
                                        // 否则最后一次)的路由，与顶部结果所反映的渠道一致。用户日志无 attempts，
                                        // 自然不显示。
                                        if (attempts.length === 0) return null;
                                        let finalIdx = attempts.length - 1;
                                        for (let i = attempts.length - 1; i >= 0; i--) {
                                            if (attempts[i].status === 'success') { finalIdx = i; break; }
                                        }
                                        return (
                                            <DetailTile icon={<ArrowUpFromLine className="size-3.5" />} label="出站路由">
                                                <MonoSafeText mode="wrap" value={attemptRouteLabel(attempts[finalIdx])} className="block text-xs text-foreground" />
                                            </DetailTile>
                                        );
                                    })()}
                                    <DetailTile icon={<StatusIcon className="size-3.5" />} label={t('auditStatus')}>
                                        <SafeText
                                            mode="wrap"
                                            value={statusLabel}
                                            className={cn("block text-xs font-semibold", statusTextClass)}
                                        />
                                    </DetailTile>
                                    {upstreamPaths.length > 0 && (
                                        <DetailTile icon={<ArrowRight className="size-3.5" />} label="上游路径">
                                            <div className="flex min-w-0 flex-col gap-1">
                                                {upstreamPaths.map((path) => (
                                                    <MonoSafeText
                                                        key={path}
                                                        mode="wrap"
                                                        value={path}
                                                        className="block text-xs text-foreground"
                                                    />
                                                ))}
                                            </div>
                                        </DetailTile>
                                    )}
                                    {humanErrorCode && (
                                        <DetailTile icon={<AlertCircle className="size-3.5" />} label="错误原因">
                                            <SafeText mode="wrap" value={humanErrorCode} className="block text-xs font-semibold text-destructive" />
                                        </DetailTile>
                                    )}
                                    <DetailTile icon={<RotateCw className="size-3.5" />} label={t('channelAttempts')}>
                                        <MonoSafeText mode="wrap" value={String(attemptCount)} className="block text-xs font-semibold text-foreground" />
                                    </DetailTile>
                                    <DetailTile icon={<Pin className="size-3.5" />} label="会话粘连">
                                        <SafeText mode="wrap" value={log.route_sticky_hit ? '命中（沿用上次渠道）' : '未命中'} className="block text-xs font-semibold text-foreground" />
                                    </DetailTile>
                                    {!isModelTest && humanUsageSource && (
                                        <DetailTile icon={<Cpu className="size-3.5" />} label="用量来源">
                                            <SafeText mode="wrap" value={humanUsageSource} className="block text-xs font-semibold text-foreground" />
                                        </DetailTile>
                                    )}
                                    {!isModelTest && humanUsageReason && (
                                        <DetailTile icon={<AlertCircle className="size-3.5" />} label="无用量原因">
                                            <SafeText mode="wrap" value={humanUsageReason} className="block text-xs text-amber-700 dark:text-amber-300" />
                                        </DetailTile>
                                    )}
                                    {!isModelTest && (
                                        <DetailTile icon={<Percent className="size-3.5" />} label={t('cacheHit')}>
                                            <MonoSafeText mode="wrap" value={formatCacheRate(log.cache_hit_rate)} className="block text-xs font-semibold text-cyan-600 dark:text-cyan-400" />
                                        </DetailTile>
                                    )}
                                    {!isModelTest && (
                                        <DetailTile icon={<DollarSign className="size-3.5" />} label={t('cost')}>
                                            <MonoSafeText mode="wrap" value={Number(log.cost).toFixed(6)} className="block whitespace-nowrap text-xs font-semibold tabular-nums text-emerald-600 dark:text-emerald-400" />
                                        </DetailTile>
                                    )}
                                    {!isModelTest && ((log.base_input_price !== undefined && log.base_input_price > 0) || (log.base_output_price !== undefined && log.base_output_price > 0)) && (
                                        <DetailTile icon={<DollarSign className="size-3.5" />} label={t('basePrice')}>
                                            <MonoSafeText mode="wrap" value={`$${(log.base_input_price ?? 0).toFixed(2)} / $${(log.base_output_price ?? 0).toFixed(2)} / 1M`} className="block whitespace-nowrap text-xs font-semibold tabular-nums text-muted-foreground" />
                                        </DetailTile>
                                    )}
                                </div>
                                {(hasError || hasPartialFailure || shouldShowAttempts) && (
                                    <div className={cn(
                                        "flex-initial min-h-0 flex flex-col rounded-lg border overflow-hidden max-h-64 md:max-h-[40%]",
                                        hasError
                                            ? "bg-destructive/5 border-destructive/20"
                                            : hasPartialFailure
                                                ? "bg-amber-500/5 border-amber-500/20"
                                            : "bg-secondary/30 border-border/50"
                                    )}>
                                        <div
                                            className={cn(
                                                "flex items-center gap-2 px-3 py-2.5 shrink-0 cursor-pointer select-none hover:bg-muted/50 transition-colors",
                                                hasError && "hover:bg-destructive/10",
                                                hasPartialFailure && "hover:bg-amber-500/10"
                                            )}
                                            onClick={() => setIsDiagnosticExpanded(!isDiagnosticExpanded)}
                                        >
                                            {hasError ? (
                                                <AlertCircle className="size-4 text-destructive" />
                                            ) : hasPartialFailure ? (
                                                <AlertCircle className="size-4 text-amber-600 dark:text-amber-300" />
                                            ) : (
                                                <RotateCw className="size-4 text-muted-foreground" />
                                            )}
                                            <span className={cn(
                                                "text-sm font-medium",
                                                hasError ? "text-destructive" : hasPartialFailure ? "text-amber-700 dark:text-amber-300" : "text-secondary-foreground"
                                            )}>
                                                {hasError ? t('errorInfo') : hasPartialFailure ? t('warningInfo') : t('retryDetails')}
                                            </span>
                                            <div className="ml-auto flex min-w-0 items-center gap-2">
                                                {hasMultipleAttempts && (
                                                    <Badge
                                                        variant="outline"
                                                        className={cn(
                                                            "text-xs border-0",
                                                            hasError
                                                                ? "bg-destructive/10 text-destructive"
                                                                : hasPartialFailure
                                                                    ? "bg-amber-500/10 text-amber-700 dark:text-amber-300"
                                                                : "bg-secondary text-secondary-foreground"
                                                        )}
                                                    >
                                                        {attemptCount} {t('attempts')}
                                                    </Badge>
                                                )}
                                                {isDiagnosticExpanded ? (
                                                    <ChevronUp className="size-4 text-muted-foreground" />
                                                ) : (
                                                    <ChevronDown className="size-4 text-muted-foreground" />
                                                )}
                                            </div>
                                        </div>

                                        <AnimatePresence initial={false}>
                                            {isDiagnosticExpanded && (
                                                <motion.div
                                                    initial={{ height: 0, opacity: 0 }}
                                                    animate={{ height: "auto", opacity: 1 }}
                                                    exit={{ height: 0, opacity: 0 }}
                                                    transition={{ duration: 0.2, ease: "easeInOut" }}
                                                    className="overflow-hidden flex flex-col min-h-0"
                                                >
                                                    <div className="flex-1 overflow-auto p-2.5 md:p-3 flex flex-col gap-4">
                                                        {hasError && (
                                                            <div className="relative min-w-0 pl-1">
                                                                <div className="absolute right-0 top-0">
                                                                    <CopyIconButton
                                                                        text={log.error ?? ''}
                                                                        className="p-1 rounded-md text-destructive/60 hover:text-destructive hover:bg-destructive/10 transition-colors"
                                                                        copyIconClassName="size-4"
                                                                        checkIconClassName="size-4"
                                                                    />
                                                                </div>
                                                                <ErrorSafeText value={log.error} className="block pr-8 text-sm" />
                                                            </div>
                                                        )}

                                                        {shouldShowAttempts && (
                                                            <div className="flex min-w-0 flex-col gap-2">
                                                                {attempts.map((attempt, idx) => (
                                                                    <div
                                                                        key={idx}
                                                                        className={cn(
                                                                            "text-xs p-2.5 rounded-xl border transition-colors flex min-w-0 flex-col gap-2",
                                                                            attempt.status === 'success'
                                                                                ? "bg-primary/5 border-primary/20 hover:bg-primary/10"
                                                                                : "bg-destructive/5 border-destructive/20 hover:bg-destructive/10"
                                                                        )}
                                                                    >
                                                                        <div className="flex min-w-0 flex-col gap-1 sm:flex-row sm:items-start sm:gap-2">
                                                                            <Badge
                                                                                className={cn(
                                                                                    "h-5 w-fit shrink-0 px-1.5 text-[10px] font-bold uppercase shadow-none border-0",
                                                                                    attemptStatusBadgeClass(attempt.status)
                                                                                )}
                                                                            >
                                                                                {formatAttemptStatus(attempt.status, t)}
                                                                            </Badge>
                                                                            <SafeText
                                                                                mode="wrap"
                                                                                value={attempt.channel_name}
                                                                                className="min-w-0 text-xs font-semibold text-foreground sm:flex-1"
                                                                            />
                                                                            <div className="flex min-w-0 items-center gap-1 sm:flex-1">
                                                                                {(() => {
                                                                                    const modelName = attempt.model_name || log.actual_model_name || log.request_model_name || '';
                                                                                    const { Avatar: AttemptAvatar } = getModelIcon(modelName);
                                                                                    return AttemptAvatar ? <AttemptAvatar size={14} className="shrink-0" /> : null;
                                                                                })()}
                                                                                <MonoSafeText
                                                                                    mode="wrap"
                                                                                    value={marketModelName(attempt.model_name)}
                                                                                    className="min-w-0 text-[11px] text-muted-foreground"
                                                                                />
                                                                            </div>
                                                                            {attempt.upstream_path && (
                                                                                <MonoSafeText
                                                                                    mode="wrap"
                                                                                    value={attempt.upstream_path}
                                                                                    className="min-w-0 text-[11px] text-muted-foreground sm:flex-1"
                                                                                />
                                                                            )}
                                                                            <MonoSafeText
                                                                                value={`#${attempt.attempt_num || idx + 1} - ${formatDuration(attempt.duration)}`}
                                                                                className="shrink-0 text-[11px] text-muted-foreground"
                                                                            />
                                                                        </div>
                                                                        {attempt.msg && (
                                                                            <ErrorSafeText value={attempt.msg} className="block min-w-0 border-l-2 border-destructive/30 pl-2 text-[11px] text-destructive/90" />
                                                                        )}
                                                                    </div>
                                                                ))}
                                                            </div>
                                                        )}
                                                    </div>
                                                </motion.div>
                                            )}
                                        </AnimatePresence>
                                    </div>
                                )}
                                {techMeta.length > 0 && (
                                    <div className="shrink-0 rounded-lg border border-border/50 bg-muted/20 overflow-hidden">
                                        <div
                                            className="flex cursor-pointer select-none items-center gap-2 px-3 py-2.5 transition-colors hover:bg-muted/50"
                                            onClick={() => setIsTechExpanded(!isTechExpanded)}
                                        >
                                            <AlertCircle className="size-4 text-muted-foreground" />
                                            <span className="text-sm font-medium text-muted-foreground">技术详情（给技术同学排查用）</span>
                                            <div className="ml-auto">
                                                {isTechExpanded ? (
                                                    <ChevronUp className="size-4 text-muted-foreground" />
                                                ) : (
                                                    <ChevronDown className="size-4 text-muted-foreground" />
                                                )}
                                            </div>
                                        </div>
                                        <AnimatePresence initial={false}>
                                            {isTechExpanded && (
                                                <motion.div
                                                    initial={{ height: 0, opacity: 0 }}
                                                    animate={{ height: "auto", opacity: 1 }}
                                                    exit={{ height: 0, opacity: 0 }}
                                                    transition={{ duration: 0.2, ease: "easeInOut" }}
                                                    className="overflow-hidden"
                                                >
                                                    <div className="grid grid-cols-1 gap-2 p-2.5 sm:grid-cols-2">
                                                        {techMeta.map((item) => (
                                                            <div key={item.label} className="min-w-0 rounded-lg border border-border/60 bg-card/50 px-2.5 py-1.5">
                                                                <div className="text-[10px] uppercase tracking-wide text-muted-foreground">{item.label}</div>
                                                                <MonoSafeText mode="wrap" value={item.value} className="mt-0.5 block text-[11px] text-foreground/80" />
                                                            </div>
                                                        ))}
                                                    </div>
                                                </motion.div>
                                            )}
                                        </AnimatePresence>
                                    </div>
                                )}
                                {/* 移动端：外层 description 已 overflow-y-auto，此区域自然高度不约束；
                                    桌面端 flex-[1_1_55dvh]：空间富余时从 55dvh 基准长高填满剩余（同旧 flex-1 观感），
                                    上方内容挤压时最多缩到 20rem 保底——请求/响应面板永远保有可用高度做内部滚动，
                                    再挤就由外层 description 滚动兜底，不再出现被压成 0 高 / 底部裁掉。 */}
                                <div className="overflow-hidden md:flex-[1_1_55dvh] md:min-h-[20rem]">
                                    <LazyLogBodies
                                        logId={log.id}
                                        fallbackRequest={log.request_content}
                                        fallbackResponse={log.response_content}
                                        fallbackUpstreamRequest={log.upstream_request_body}
                                        requestLabel={t('requestContent')}
                                        responseLabel={t('responseContent')}
                                        upstreamRequestLabel={t('upstreamRequestBody')}
                                        upstreamRequestHint={t('upstreamRequestBodyHint')}
                                        tokensLabel={t('tokens')}
                                        cacheHitLabel={t('cacheHit')}
                                        noRequestText={t('noRequestContent')}
                                        noResponseText={t('noResponseContent')}
                                        inputTokens={log.input_tokens}
                                        outputTokens={log.output_tokens}
                                        cacheHitTokens={log.cache_hit_tokens ?? 0}
                                        isModelTest={isModelTest}
                                    />
                                </div>
                            </div>
                        </MorphingDialogDescription>

                        <div className="mt-auto flex max-h-24 min-w-0 shrink-0 flex-wrap items-center gap-3 overflow-y-auto pt-4 text-xs text-muted-foreground md:max-h-none md:gap-4 md:overflow-visible">
                            <div className="flex items-center gap-1.5">
                                <Clock className="size-3.5 text-muted-foreground" />
                                <span className="tabular-nums">{formatTime(log.time)}</span>
                            </div>
                            {requestAPIKeyName && (
                                <div className="flex min-w-0 items-center gap-1.5">
                                    <KeyRound className="size-3.5 shrink-0 text-muted-foreground" />
                                    <MonoSafeText value={maskSensitive(requestAPIKeyName, sensitiveVisible)} className="text-muted-foreground" />
                                </div>
                            )}
                            <div className="flex items-center gap-1.5">
                                <Zap className="size-3.5 text-muted-foreground" />
                                <span className="min-w-0 break-words">{t('firstTokenTime')}: {formatDuration(log.ftut)}</span>
                            </div>
                            <div className="flex items-center gap-1.5">
                                <Cpu className="size-3.5 text-muted-foreground" />
                                <span className="min-w-0 break-words">{t('totalTime')}: {formatDuration(log.use_time)}</span>
                            </div>
                            {!isModelTest && (
                                <div className="flex items-center gap-1.5">
                                    <Percent className="size-3.5 text-muted-foreground" />
                                    <span className="min-w-0 break-words">
                                        {t('cacheHit')}: {(log.cache_hit_tokens ?? 0).toLocaleString()} / {formatCacheRate(log.cache_hit_rate)}
                                        {(log.cache_write_tokens ?? 0) > 0 && ` · ${t('cacheWrite')}: ${log.cache_write_tokens.toLocaleString()}`}
                                        {(log.cache_input_tokens ?? 0) > 0 && ` · ${t('cacheInput')}: ${log.cache_input_tokens.toLocaleString()}`}
                                    </span>
                                </div>
                            )}
                            {!isModelTest && (
                                <div className="flex items-center gap-1.5">
                                    <DollarSign className="size-3.5 text-emerald-500" />
                                    <span className="whitespace-nowrap font-medium tabular-nums text-emerald-600 dark:text-emerald-400">
                                        {t('cost')}: {Number(log.cost).toFixed(6)}
                                    </span>
                                </div>
                            )}
                        </div>
                    </MorphingDialogContent>
                </MorphingDialogContainer>
            </MorphingDialog>
    );
}, (prevProps, nextProps) => (
    prevProps.log.id === nextProps.log.id
    && prevProps.log.time === nextProps.log.time
    // 本行审计字段随后端回写而变化时（手动检测带 log_id 写回同一行、id/time 不变），
    // 必须让卡片重绘，否则「模型检测」徽标不会出现/不会更新。
    && prevProps.log.model_audit_verdict === nextProps.log.model_audit_verdict
    && prevProps.log.model_audit_trigger === nextProps.log.model_audit_trigger
    && prevProps.log.model_audit_score === nextProps.log.model_audit_score
    && prevProps.log.model_audit_finding_n === nextProps.log.model_audit_finding_n
    // 被动摘要随后端回写而变化时必须重绘（字段在同一 log 对象上，引用比较即可）。
    && prevProps.log.passive_summary === nextProps.log.passive_summary
));
