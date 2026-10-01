'use client';

import React, { useMemo, useState, useEffect, useCallback, type ReactNode } from 'react';
import { Brain, Clock, Cpu, Zap, AlertCircle, ArrowDownToLine, ArrowUpFromLine, DollarSign, ArrowRight, ArrowDown, ArrowUp, Send, MessageSquare, Loader2, RotateCw, ChevronDown, ChevronUp, Pin, KeyRound, Percent, CheckCircle2, XCircle, Eye, Hash, MapPin, User, type LucideIcon } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { motion, AnimatePresence } from 'motion/react';
import JsonView from '@uiw/react-json-view';
import { githubDarkTheme } from '@uiw/react-json-view/githubDark';
import { githubLightTheme } from '@uiw/react-json-view/githubLight';
import { useTheme } from 'next-themes';
import { create } from 'zustand';
import { fetchLogById, getRelayLogSeverity, type RelayLog, type ChannelAttempt } from '@/api/endpoints/log';
import type { ScheduledAuditResult } from '@/api/endpoints/model-audit';
import {
    getLogVerdict,
    humanizeClient,
    humanizeErrorCode,
    humanizeUsageSource,
    humanizeUsageReason,
    humanizeEndpoint,
    isModelTestEndpoint,
} from './humanize';
import { getModelIcon } from '@/lib/model-icons';
import { Popover, PopoverContent, PopoverTrigger } from '@/components/ui/popover';
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

function formatAttemptStatus(status: ChannelAttempt['status'], successLabel: string, failedLabel: string): string {
    if (status === 'success') return successLabel;
    if (status === 'failed') return failedLabel;
    return status.replace(/_/g, ' ');
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
                    onKeyDown={(e) => e.stopPropagation()}
                    className="min-w-0 max-w-[12rem] cursor-pointer px-1.5 py-0 text-xs"
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
                                        attempt.status === 'success'
                                            ? "bg-primary/15 text-primary"
                                            : "bg-destructive/15 text-destructive"
                                    )}
                                >
                                    {formatAttemptStatus(attempt.status, t('success'), t('failed'))}
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
    /** card 专用槽位：B1 尾部的「自动救援」徽标（路由状态）。detail 忽略。 */
    autoRescueSlot?: ReactNode;
    /** card 专用槽位：A2 下游客户端徽标（含 Popover）。缺省时渲染低强调占位。 */
    clientSlot?: ReactNode;
    /** card 专用槽位：B2 模型检测徽标。无结果时传 null，不造假占位。 */
    auditSlot?: ReactNode;
    /** card 专用槽位：C2「查看详情」。detail 忽略。 */
    detailsSlot?: ReactNode;
}

