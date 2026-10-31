'use client';

import { useState } from 'react';
import { ArrowRight, Loader2, Logs } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useUserUsage, type User, type UserUsage, type UserUsageRange } from '@/api/endpoints/user';
import { useAPIKeyList } from '@/api/endpoints/apikey';
import { useStatsAPIKey } from '@/api/endpoints/stats';
import { SafeText } from '@/components/common/SafeText';
import { cn } from '@/lib/utils';
import { formatUSD } from '../lib/balance-preview';
import { formatCount, formatTokens } from '../lib/format';
import { Bar, HelpTip, Pill, Seg } from './ui-bits';

const MODEL_TONES = ['primary', 'chart3', 'chart5'] as const;

export function UsageRangeSeg({ value, onChange }: { value: UserUsageRange; onChange: (v: UserUsageRange) => void }) {
    const t = useTranslations('user.usage');
    return (
        <Seg
            size="sm"
            value={value}
            onChange={onChange}
            options={(['today', '7d', '30d', 'all'] as const).map((v) => ({ value: v, label: t(`range.${v}`) }))}
        />
    );
}

function dayLabel(date: string, isToday: boolean, todayText: string) {
    if (isToday) return todayText;
    const [, m, d] = date.split('-');
    return `${Number(m)}/${Number(d)}`;
}

/** 用量概览：三张卡（请求 / 花费 / Tokens）+ 每天花费 + 常用模型前 3。用户抽屉和 API Key 抽屉共用。 */
export function UsageOverview({ usage, loading, range, costTip, wide }: { usage: UserUsage | undefined; loading: boolean; range: UserUsageRange; costTip: string; wide?: boolean }) {
    const t = useTranslations('user.usage');
    const [hoverDay, setHoverDay] = useState<number | null>(null);
    if (loading || !usage) {
        return <div className="flex justify-center py-16 text-muted-foreground"><Loader2 className="size-5 animate-spin" /></div>;
    }
    const requests = usage.request_success + usage.request_failed;
    const successRate = requests > 0 ? (usage.request_success / requests) * 100 : 0;
    const maxDay = Math.max(0, ...usage.daily.map((d) => d.cost));
    const many = usage.daily.length > 10;
    const topModels = usage.top_models.slice(0, 3);
    const maxModel = Math.max(0, ...topModels.map((m) => m.cost));

    return (
        <div className="space-y-5">
            <div className="grid grid-cols-3 gap-2">
                <div className="rounded-2xl border border-border/70 bg-muted/20 px-3 py-2.5">
                    <div className="text-xs text-muted-foreground">{t('requests')}</div>
                    <div className="mt-1 text-lg font-semibold tabular-nums">{formatCount(requests)}</div>
                    <div className="text-[11px] text-muted-foreground">{requests > 0 ? t('successRate', { rate: `${Number(successRate.toFixed(1))}%` }) : t('noRequests')}</div>
                </div>
                <div className="rounded-2xl border border-border/70 bg-muted/20 px-3 py-2.5">
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">{t('cost')}<HelpTip content={costTip} /></div>
                    <div className="mt-1 text-lg font-semibold tabular-nums">{formatUSD(usage.total_cost)}</div>
                    <div className="text-[11px] text-muted-foreground">{requests > 0 ? t('perRequest', { cost: formatUSD(usage.total_cost / requests) }) : '—'}</div>
                </div>
                <div className="rounded-2xl border border-border/70 bg-muted/20 px-3 py-2.5">
                    <div className="text-xs text-muted-foreground">Tokens</div>
                    <div className="mt-1 text-lg font-semibold tabular-nums">{formatTokens(usage.input_tokens + usage.output_tokens)}</div>
                    <div className="truncate text-[11px] text-muted-foreground">{t('tokensSplit', { input: formatTokens(usage.input_tokens), output: formatTokens(usage.output_tokens) })}</div>
                </div>
            </div>

            <div className={wide ? 'grid gap-5 lg:grid-cols-[3fr_2fr]' : 'space-y-5'}>
                <section>
                    <h4 className="mb-2 flex items-center gap-1 text-sm font-semibold">
                        {range === 'all' ? t('dailyAll') : t('daily')}
                        <HelpTip content={range === 'all' ? t('dailyAllTip') : t('dailyTip')} />
                    </h4>
                    <div className={cn('flex h-32 items-end', many ? 'gap-[3px]' : 'gap-2')} onMouseLeave={() => setHoverDay(null)}>
                        {usage.daily.map((d, i) => {
                            const isToday = i === usage.daily.length - 1;
                            const h = maxDay > 0 ? Math.max(d.cost > 0 ? 3 : 0, (d.cost / maxDay) * 80) : 0;
                            const showLabel = !many || isToday || i % 5 === (usage.daily.length - 1) % 5;
                            return (
                                <div key={d.date} className="relative flex min-w-0 flex-1 flex-col items-center gap-1" onMouseEnter={() => setHoverDay(i)} title={`${d.date} · ${formatUSD(d.cost)} · ${t('times', { n: d.requests })}`}>
                                    {!many && <span className="text-[10px] tabular-nums text-muted-foreground">{d.cost > 0 ? d.cost.toFixed(d.cost < 10 ? 2 : 1) : '0'}</span>}
                                    {many && hoverDay === i && (
                                        <span className="absolute -top-5 z-10 rounded bg-muted px-1.5 text-[10px] whitespace-nowrap text-foreground shadow ring-1 ring-border">{formatUSD(d.cost)}</span>
                                    )}
                                    <div className={cn('w-full rounded-t-md', isToday ? 'bg-primary' : 'bg-primary/40', d.cost === 0 && 'bg-muted')} style={{ height: d.cost === 0 ? 2 : h }} />
                                    <span className={cn('text-[10px] whitespace-nowrap text-muted-foreground', !showLabel && 'invisible')}>{dayLabel(d.date, isToday, t('today'))}</span>
                                </div>
                            );
                        })}
                    </div>
                </section>

                <section>
                    <h4 className="mb-2 text-sm font-semibold">{t('topModels')}</h4>
                    {topModels.length === 0 ? (
                        <div className="text-xs text-muted-foreground">{t('noModels')}</div>
                    ) : (
                        <div className="space-y-2 text-sm">
                            {topModels.map((m, i) => (
                                <div key={m.model} className="flex items-center gap-3" title={`${m.model} · ${t('times', { n: m.requests })} · ${formatTokens(m.tokens)} tokens`}>
                                    <SafeText value={m.model} className={cn('shrink-0', wide ? 'w-32' : 'w-40')} />
                                    <Bar value={maxModel > 0 ? (m.cost / maxModel) * 100 : 0} tone={MODEL_TONES[i]} className="flex-1" />
                                    <span className="w-16 text-right text-xs tabular-nums text-muted-foreground">{formatUSD(m.cost)}</span>
                                </div>
                            ))}
                        </div>
                    )}
                </section>
            </div>
        </div>
    );
}

