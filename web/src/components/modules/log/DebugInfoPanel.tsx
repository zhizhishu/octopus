'use client';

import { useState, type ReactNode } from 'react';
import { Bug, ChevronDown, ChevronUp } from 'lucide-react';
import { cn } from '@/lib/utils';
import type { RelayLogDebugCall, RelayLogDebugHeader, RelayLogDebugInfo } from '@/api/endpoints/log';
import {
    debugCallTone,
    debugMs,
    debugRouteModeLabel,
    debugRouteStageLabel,
    debugTimingRows,
    hasDebugInfo,
    splitRouteTrace,
} from './debug-info';

/**
 * 详情弹窗里的「调试详情」。只有「日志模式 = 调试」期间的请求才有数据，没有就不渲染。
 *
 * 数据来自详情接口（列表/实时流不带）。凭据类请求头后端已整值隐藏，这里只负责展示。
 * 默认折叠：多数时候看的是请求/响应正文，调试详情点开才看。
 */
export function DebugInfoPanel({ info, defaultOpen = false }: { info?: RelayLogDebugInfo | null; defaultOpen?: boolean }) {
    const [open, setOpen] = useState(defaultOpen);
    if (!hasDebugInfo(info)) return null;
    const calls = info.calls ?? [];
    const timing = debugTimingRows(info);

    return (
        <div className="shrink-0 overflow-hidden rounded-lg border border-amber-500/30 bg-amber-500/[0.04]">
            <button
                type="button"
                onClick={() => setOpen(!open)}
                aria-expanded={open}
                className="flex w-full min-w-0 items-center gap-2 px-3 py-2.5 text-left transition-colors hover:bg-amber-500/10"
            >
                <Bug className="size-4 shrink-0 text-amber-600 dark:text-amber-400" />
                <span className="text-sm font-medium text-foreground">调试详情</span>
                <span className="min-w-0 truncate text-xs text-muted-foreground">
                    {calls.length} 次上游请求 · 总耗时 {debugMs(info.timing?.total_ms)}
                </span>
                <span className="ml-auto shrink-0">
                    {open ? <ChevronUp className="size-4 text-muted-foreground" /> : <ChevronDown className="size-4 text-muted-foreground" />}
                </span>
            </button>
            {open && (
                <div className="flex min-w-0 flex-col gap-4 border-t border-amber-500/20 p-2.5 md:p-3">
                    <section className="min-w-0">
                        <SectionTitle>耗时</SectionTitle>
                        <div className="grid grid-cols-3 gap-1.5 md:grid-cols-6">
                            {timing.map((row) => (
                                <div key={row.key} className="min-w-0 rounded-md border border-border/60 bg-card/60 px-2 py-1.5">
                                    <div className="truncate text-[10px] text-muted-foreground">{row.label}</div>
                                    <div className="truncate font-mono text-xs tabular-nums text-foreground">{row.value}</div>
                                </div>
                            ))}
                        </div>
                    </section>

                    {(info.routes?.length ?? 0) > 0 && (
                        <section className="min-w-0">
                            <SectionTitle>选路</SectionTitle>
                            <div className="flex flex-col gap-2">
                                {info.routes!.map((route, i) => (
                                    <RouteBlock key={`${route.stage}-${i}`} route={route} />
                                ))}
                            </div>
                        </section>
                    )}

                    {calls.length > 0 && (
                        <section className="min-w-0">
                            <SectionTitle>上游请求（按发出顺序）</SectionTitle>
                            <ol className="flex flex-col gap-2">
                                {calls.map((call, i) => (
                                    <CallCard key={i} index={i} call={call} />
                                ))}
                            </ol>
                        </section>
                    )}

                    {(info.inbound_headers?.length ?? 0) > 0 && (
                        <HeaderToggle label="客户端请求头" headers={info.inbound_headers!} />
                    )}

                    <p className="text-[11px] leading-5 text-muted-foreground">
                        密钥类请求头不保存，显示为「[已隐藏]」。请求头按名字排序，实际发送顺序不受影响。调试详情保留 3 天。
                        {info.truncated ? ` 内容较多，已省略一部分${info.dropped_calls ? `（少记 ${info.dropped_calls} 次请求）` : ''}。` : ''}
                    </p>
                </div>
            )}
        </div>
    );
}

function SectionTitle({ children }: { children: ReactNode }) {
    return <div className="mb-1.5 text-xs font-medium text-muted-foreground">{children}</div>;
}

function RouteBlock({ route }: { route: NonNullable<RelayLogDebugInfo['routes']>[number] }) {
    const [showTrace, setShowTrace] = useState(false);
    const lines = splitRouteTrace(route.trace);
    return (
        <div className="min-w-0 rounded-md border border-border/60 bg-card/60 px-2.5 py-2 text-xs">
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                <span className="font-medium text-foreground">{debugRouteStageLabel(route.stage)}</span>
                {route.group_name && <span className="min-w-0 break-all text-muted-foreground">分组 {route.group_name}</span>}
                <span className="rounded bg-primary/10 px-1.5 py-0.5 text-[11px] font-medium text-primary">{debugRouteModeLabel(route.mode)}</span>
                <span className="text-muted-foreground">候选 {route.candidates} 个</span>
                {route.sticky && <span className="text-muted-foreground">会话粘性</span>}
            </div>
            {lines.length > 0 && (
                <>
                    <button
                        type="button"
                        onClick={() => setShowTrace(!showTrace)}
                        className="mt-1.5 text-[11px] text-primary hover:underline"
                    >
                        {showTrace ? '收起排序原因' : '看排序原因'}
                    </button>
                    {showTrace && (
                        <ul className="mt-1 space-y-0.5 font-mono text-[11px] leading-5 text-muted-foreground [overflow-wrap:anywhere]">
                            {lines.map((line, i) => (
                                <li key={i}>{line}</li>
                            ))}
                        </ul>
                    )}
                </>
            )}
        </div>
    );
}