/**
 * 日志「路由头部」：状态 / 接口 / 上游路径徽标 → request_model_name → 渠道（多尝试展开
 * RetryBadgeWithPopover，否则渠道 Badge）→ actual_model_name → 可能的 stream / sticky 徽标。
 * 卡片列表头部与详情弹窗标题渲染同一套元素，只差尺寸与换行，用 variant 收敛：
 * card 走「固定 3 列 × 2 行列轨道」grid（见下方 isCard 分支），detail 标题区仍是可换行 flex。
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
    detailsSlot,
}: LogRouteHeaderProps) {
    const t = useTranslations('log.card');
    const isCard = variant === 'card';
    const textMode = isCard ? undefined : 'wrap';
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
        <SafeText
            mode={textMode}
            value={requestModelDisplayName}
            title={requestModelDisplayName === log.request_model_name ? undefined : log.request_model_name}
            className="font-semibold text-card-foreground"
        />
    );
    /* Channel identity is admin-only: a normal user's log carries no
       channel_name (see RelayLogUserSummary), so the header degrades to
       request model → actual model without ever revealing the upstream. */
    const channelGroup = log.channel_name ? (
        <>
            <ArrowRight className={cn("size-3.5 text-muted-foreground/50", isCard && "shrink-0")} />
            {hasMultipleAttempts ? (
                <RetryBadgeWithPopover
                    channelName={log.channel_name}
                    brandColor={brandColor}
                    attempts={attempts}
                />
            ) : (
                <Badge
                    variant="secondary"
                    className="min-w-0 max-w-[12rem] px-1.5 py-0 text-xs"
                    style={{ backgroundColor: `${brandColor}15`, color: brandColor }}
                >
                    <SafeText value={log.channel_name} className="text-xs" />
                </Badge>
            )}
        </>
    ) : null;
    const actualModelText = (
        <SafeText
            mode={textMode}
            value={actualModelDisplayName}
            title={actualModelDisplayName === log.actual_model_name ? undefined : log.actual_model_name}
            className="text-muted-foreground"
        />
    );
    // 上游自报名 + 「回显不符」徽标：只在上游自报与发出去的不一致时整组出现。
    const echoGroup = log.upstream_model_mismatch === true ? (
        <>
            {upstreamEchoDisplayName && (
                <>
                    <ArrowRight className={cn("size-3.5 text-amber-600/70 dark:text-amber-400/70", isCard && "shrink-0")} />
                    <SafeText
                        mode={textMode}
                        value={upstreamEchoDisplayName}
                        title={upstreamEchoDisplayName === upstreamEchoName
                            ? t('echoMismatchHint', { model: upstreamEchoName })
                            : `${upstreamEchoName}\n${t('echoMismatchHint', { model: upstreamEchoName })}`}
                        className="text-amber-800 dark:text-amber-200"
                    />
                </>
            )}
            {/* 「回显不符」徽标本行证据（发送模型 vs 上游自报）点按可见。卡片外层是
                MorphingDialogTrigger（自带 onClick/onKeyDown），徽标必须就地掐断冒泡，
                否则点徽标会连带打开整卡详情——与上面「客户端 / 检测」徽标同款写法。 */}
            <Popover>
                <PopoverTrigger asChild>
                    <Badge
                        variant="outline"
                        role="button"
                        tabIndex={0}
                        onClick={(e) => e.stopPropagation()}
                        onKeyDown={(e) => e.stopPropagation()}
                        className={cn(
                            "shrink-0 cursor-pointer border-amber-500/40 bg-amber-500/10 text-xs text-amber-800 dark:text-amber-200",
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
                        <span className="break-all">{actualModelDisplayName || requestModelDisplayName}</span>
                        <span className="text-muted-foreground">{t('upstreamEcho')}</span>
                        <span className="break-all text-amber-700 dark:text-amber-300">
                            {upstreamEchoDisplayName || t('upstreamSilent')}
                        </span>
                    </div>
                    {upstreamEchoName && (
                        <p className="mt-2 border-t border-border pt-2 text-[11px] leading-5 text-muted-foreground">
                            {t('echoMismatchHint', { model: upstreamEchoName })}
                        </p>
                    )}
                </PopoverContent>
            </Popover>
        </>
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
         * 固定列轨道（行落强制）：一个 grid + 6 个子元素，row-major 填进 3 列 × 2 行。
         * md 及以上 `auto | minmax(0,1fr) | auto`——上下两排共享列宽，所以「状态」永远压在
         * 「客户端」上面、「流式」永远压在「查看详情」上面，不再由 flex-wrap 按剩余宽度决定谁换行。
         * md 以下退化成单列自然堆叠，顺序仍是 A1→B1→C1→A2→B2（流式排在客户端之前）。
         * 所有单元格 min-w-0：长文本交给 SafeText/MonoSafeText 截断，整卡不产生横向滚动。
         */
        return (
            <div className="grid min-w-0 gap-x-4 gap-y-1 text-sm md:grid-cols-[auto_minmax(0,1fr)_auto] md:items-center">
                {/* A1 状态 · 接口 · 上游路径 */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {statusBadge}
                    {endpointBadge}
                    {upstreamPathBadge}
                </div>
                {/* B1 请求模型 → 渠道 · 实际模型 · 自动救援（路由状态收在这一格尾部） */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {requestModelText}
                    {channelGroup}
                    {actualModelText}
                    {autoRescueSlot}
                </div>
                {/* C1 流式 · 粘性图钉（固定右列） */}
                <div className="flex min-w-0 items-center gap-2 md:justify-self-end">
                    {streamBadge}
                    {stickyPin}
                </div>
                {/* A2 下游客户端；缺失时给低强调占位，「第 2 排永远渲染」让列轨道恒定 */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {clientSlot ?? (
                        <Badge
                            variant="outline"
                            className="shrink-0 border-border/60 bg-muted/30 px-1.5 py-0 text-xs font-normal text-muted-foreground"
                        >
                            {t('clientUnknown')}
                        </Badge>
                    )}
                </div>
                {/* B2 上游回显 · 回显不符 · 模型检测（检测无结果就留空，不造假占位） */}
                <div className="flex min-w-0 flex-wrap items-center gap-2">
                    {echoGroup}
                    {auditSlot}
                </div>
                {/* C2 查看详情 */}
                <div className="hidden min-w-0 items-center gap-1 text-xs text-muted-foreground md:flex md:justify-self-end">
                    {detailsSlot}
                </div>
            </div>
        );
    }

    return (
        <>
            {statusBadge}
            {endpointBadge}
            {upstreamPathBadge}
            {requestModelText}
            {channelGroup}
            {actualModelText}
            {echoGroup}
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

    const parsed = useMemo(() => {
        if (!content) return { isJson: false, data: null };
        if (!isOpen || !shouldRender) return { isJson: false, data: null };
        try {
            const data = JSON.parse(content);
            return data !== null && typeof data === 'object'
                ? { isJson: true, data }
                : { isJson: false, data: content };
        } catch {
            return { isJson: false, data: content };
        }
    }, [content, isOpen, shouldRender]);

    const isLargeJson = (content?.length ?? 0) > LARGE_JSON_CHAR_LIMIT;

    useEffect(() => {
        if (!isOpen) {
            const resetTimer = setTimeout(() => setShouldRender(false), 0);
            return () => clearTimeout(resetTimer);
        }

        const timer = setTimeout(() => setShouldRender(true), 300);
        return () => clearTimeout(timer);
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

    return (
        <AnimatePresence mode="wait">
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
            ) : parsed.isJson ? (
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
                <motion.pre
                    key="text"
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    exit={{ opacity: 0 }}
                    transition={{ duration: 0.2 }}
                    className="max-w-full min-w-0 whitespace-pre-wrap break-words [overflow-wrap:anywhere] p-3 font-mono text-xs leading-relaxed text-muted-foreground sm:p-4"
                >
                    {content}
                </motion.pre>
            )}
        </AnimatePresence>
    );
}


function LazyLogBodies({ logId, fallbackRequest, fallbackResponse, requestLabel, responseLabel, tokensLabel, cacheHitLabel, noRequestText, noResponseText, inputTokens, outputTokens, cacheHitTokens, isModelTest }: {
    logId: number;
    fallbackRequest?: string;
    fallbackResponse?: string;
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
        </div>
    );
}

type AuditBadge = {
    label: string;
    className: string;
    /** 命中的快检结果原样保留，供点按弹层展示（得分/发现数/渠道/模型）。 */
    hit: ScheduledAuditResult;
};

/**
 * 模型检测徽标：把定时快检快照（按 渠道ID|模型 索引）匹配到这条日志上，
 * 回答「上游模型对不对」。匹配顺序：先按实际发上游的模型名，再按请求模型名
 * （model_mapping 场景下快检记的可能是任一侧）。没检过/跳过/普通用户 → 无徽标。
 * verdict 语义与检测页一致：none=未见异常、unknown=证据不足、low/medium/high=待复核。
 * 徽标本身点按弹出详情（Popover，移动端手指可触），不再依赖悬停 title。
 */
function auditBadgeForLog(
    log: RelayLog,
    index: Map<string, ScheduledAuditResult> | undefined,
    t: (key: string) => string,
): AuditBadge | undefined {
    if (!index?.size || !log.channel) return undefined;
    const hit =
        index.get(`${log.channel}|${log.actual_model_name}`) ??
        index.get(`${log.channel}|${log.request_model_name}`);
    if (!hit || hit.skipped || !hit.verdict) return undefined;

    const verdict = hit.verdict;
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
    return {
        label: `${t('auditPrefix')} · ${label}`,
        className,
        hit,
    };
}

export const LogCard = React.memo(function LogCard({
    log,
    auditByChannelModel,
}: {
    log: RelayLog;
    /** 定时快检快照按 `渠道ID|模型` 的索引（管理员日志页拉取；普通用户不传）。 */
    auditByChannelModel?: Map<string, ScheduledAuditResult>;
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
        () => auditBadgeForLog(log, auditByChannelModel, t),
        [log, auditByChannelModel, t],
    );
    const requestEndpoint = useMemo(() => log.request_endpoint?.trim() ?? '', [log.request_endpoint]);
    const requestPath = useMemo(() => log.request_path?.trim() ?? '', [log.request_path]);
    // 连通性测试日志：费用/缓存/token 这些对它全是噪音，按这个标记隐掉。
    const isModelTest = isModelTestEndpoint(requestEndpoint) || isModelTestEndpoint(requestPath);
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
     * B1 自动救援（路由状态）、A2 下游客户端、B2 模型检测、C2 查看详情。
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
    // 客户端徽标来自调用端 UA（humanizeClient）。徽标点按弹出详情（Popover）——移动端手指
    // 可触，不依赖悬停。卡片外层是 MorphingDialogTrigger（自带 onClick/onKeyDown），徽标必须
    // 就地掐断事件冒泡，否则点徽标会连带打开整卡详情弹窗。
    const clientSlot = clientLabel ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => e.stopPropagation()}
                    className="shrink-0 cursor-pointer border-violet-500/30 bg-violet-500/10 px-1.5 py-0 text-xs text-violet-600 dark:text-violet-300"
                >
                    {clientLabel}
                </Badge>
            </PopoverTrigger>
            <PopoverContent align="start" className="w-72 p-3">
                <p className="text-xs font-medium text-foreground">{t('client')}</p>
                <p className="mt-1 break-all font-mono text-[11px] leading-5 text-muted-foreground">
                    {log.request_user_agent}
                </p>
            </PopoverContent>
        </Popover>
    ) : null;
    // 检测徽标来自定时快检快照按 渠道+模型 匹配（管理员拉取，普通用户没有 → null）。
    // 弹层同时给「发送模型 vs 上游自报」的本行证据；同样掐断冒泡，避免误开整卡详情。
    const auditSlot = auditBadge ? (
        <Popover>
            <PopoverTrigger asChild>
                <Badge
                    variant="outline"
                    role="button"
                    tabIndex={0}
                    onClick={(e) => e.stopPropagation()}
                    onKeyDown={(e) => e.stopPropagation()}
                    className={cn('shrink-0 cursor-pointer px-1.5 py-0 text-xs', auditBadge.className)}
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
                                    {log.upstream_model_mismatch === true && ` · ${t('echoInconsistent')}`}
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
    const detailsSlot = canViewDetails ? (
        <>
            <Eye className="size-3.5" />
            {t('openDetails')}
        </>
    ) : null;

    return (
            <MorphingDialog>
                <MorphingDialogTrigger
                    disabled={!canViewDetails}
                    className={cn(
                        "relative w-full overflow-hidden rounded-lg border bg-card text-left",
                        hasError
                            ? "border-destructive/40 bg-destructive/[0.05]"
                            : hasPartialFailure
                                ? "border-amber-500/40 bg-amber-500/[0.05]"
                                : "border-border",
                    )}
                >
                    {/* 左侧状态色条：失败/重试的行一眼可辨（抄自 new-api 的整行 tint 思路）。 */}
                    <span
                        aria-hidden
                        className={cn(
                            "absolute inset-y-0 left-0 w-1",
                            hasError ? "bg-destructive" : hasPartialFailure ? "bg-amber-500" : "bg-emerald-500/50",
                        )}
                    />
                    {/* 5050 基线：裸官方 Logo（auto 列）+ 内容列，p-4 / gap-4 */}
                    <div
                        // 验收/排障锚点：让真机脚本能把「模型检测」徽标绑定到某一条确定的日志卡，
                        // 而不是全页面按文字找。日志 id 不是敏感字段（详情弹层里本来就显示它）。
                        // 必须挂在真实 DOM 节点上：MorphingDialogTrigger 只透传它解构过的
                        // props，挂在它上面会被静默丢掉、压根渲染不出来。
                        data-log-id={log.id}
                        className="p-4 grid grid-cols-[auto_1fr] gap-4 items-center"
                    >
                        <div title={modelNameToDisplay} className="shrink-0">
                            <ModelAvatar size={40} />
                        </div>
                        <div className="min-w-0 flex flex-col gap-3">
                            {/* 路由头部：固定 3 列 × 2 行列轨道（在 LogRouteHeader 的 card 分支里）。
                                第 1 排 = 状态·接口·路径 | 请求模型→渠道·实际模型·自动救援 | 流式·图钉；
                                第 2 排 = 下游客户端 | 上游回显·回显不符·模型检测 | 查看详情。
                                第 2 排永远渲染（客户端缺失给灰占位），所以上下两排列宽恒定、内容不再漂移。 */}
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
                                detailsSlot={detailsSlot}
                            />

                            {/* 第 2 行：紧凑摘要带固定两行（与 5050 等高对齐：行1 身份与用量，行2 性能与费用） */}
                            <div className="flex min-w-0 flex-col gap-2 text-xs tabular-nums text-muted-foreground">
                                <div className="flex min-w-0 flex-wrap items-center gap-x-4">
                                {/* G1 时间与位置 */}
                                <div className="flex shrink-0 items-center gap-1.5 whitespace-nowrap">
                                    <Clock className="size-3.5 shrink-0 text-muted-foreground" />
                                    <span>{formatTime(log.time)}</span>
                                    {log.request_ip?.trim() && (
                                        <>
                                            <span className="text-muted-foreground/60">·</span>
                                            <span className="font-mono">{log.request_ip.trim()}</span>
                                        </>
                                    )}
                                </div>

                                {/* G2 用量与推理 */}
                                {!isModelTest && (
                                    <div className="flex shrink-0 items-center gap-1.5 whitespace-nowrap">
                                        <span>{formatTokensSummary(log)}</span>
                                        {reasoningEffort && (
                                            <>
                                                <span className="text-muted-foreground/60">·</span>
                                                <span className="inline-flex items-center gap-1">
                                                    <Brain className="size-3.5 shrink-0 text-muted-foreground" />
                                                    <span>{t('thinking')} {reasoningEffort}</span>
                                                </span>
                                            </>
                                        )}
                                    </div>
                                )}
                                {isModelTest && reasoningEffort && (
                                    <div className="flex shrink-0 items-center gap-1 whitespace-nowrap">
                                        <Brain className="size-3.5 shrink-0 text-muted-foreground" />
                                        <span>{t('thinking')} {reasoningEffort}</span>
                                    </div>
                                )}
                                </div>
                                <div className="flex min-w-0 flex-wrap items-center gap-x-4">

                                {/* G3 耗时与费用 */}
                                <div className="flex shrink-0 items-center gap-1.5 whitespace-nowrap">
                                    <span className={cn("inline-flex items-center gap-1 font-medium", latencyTextColor(log.ftut, 1500, 4000))}>
                                        <Zap className="size-3.5 shrink-0" />
                                        <span>{t('firstToken')} {Number.isFinite(log.ftut) && log.ftut > 0 ? formatDuration(log.ftut) : '--'}</span>
                                    </span>
                                    <span className="text-muted-foreground/60">·</span>
                                    <span className={cn("inline-flex items-center gap-1 font-medium", latencyTextColor(log.use_time, 5000, 15000))}>
                                        <Clock className="size-3.5 shrink-0 text-muted-foreground" />
                                        <span>{t('totalTime')} {formatDuration(log.use_time)}</span>
                                    </span>
                                    {!isModelTest && (
                                        <>
                                            <span className="text-muted-foreground/60">·</span>
                                            <span className="inline-flex items-center gap-1 font-medium tabular-nums text-emerald-600 dark:text-emerald-400">
                                                <DollarSign className="size-3.5 shrink-0 text-emerald-500" />
                                                <span>{Number(log.cost) > 0 ? `$${Number(log.cost).toFixed(6)}` : '$0.000000'}</span>
                                            </span>
                                        </>
                                    )}
                                </div>

                                {/* G4 身份 */}
                                {(requestAPIKeyName || (canViewDetails && userName)) && (
                                    <div className="flex shrink-0 items-center gap-1.5 whitespace-nowrap">
                                        {requestAPIKeyName && (
                                            <span className="inline-flex items-center gap-1">
                                                <KeyRound className="size-3.5 shrink-0 text-muted-foreground" />
                                                <MonoSafeText value={maskSensitive(requestAPIKeyName, sensitiveVisible)} className="max-w-[140px] truncate" />
                                            </span>
                                        )}
                                        {requestAPIKeyName && canViewDetails && userName && (
                                            <span className="text-muted-foreground/60">·</span>
                                        )}
                                        {canViewDetails && userName && (
                                            <span className="inline-flex items-center gap-1">
                                                <User className="size-3.5 shrink-0 text-muted-foreground" />
                                                <span className="max-w-[120px] truncate">{userName}</span>
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
                            />
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
                                            <MonoSafeText mode="wrap" value={log.request_user_agent} className="block text-xs text-foreground" />
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
                                                                                    attempt.status === 'success'
                                                                                        ? "bg-primary/15 text-primary"
                                                                                        : "bg-destructive/15 text-destructive"
                                                                                )}
                                                                            >
                                                                                {formatAttemptStatus(attempt.status, t('success'), t('failed'))}
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
                                        requestLabel={t('requestContent')}
                                        responseLabel={t('responseContent')}
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
    // 定时快检结果通常晚于日志到达（日志先推送、快检后跑）。父组件已用 useMemo 稳定住
    // auditByChannelModel 的引用，所以这里比引用即可；不比较它的话快检结果到达时卡片不重绘，
    // 第 2 排的「模型检测」徽标会一直空着，只有刷新或 log.time 变化才可能露出来。
    && prevProps.auditByChannelModel === nextProps.auditByChannelModel
));
