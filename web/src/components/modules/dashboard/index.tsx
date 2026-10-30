'use client';

import { useState, type ComponentType, type ReactNode } from 'react';
import { Clock3, Coins, DollarSign, MessageSquare, Percent, Users as UsersIcon } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { useQueryClient } from '@tanstack/react-query';
import { PageWrapper } from '@/components/common/PageWrapper';
import { useStatsToday } from '@/api/endpoints/stats';
import { useUserList } from '@/api/endpoints/user';
import { useStatsBreakdown, useStatsDailyRaw, useStatsUserTrend, type StatsBreakdownItem } from '@/api/endpoints/dashboard';
import { useNavStore } from '@/components/modules/navbar/nav-store';
import { useLogPresetStore } from '@/components/modules/log/log-preset';
import { useUserPresetStore } from '@/components/modules/user/user-preset';
import { cn } from '@/lib/utils';
import { Delta, InfoTip, Sparkline } from './parts';
import { RangeBar } from './range-bar';
import { KeyRankPanel, MobileRankPanel, SharePanel, TrendPanel, UserRankPanel, UserTrendPanel, type ShareDimension } from './sections';
import { formatCountText, formatDurationText, formatMoneyText, formatPercentText, shortDate } from './format';
import {
    buildTrend,
    cacheRateBase,
    compactToISO,
    currentHourIndex,
    dailySeries,
    effectiveGranularity,
    fromLog,
    isSingleDay,
    listDates,
    logsCleanedBefore,
    matchPreset,
    pctChange,
    presetRange,
    previousRange,
    rangeDays,
    summarize,
    totalTokens,
    type DateRange,
    type Granularity,
    type RangePreset,
    type TrendMetric,
    type Usage,
} from './model';

function browserToday(): string {
    const d = new Date();
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
}

function StatCard({ icon: Icon, title, tip, value, suffix, sub, spark, className }: {
    icon: ComponentType<{ className?: string }>;
    title: string;
    tip: string;
    value: string;
    suffix?: string;
    sub: ReactNode;
    spark?: (number | null)[];
    className?: string;
}) {
    return (
        <div className={cn('flex min-w-0 flex-col rounded-3xl border border-card-border bg-card p-3.5 text-card-foreground custom-shadow md:p-4', className)}>
            <div className="flex items-center gap-1.5 text-xs text-muted-foreground">
                <Icon className="size-3.5 shrink-0" />
                <span className="truncate">{title}</span>
                <InfoTip className="ml-auto">{tip}</InfoTip>
            </div>
            <div className="mt-1.5 truncate text-xl font-semibold tabular-nums md:text-2xl">
                {value}
                {suffix ? <span className="ml-1 text-sm font-normal text-muted-foreground">{suffix}</span> : null}
            </div>
            <div className="mt-0.5 truncate text-[11px] text-muted-foreground md:text-xs">{sub}</div>
            {spark ? <Sparkline values={spark} className="mt-2 hidden md:block" /> : <div className="mt-2 hidden h-8 md:block" />}
        </div>
    );
}