function CallCard({ call, index }: { call: RelayLogDebugCall; index: number }) {
    const tone = debugCallTone(call.status, call.error);
    return (
        <li
            className={cn(
                'min-w-0 rounded-md border px-2.5 py-2 text-xs',
                tone === 'ok' && 'border-primary/20 bg-primary/5',
                tone === 'warn' && 'border-amber-500/30 bg-amber-500/5',
                tone === 'error' && 'border-destructive/30 bg-destructive/5',
            )}
        >
            <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                <span className="font-mono text-muted-foreground">#{index + 1}</span>
                <span className="min-w-0 break-all font-medium text-foreground">{call.channel_name || `渠道 ${call.channel_id}`}</span>
                {call.channel_key_id ? <span className="text-muted-foreground">Key #{call.channel_key_id}</span> : null}
                <span
                    className={cn(
                        'rounded px-1.5 py-0.5 font-mono text-[11px] font-medium',
                        tone === 'ok' && 'bg-primary/10 text-primary',
                        tone === 'warn' && 'bg-amber-500/15 text-amber-700 dark:text-amber-300',
                        tone === 'error' && 'bg-destructive/10 text-destructive',
                    )}
                >
                    {call.status ? call.status : '无响应'}
                </span>
                <span className="ml-auto shrink-0 font-mono text-[11px] tabular-nums text-muted-foreground">
                    +{debugMs(call.start_ms)} 发出 · 等头 {debugMs(call.header_ms)}
                </span>
            </div>
            {call.url && (
                <div className="mt-1 font-mono text-[11px] text-muted-foreground [overflow-wrap:anywhere]">
                    {call.method ? `${call.method} ` : ''}{call.url}
                </div>
            )}
            {call.error && (
                <div className="mt-1.5 border-l-2 border-destructive/40 pl-2 text-[11px] text-destructive [overflow-wrap:anywhere]">{call.error}</div>
            )}
            {call.error_body && (
                <div className="mt-1.5">
                    <div className="text-[11px] text-muted-foreground">上游报错原文</div>
                    <pre className="mt-0.5 max-h-40 overflow-auto whitespace-pre-wrap rounded bg-background/70 p-2 font-mono text-[11px] leading-5 text-foreground/90 [overflow-wrap:anywhere]">
                        {call.error_body}
                    </pre>
                </div>
            )}
            {(call.notes?.length ?? 0) > 0 && (
                <ul className="mt-1.5 space-y-0.5 text-[11px] text-amber-700 dark:text-amber-300">
                    {call.notes!.map((note, i) => (
                        <li key={i}>→ {note}</li>
                    ))}
                </ul>
            )}
            <div className="mt-1.5 flex flex-wrap gap-2">
                {(call.request_headers?.length ?? 0) > 0 && <HeaderToggle label="发出的请求头" headers={call.request_headers!} compact />}
                {(call.response_headers?.length ?? 0) > 0 && <HeaderToggle label="收到的响应头" headers={call.response_headers!} compact />}
            </div>
        </li>
    );
}

function HeaderToggle({ label, headers, compact = false }: { label: string; headers: RelayLogDebugHeader[]; compact?: boolean }) {
    const [open, setOpen] = useState(false);
    return (
        <div className={cn('min-w-0', open && 'basis-full')}>
            <button
                type="button"
                onClick={() => setOpen(!open)}
                aria-expanded={open}
                className={cn(
                    'inline-flex items-center gap-1 rounded-md border border-border/70 bg-background/60 px-2 text-[11px] text-muted-foreground transition-colors hover:text-foreground',
                    compact ? 'h-7' : 'h-8',
                )}
            >
                {label}（{headers.length}）
                {open ? <ChevronUp className="size-3" /> : <ChevronDown className="size-3" />}
            </button>
            {open && (
                <dl className="mt-1.5 grid min-w-0 grid-cols-1 gap-x-3 gap-y-1 rounded-md bg-background/70 p-2 font-mono text-[11px] leading-5 sm:grid-cols-[minmax(8rem,max-content)_1fr]">
                    {headers.map((h, i) => (
                        <div key={`${h.name}-${i}`} className="contents">
                            <dt className="text-muted-foreground [overflow-wrap:anywhere]">{h.name}</dt>
                            <dd
                                className={cn(
                                    'mb-1 min-w-0 [overflow-wrap:anywhere] sm:mb-0',
                                    h.value === '[已隐藏]' ? 'italic text-muted-foreground' : 'text-foreground/90',
                                )}
                            >
                                {h.value}
                            </dd>
                        </div>
                    ))}
                </dl>
            )}
        </div>
    );
}
