'use client';

import { useModelHealth, type ModelHealthHour, type ModelHealthModel } from '@/api/endpoints/stats';
import { cn, formatPercent } from '@/lib/utils';
import { ChevronDown } from 'lucide-react';
import { Fragment, useMemo, useState } from 'react';
import { createPortal } from 'react-dom';
import { useTranslations } from 'next-intl';

const HOUR_LEVELS = [
    { min: 15, level: 4 },
    { min: 8, level: 3 },
    { min: 3, level: 2 },
    { min: 1, level: 1 },
];

function requestCount(item: ModelHealthHour) {
    return item.request_success + item.request_failed;
}

function getHourLevel(value: number): number {
    if (value === 0) return 0;
    return HOUR_LEVELS.find(level => value >= level.min)?.level || 1;
}

function getHourColor(item: ModelHealthHour) {
    const total = requestCount(item);
    if (total === 0) return 'var(--muted)';

    const level = getHourLevel(total);
    const intensity = level * 22 + 12;
    const failRate = item.request_failed / total;
    if (failRate >= 0.5) {
        return `color-mix(in oklch, var(--destructive) ${intensity}%, var(--muted))`;
    }
    if (failRate > 0) {
        return `color-mix(in oklch, var(--destructive) ${Math.max(35, failRate * 100)}%, color-mix(in oklch, var(--primary) ${intensity}%, var(--muted)))`;
    }
    return `color-mix(in oklch, var(--primary) ${intensity}%, var(--muted))`;
}

function formatLatency(ms: number) {
    if (!ms || ms <= 0) return '0.00s';
    return `${(ms / 1000).toFixed(2)}s`;
}

function formatThroughput(value: number) {
    return `${(value || 0).toFixed(1)} tok/s`;
}

function formatCacheRate(value: number) {
    const formatted = formatPercent(value);
    return `${formatted.formatted.value}${formatted.formatted.unit}`;
}

function metricLine(model: ModelHealthModel, t: ReturnType<typeof useTranslations>) {
    return [
        `${t('firstTokenP90')} ${formatLatency(model.summary.first_token_p90_ms)}`,
        `${t('avgThroughput')} ${formatThroughput(model.summary.avg_throughput)}`,
        `${t('cacheRate')} ${formatCacheRate(model.summary.cache_hit_rate)}`,
    ].join(' · ');
}