export function Dashboard() {
    const t = useTranslations('dashboard');
    const queryClient = useQueryClient();
    const setActiveItem = useNavStore((s) => s.setActiveItem);
    const openLog = useLogPresetStore((s) => s.setPreset);
    const openUser = useUserPresetStore((s) => s.setFocusUserID);

    // 「今天」用统计时区的（/stats/today 的 date），没拿到前先用浏览器的。
    const todayQuery = useStatsToday();
    const today = compactToISO(todayQuery.data?.date) || browserToday();

    const [preset, setPreset] = useState<RangePreset>('7d');
    const [customRange, setCustomRange] = useState<DateRange | null>(null);
    const range: DateRange = preset === 'custom' && customRange ? customRange : presetRange(preset === 'custom' ? '7d' : preset, today);
    const prev = previousRange(range);
    const days = rangeDays(range);
    const oneDay = isSingleDay(range);
    const isToday = oneDay && range.from === today;

    const [wantGranularity, setWantGranularity] = useState<Granularity>('hour');
    const granularity = effectiveGranularity(range, wantGranularity);
    const [trendMetric, setTrendMetric] = useState<TrendMetric>('cost');
    const [shareDim, setShareDim] = useState<ShareDimension>('model');

    const dailyQuery = useStatsDailyRaw();
    const usersQuery = useUserList();
    // 按小时（只在一天时）：这一天和前一天都按日志切小时；看今天时还要拿它做「比昨天这时」。
    const hourCur = useStatsBreakdown(oneDay ? { from: range.from, to: range.to, by: 'hour' } : null);
    const hourPrev = useStatsBreakdown(oneDay ? { from: prev.from, to: prev.to, by: 'hour' } : null);
    const share = useStatsBreakdown({ from: range.from, to: range.to, by: shareDim, limit: 5 });
    const userRank = useStatsBreakdown({ from: range.from, to: range.to, by: 'user', limit: 8 });
    const keyRank = useStatsBreakdown({ from: range.from, to: range.to, by: 'apikey', limit: 8 });
    const userTrend = useStatsUserTrend(oneDay ? null : { from: range.from, to: range.to, top: 5 });

    const curDaily = dailySeries(range, dailyQuery.data, today, todayQuery.data);
    const prevDaily = dailySeries(prev, dailyQuery.data, today, todayQuery.data);
    const hourCurUsage = (hourCur.data?.items ?? []).map(fromLog);
    const hourPrevUsage = (hourPrev.data?.items ?? []).map(fromLog);
    // 「现在」取这份小时数据拿到的时刻：画到哪一格、和昨天比到哪一格，都以它为准。
    const nowCut = isToday && hourCur.data ? currentHourIndex(hourCur.data.items.map((i) => i.start ?? 0), Math.floor(hourCur.dataUpdatedAt / 1000)) : undefined;

    // 卡片的数：来自每日统计（和主页同一口径）。
    const current = summarize(curDaily);
    // 上一段：看今天时只比到昨天的同一钟点（两边都按日志）；其他情况比前一段的每日统计。
    const compare = isToday && nowCut !== undefined && nowCut >= 0 && hourCurUsage.length > 0
        ? { cur: summarize(hourCurUsage.slice(0, nowCut + 1)), prev: summarize(hourPrevUsage.slice(0, nowCut + 1)) }
        : { cur: current, prev: summarize(prevDaily) };
    const vsLabel = isToday ? t('card.vsToday') : oneDay ? t('card.vsOneDay') : t('card.vsDays', { days });
    const costDelta = pctChange(compare.cur.cost, compare.prev.cost);
    const requestDelta = pctChange(compare.cur.requests, compare.prev.requests);

    // 小折线和趋势：按小时用日志，按天用每日统计。
    const bucketSeries: Usage[] = granularity === 'hour' ? (nowCut !== undefined ? hourCurUsage.slice(0, nowCut + 1) : hourCurUsage) : curDaily;
    const spark = {
        requests: bucketSeries.map((u) => u.success + u.failed),
        cost: bucketSeries.map((u) => u.cost),
        tokens: bucketSeries.map((u) => totalTokens(u)),
        cache: bucketSeries.map((u) => { const base = cacheRateBase(u); return base > 0 ? u.cacheHit / base : null; }),
        avg: bucketSeries.map((u) => { const n = u.success + u.failed; return n > 0 ? u.wait / n : null; }),
    };

    const trendPoints = granularity === 'hour'
        ? buildTrend((hourCur.data?.items ?? []).map((i) => i.name.replace(/^0(\d)/, '$1')), hourCurUsage, hourPrevUsage, trendMetric, nowCut)
        : buildTrend(listDates(range).map(shortDate), curDaily, prevDaily, trendMetric);

    const periodLabel = isToday ? t('rangeLabel.today') : range.from === presetRange('yesterday', today).from && oneDay ? t('rangeLabel.yesterday') : oneDay ? t('rangeLabel.oneDay') : t('rangeLabel.days', { days });
    const trendCurrentLabel = isToday ? t('rangeLabel.today') : oneDay ? t('rangeLabel.oneDay') : t('trend.currentDays', { days });
    const trendPreviousLabel = isToday ? t('rangeLabel.yesterday') : oneDay ? t('trend.previousDay') : t('trend.previousDays', { days });

    // 活跃用户：日志里这段时间发过请求的人；总人数和期间新注册来自用户列表。
    const allUsers = usersQuery.data ?? [];
    const activeUsers = userRank.data?.active_users;
    const newUsers = userRank.data
        ? allUsers.filter((u) => (u.created_at ?? 0) >= userRank.data!.start && (u.created_at ?? 0) < userRank.data!.end).length
        : 0;

    const oldest = userRank.data?.oldest_log_time ?? 0;
    const cleaned = logsCleanedBefore(userRank.data?.start, oldest);
    const oldestDate = oldest ? new Date(oldest * 1000) : null;
    const oldestLabel = oldestDate ? `${oldestDate.getFullYear()}-${String(oldestDate.getMonth() + 1).padStart(2, '0')}-${String(oldestDate.getDate()).padStart(2, '0')}` : '';

    const queries = [todayQuery, dailyQuery, hourCur, hourPrev, share, userRank, keyRank, userTrend];
    const refreshing = queries.some((q) => q.isFetching);
    const updatedAt = Math.max(...queries.map((q) => q.dataUpdatedAt || 0));
    const firstError = queries.find((q) => q.error)?.error as Error | undefined;

    const handleRefresh = () => {
        void queryClient.invalidateQueries({ queryKey: ['stats'] });
        void queryClient.invalidateQueries({ queryKey: ['users', 'list'] });
    };

    const goLogs = (intent: { userId?: number; apiKeyId?: number; model?: string; search?: string }) => {
        openLog({ startDate: range.from, endDate: range.to, ...intent });
        setActiveItem('log');
    };
    const goUser = (id: number) => {
        openUser(id);
        setActiveItem('user');
    };

    const openShareItem = (item: StatsBreakdownItem) => {
        if (shareDim === 'model') goLogs({ model: item.name });
        else if (shareDim === 'channel' && item.id > 0) goLogs({ search: item.name });
    };
    const openUserLogs = (item: StatsBreakdownItem) => goLogs(item.id > 0 ? { userId: item.id } : { search: item.name });
    const openKeyLogs = (item: StatsBreakdownItem) => goLogs(item.id > 0 && !item.missing ? { apiKeyId: item.id, userId: item.user_id || undefined } : { search: item.name });

    const selectPreset = (p: Exclude<RangePreset, 'custom'>) => {
        setPreset(p);
        setWantGranularity('hour');
    };
    const applyCustom = (r: DateRange) => {
        const matched = matchPreset(r, today);
        if (matched === 'custom') {
            setCustomRange(r);
            setPreset('custom');
        } else {
            setPreset(matched);
        }
        setWantGranularity('hour');
    };

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-3 pb-24 md:space-y-4 md:pb-4 rounded-t-3xl">
            <div className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 px-1 text-xs text-muted-foreground md:text-sm">
                <span className="hidden md:inline">{t('subtitle')}</span>
                <span className="flex items-center gap-2">
                    {cleaned ? (
                        <span className="inline-flex items-center gap-0.5 rounded-full bg-amber-500/10 px-2 py-0.5 text-amber-700 dark:text-amber-400">
                            {t('cleaned', { date: oldestLabel })}
                            <InfoTip className="text-amber-700/70 dark:text-amber-400/70">{t('cleanedTip')}</InfoTip>
                        </span>
                    ) : userRank.data && !oldest ? (
                        <span>{t('noLogs')}</span>
                    ) : null}
                    {updatedAt > 0 ? <span className="hidden md:inline">{t('updatedAt', { time: new Date(updatedAt).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) })}</span> : null}
                </span>
            </div>

            <RangeBar
                today={today}
                range={range}
                preset={preset === 'custom' ? 'custom' : preset}
                granularity={granularity}
                refreshing={refreshing}
                onPreset={selectPreset}
                onCustom={applyCustom}
                onGranularity={setWantGranularity}
                onRefresh={handleRefresh}
            />

            {firstError ? (
                <div className="rounded-2xl border border-destructive/30 bg-destructive/5 px-4 py-2 text-sm text-destructive">{t('error', { message: firstError.message })}</div>
            ) : null}

            <div className="grid grid-cols-2 gap-3 md:grid-cols-3 lg:grid-cols-6">
                <StatCard
                    icon={MessageSquare}
                    title={t('card.requests')}
                    tip={t('card.requestsTip')}
                    value={formatCountText(current.requests)}
                    sub={<>{t('card.requestsSub', { rate: formatPercentText(current.successRate) })} · <Delta value={requestDelta} /></>}
                    spark={spark.requests}
                    className="order-2 md:order-none"
                />
                <StatCard
                    icon={DollarSign}
                    title={t('card.cost')}
                    tip={`${t('card.costTip')} ${t('card.compareTip')}`}
                    value={formatMoneyText(current.cost)}
                    sub={<>{vsLabel} <Delta value={costDelta} /></>}
                    spark={spark.cost}
                    className="order-1 md:order-none"
                />
                <StatCard
                    icon={Coins}
                    title={t('card.tokens')}
                    tip={t('card.tokensTip')}
                    value={formatCountText(current.tokens)}
                    sub={t('card.tokensSub', { value: formatCountText(current.output) })}
                    spark={spark.tokens}
                    className="order-3 md:order-none"
                />
                <StatCard
                    icon={Percent}
                    title={t('card.cache')}
                    tip={t('card.cacheTip')}
                    value={formatPercentText(current.cacheRate)}
                    sub={t('card.cacheSub', { value: formatCountText(current.cacheHit) })}
                    spark={spark.cache}
                    className="order-4 md:order-none"
                />
                <StatCard
                    icon={UsersIcon}
                    title={t('card.active')}
                    tip={t('card.activeTip')}
                    value={activeUsers === undefined ? '—' : formatCountText(activeUsers)}
                    suffix={allUsers.length ? `/ ${allUsers.length}` : undefined}
                    sub={t('card.activeSub', { count: newUsers })}
                    className="order-5 md:order-none"
                />
                <StatCard
                    icon={Clock3}
                    title={t('card.avg')}
                    tip={t('card.avgTip')}
                    value={formatDurationText(current.avgMs)}
                    sub={t('card.avgSub')}
                    spark={spark.avg}
                    className="order-6 md:order-none"
                />
            </div>

            <div className="grid min-w-0 gap-3 md:gap-4 lg:grid-cols-[minmax(0,1.35fr)_minmax(0,1fr)]">
                <TrendPanel points={trendPoints} metric={trendMetric} onMetric={setTrendMetric} currentLabel={trendCurrentLabel} previousLabel={trendPreviousLabel} />
                <SharePanel data={share.data} dim={shareDim} onDim={setShareDim} periodLabel={periodLabel} onOpen={openShareItem} />
            </div>

            <div className="hidden min-w-0 gap-4 md:grid lg:grid-cols-2">
                <UserRankPanel data={userRank.data} onUser={goUser} onLogs={openUserLogs} onGoto={() => setActiveItem('user')} />
                <KeyRankPanel data={keyRank.data} onUser={goUser} onLogs={openKeyLogs} onGoto={() => setActiveItem('key')} />
            </div>
            <MobileRankPanel users={userRank.data} keys={keyRank.data} onUser={goUser} onUserLogs={openUserLogs} onKeyLogs={openKeyLogs} />

            {!oneDay ? <UserTrendPanel data={userTrend.data} /> : null}
        </PageWrapper>
    );
}