// 抽屉「用量」页：请求(成功率)、花费、Tokens；每天花费柱状图；常用模型前 3；他的 API Key；看他最近的请求。
export function UserUsageTab({ user, range, onViewLogs }: { user: User; range: UserUsageRange; onViewLogs: () => void }) {
    const t = useTranslations('user.usage');
    const { data: usage, isLoading } = useUserUsage(user.id, range);
    const { data: keys = [] } = useAPIKeyList();
    const { data: keyStats = [] } = useStatsAPIKey();
    const userKeys = keys.filter((k) => k.user_id === user.id);
    const costOf = (id: number) => keyStats.find((s) => s.api_key_id === id)?.total_cost.raw ?? 0;

    return (
        <div className="space-y-5">
            <UsageOverview usage={usage} loading={isLoading} range={range} costTip={user.role === 'admin' ? t('costTipAdmin') : t('costTip')} />

            <section>
                <div className="mb-2 flex items-center justify-between">
                    <h4 className="flex items-center gap-1 text-sm font-semibold">{t('keys')}<HelpTip content={t('keysTip')} /></h4>
                    <span className="text-xs text-muted-foreground">{t('keysCount', { n: userKeys.length })}</span>
                </div>
                {userKeys.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border/70 px-3 py-4 text-center text-xs text-muted-foreground">{t('noKeys')}</div>
                ) : (
                    <div className="divide-y divide-border/70 rounded-2xl border border-border/70 text-sm">
                        {userKeys.map((k) => (
                            <div key={k.id} className="flex items-center gap-2 px-3 py-2">
                                <SafeText value={k.name} className="min-w-0 flex-1" />
                                <span className="text-xs tabular-nums text-muted-foreground">{t('keyTotal', { cost: formatUSD(costOf(k.id)) })}</span>
                                {k.enabled ? <Pill tone="ok">{t('keyOn')}</Pill> : <Pill tone="off">{t('keyOff')}</Pill>}
                            </div>
                        ))}
                    </div>
                )}
            </section>

            <button type="button" onClick={onViewLogs} className="flex w-full items-center justify-between rounded-2xl bg-muted/40 px-3 py-2.5 text-sm hover:bg-muted/70">
                <span className="flex items-center gap-2"><Logs className="size-4" />{t('viewLogs')}</span>
                <ArrowRight className="size-4 text-muted-foreground" />
            </button>
        </div>
    );
}