export function ModelHealth() {
    const { data, isPending, isError } = useModelHealth();
    const t = useTranslations('home.modelHealth');
    const [expanded, setExpanded] = useState<Record<string, boolean>>({
        OpenAI: true,
        Gemini: true,
        Anthropic: true,
    });
    const [modelsExpanded, setModelsExpanded] = useState<Record<string, boolean>>({});
    const [tooltip, setTooltip] = useState<{
        provider: string;
        model: string;
        hour: ModelHealthHour;
        x: number;
        y: number;
        visible: boolean;
    } | null>(null);

    const providers = useMemo(() => data?.providers ?? [], [data]);
    const hasModels = providers.some((provider) => provider.models.length > 0);

    return (
        <div className="rounded-3xl bg-card border-card-border border p-4 text-card-foreground custom-shadow">
            <h3 className="font-semibold text-base mb-3">{t('title')}</h3>
            {isPending ? (
                <p className="text-sm text-muted-foreground">{t('loading')}</p>
            ) : isError ? (
                <p className="text-sm text-muted-foreground">{t('loadFailed')}</p>
            ) : !hasModels ? (
                <p className="text-sm text-muted-foreground">{t('noData')}</p>
            ) : (
            <div className="space-y-3">
                {providers.map((provider) => {
                    const isExpanded = expanded[provider.provider] ?? true;
                    const hasMoreModels = provider.models.length > 2;
                    const isModelListExpanded = modelsExpanded[provider.provider] ?? false;
                    const visibleModels = hasMoreModels && !isModelListExpanded ? provider.models.slice(0, 2) : provider.models;
                    return (
                        <section key={provider.provider} className="rounded-2xl border border-border/50">
                            <button
                                type="button"
                                onClick={() => setExpanded(prev => ({ ...prev, [provider.provider]: !isExpanded }))}
                                className="flex w-full items-center justify-between gap-3 px-3 py-2 text-left"
                            >
                                <div className="flex items-center gap-2">
                                    <ChevronDown className={cn('size-4 text-muted-foreground transition-transform', !isExpanded && '-rotate-90')} />
                                    <span className="font-medium">{provider.provider}</span>
                                </div>
                                <span className="text-xs text-muted-foreground">{provider.models.length} {t('models')}</span>
                            </button>

                            {isExpanded && (
                                <div className="space-y-3 border-t border-border/50 px-3 py-3">
                                    {provider.models.length === 0 ? (
                                        <p className="text-sm text-muted-foreground">{t('noData')}</p>
                                    ) : (
                                        <>
                                            {visibleModels.map((model) => (
                                            <div key={model.model} className="grid min-w-0 gap-2 md:grid-cols-[minmax(7rem,12rem)_1fr]">
                                                <div className="min-w-0 text-sm font-medium truncate md:pt-0.5" title={model.model}>
                                                    {model.model}
                                                </div>
                                                <div className="min-w-0">
                                                    <div className="grid gap-1 overflow-x-auto pb-1"
                                                        style={{ gridTemplateColumns: 'repeat(24, 0.875rem)' }}
                                                    >
                                                        {model.hours.map((hour) => (
                                                            <button
                                                                key={hour.hour}
                                                                type="button"
                                                                className="size-3.5 rounded-sm transition-all hover:scale-150 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                                                                style={{ backgroundColor: getHourColor(hour) }}
                                                                aria-label={`${model.model} ${hour.hour}:00`}
                                                                onMouseEnter={(e) => {
                                                                    const rect = e.currentTarget.getBoundingClientRect();
                                                                    setTooltip({
                                                                        provider: provider.provider,
                                                                        model: model.model,
                                                                        hour,
                                                                        x: rect.left + rect.width / 2,
                                                                        y: rect.top,
                                                                        visible: true,
                                                                    });
                                                                }}
                                                                onFocus={(e) => {
                                                                    const rect = e.currentTarget.getBoundingClientRect();
                                                                    setTooltip({
                                                                        provider: provider.provider,
                                                                        model: model.model,
                                                                        hour,
                                                                        x: rect.left + rect.width / 2,
                                                                        y: rect.top,
                                                                        visible: true,
                                                                    });
                                                                }}
                                                                onMouseLeave={() => setTooltip(prev => prev ? { ...prev, visible: false } : null)}
                                                                onBlur={() => setTooltip(prev => prev ? { ...prev, visible: false } : null)}
                                                            />
                                                        ))}
                                                    </div>
                                                    <p className="break-words text-xs leading-relaxed text-muted-foreground">
                                                        {metricLine(model, t)}
                                                    </p>
                                                </div>
                                            </div>
                                            ))}
                                            {hasMoreModels && (
                                                <button
                                                    type="button"
                                                    className="text-xs font-medium text-primary hover:text-primary/80"
                                                    onClick={() => setModelsExpanded(prev => ({ ...prev, [provider.provider]: !isModelListExpanded }))}
                                                >
                                                    {isModelListExpanded ? t('collapseModels') : t('expandModels', { count: provider.models.length - 2 })}
                                                </button>
                                            )}
                                        </>
                                    )}
                                </div>
                            )}
                        </section>
                    );
                })}
            </div>
            )}

            {tooltip && typeof document !== 'undefined' && createPortal(
                (() => {
                    const isLeft = tooltip.x < 220;
                    const isRight = tooltip.x > window.innerWidth - 220;
                    const isTop = tooltip.y < window.innerHeight / 2;

                    let transform = 'translate(-50%, 15%)';
                    if (!isTop && !isLeft && !isRight) {
                        transform = 'translate(-50%, -105%)';
                    } else if (isTop && isLeft) {
                        transform = 'translate(10%, 15%)';
                    } else if (isTop && isRight) {
                        transform = 'translate(-110%, 15%)';
                    } else if (!isTop && isLeft) {
                        transform = 'translate(10%, -105%)';
                    } else if (!isTop && isRight) {
                        transform = 'translate(-110%, -105%)';
                    }

                    return (
                        <div
                            className={`fixed z-50 w-[min(22rem,calc(100vw-1rem))] max-w-[calc(100vw-1rem)] rounded-3xl border bg-background p-3 text-sm text-foreground shadow-sm transition-opacity duration-500 pointer-events-none ${tooltip.visible ? 'opacity-100' : 'opacity-0'}`}
                            style={{ left: tooltip.x, top: tooltip.y, transform }}
                        >
                            <div className="space-y-2">
                                <p className="break-words font-semibold text-foreground">
                                    {tooltip.provider} · {tooltip.model} · {tooltip.hour.hour.toString().padStart(2, '0')}:00
                                </p>
                                <div className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1 text-muted-foreground">
                                    {[
                                        [t('requestCount'), requestCount(tooltip.hour).toLocaleString()],
                                        [t('successFailed'), `${tooltip.hour.request_success.toLocaleString()} / ${tooltip.hour.request_failed.toLocaleString()}`],
                                        [t('firstTokenP90'), formatLatency(tooltip.hour.first_token_p90_ms)],
                                        [t('avgThroughput'), formatThroughput(tooltip.hour.avg_throughput)],
                                        [t('cacheRate'), formatCacheRate(tooltip.hour.cache_hit_rate)],
                                    ].map(([label, value]) => (
                                        <Fragment key={label}>
                                            <span className="min-w-0 break-words">{label}</span>
                                            <span className="text-right font-medium text-foreground">{value}</span>
                                        </Fragment>
                                    ))}
                                </div>
                            </div>
                        </div>
                    );
                })(),
                document.body
            )}
        </div>
    );
}
