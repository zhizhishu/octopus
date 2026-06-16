'use client';

import { useStatsDaily, type StatsDailyFormatted } from '@/api/endpoints/stats';
import { useMemo, useState } from 'react';
import { createPortal } from 'react-dom';
import { useTranslations } from 'next-intl';
import { Fragment } from 'react';
import dayjs from 'dayjs';

interface StatsDailyData {
    dateStr: string;
    formatted: StatsDailyFormatted | null;
}

const ACTIVITY_LEVELS = [
    { min: 5000, level: 4 },
    { min: 2000, level: 3 },
    { min: 1000, level: 2 },
    { min: 1, level: 1 }
];

const DAYS_IN_ACTIVITY = 30;
const ACTIVITY_COLUMNS = 15;

function getActivityLevel(value: number): number {
    if (value === 0) return 0;
    return ACTIVITY_LEVELS.find(level => value >= level.min)?.level || 1;
}

export function Activity() {
    const { data: statsDailyFormatted } = useStatsDaily();
    const t = useTranslations('home.activity');

    const [tooltip, setTooltip] = useState<{ day: StatsDailyData; x: number; y: number; visible: boolean } | null>(null);

    const days = useMemo(() => {
        if (!statsDailyFormatted) return [];
        const formattedMap = new Map(statsDailyFormatted.map(stat => [stat.date, stat]));

        const today = dayjs();
        const startDate = today.subtract(DAYS_IN_ACTIVITY - 1, 'day');

        const result: StatsDailyData[] = [];

        for (let i = 0; i < DAYS_IN_ACTIVITY; i++) {
            const currentDate = startDate.add(i, 'day');
            const dateStr = currentDate.format('YYYYMMDD');

            result.push({
                dateStr,
                formatted: formattedMap.get(dateStr) || null
            });
        }

        return result;
    }, [statsDailyFormatted]);

    const dayRows = useMemo(() => {
        return [days.slice(0, ACTIVITY_COLUMNS), days.slice(ACTIVITY_COLUMNS, DAYS_IN_ACTIVITY)];
    }, [days]);

    return (
        <div className="rounded-3xl bg-card border-card-border border text-card-foreground custom-shadow">
            <div className="p-4">
                <div className="flex w-full flex-col gap-1.5">
                    {dayRows.map((row, rowIndex) => (
                        <div key={rowIndex} className="grid w-full grid-cols-[repeat(15,minmax(0,1fr))] gap-1.5">
                            {row.map((day) => {
                                const level = getActivityLevel(day.formatted?.request_count.raw ?? 0);

                                return (
                                    <div
                                        key={day.dateStr}
                                        className="h-3.5 rounded-sm transition-all cursor-pointer hover:scale-[1.03] hover:shadow-sm md:h-4"
                                        onMouseEnter={(e) => {
                                            const rect = e.currentTarget.getBoundingClientRect();
                                            setTooltip({ day, x: rect.left + rect.width / 2, y: rect.top, visible: true });
                                        }}
                                        onMouseLeave={() => setTooltip(prev => prev ? { ...prev, visible: false } : null)}
                                        style={{ backgroundColor: level === 0 ? 'var(--muted)' : `color-mix(in oklch, var(--primary) ${level * 25}%, var(--muted))` }}
                                    />
                                );
                            })}
                        </div>
                    ))}
                </div>
            </div>
            {tooltip && typeof document !== 'undefined' && createPortal(
                (() => {
                    const isLeft = tooltip.x < 200;
                    const isRight = tooltip.x > window.innerWidth - 200;
                    const isTop = tooltip.y < window.innerHeight / 2;
                    const tooltipDate = dayjs(tooltip.day.dateStr, 'YYYYMMDD');
                    const tooltipDateLabel = tooltipDate.isValid() ? tooltipDate.format('YYYY-MM-DD') : tooltip.day.dateStr;

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
                            className={`fixed z-50 w-[min(20rem,calc(100vw-1rem))] max-w-[calc(100vw-1rem)] rounded-3xl border bg-background p-3 text-sm text-foreground shadow-sm transition-opacity duration-500 pointer-events-none ${tooltip.visible ? 'opacity-100' : 'opacity-0'}`}
                            style={{
                                left: tooltip.x,
                                top: tooltip.y,
                                transform
                            }}
                        >
                            <div className="space-y-2">
                                <p className="font-semibold text-foreground">{tooltipDateLabel}</p>
                                {tooltip.day.formatted ? (
                                    <div className="grid min-w-0 grid-cols-[minmax(0,1fr)_auto] items-center gap-x-4 gap-y-1 text-muted-foreground">
                                        {[
                                            { labelKey: 'requestCount', ...tooltip.day.formatted.request_count },
                                            { labelKey: 'waitTime', ...tooltip.day.formatted.wait_time },
                                            { labelKey: 'totalToken', ...tooltip.day.formatted.total_token },
                                            { labelKey: 'totalCost', ...tooltip.day.formatted.total_cost },
                                            { labelKey: 'cacheHitRate', ...tooltip.day.formatted.cache_hit_rate },
                                        ].map((item, index) => (
                                            <Fragment key={index}>
                                                <span className="min-w-0 break-words">{t(item.labelKey)}</span>
                                                <span className="text-right font-medium text-foreground">{item.formatted.value}{item.formatted.unit}</span>
                                            </Fragment>
                                        ))}
                                    </div>
                                ) : (
                                    <p className="text-muted-foreground">{t('noData')}</p>
                                )}
                            </div>
                        </div>
                    );
                })(),
                document.body
            )}
        </div>
    );
}
