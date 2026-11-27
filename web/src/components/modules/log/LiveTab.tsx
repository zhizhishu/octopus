'use client';

// 日志页「实时」标签（fix-ui1127a）：正在进行的请求占满整页，不再是折叠面板。
// 数据与 LiveActivityPanel 同一套（SSE 在途状态 + 救援干预），顶部一排下拉筛选
// （fix-ui1127a-r1 第 2 条：与请求日志同风格的下拉框，选项取当前在途集合里出现过
// 的用户/Key、渠道、模型、端点——文本框让人猜值，下拉直接列实际存在的值）。
// 默认按已耗时倒序（开始最早的在最前），请求结束就从列表移走。

import { useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { useRequestStateStream } from '@/api/endpoints/log';
import { useAuthStore } from '@/api/endpoints/user';
import { LiveActivityPanel } from './LiveActivityPanel';
import { cn } from '@/lib/utils';

function uniq(values: (string | number | undefined)[]): string[] {
    const set = new Set<string>();
    for (const v of values) {
        if (v === undefined || v === null || v === '') continue;
        set.add(String(v));
    }
    return [...set].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
}

export function LiveTab() {
    const t = useTranslations('log.liveTab');
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');
    const { states: requestStates, isConnected: requestStateConnected } = useRequestStateStream(true);

    const [userKeyFilter, setUserKeyFilter] = useState('');
    const [channelFilter, setChannelFilter] = useState('');
    const [modelFilter, setModelFilter] = useState('');
    const [endpointFilter, setEndpointFilter] = useState('');

    const running = useMemo(() => requestStates.filter((state) => state.status === 'running'), [requestStates]);

    // 下拉选项来自当前在途集合：用户/Key 合并为一个下拉（u:/k: 前缀区分），其余各一列。
    const userKeyOptions = useMemo(() => {
        if (!isAdmin) return [];
        const users = uniq(running.map((s) => s.user_id)).map((id) => ({ value: `u:${id}`, label: `${t('userPrefix')} ${id}` }));
        const keys = uniq(running.map((s) => s.api_key_id)).map((id) => ({ value: `k:${id}`, label: `Key ${id}` }));
        return [...users, ...keys];
    }, [isAdmin, running, t]);
    const channelOptions = useMemo(() => uniq(running.map((s) => s.target_channel)), [running]);
    const modelOptions = useMemo(() => uniq(running.flatMap((s) => [s.model, s.target_model])), [running]);
    const endpointOptions = useMemo(() => uniq(running.map((s) => s.endpoint)), [running]);

    const filtered = useMemo(() => {
        return running.filter((state) => {
            if (userKeyFilter) {
                const [kind, id] = userKeyFilter.split(':');
                if (kind === 'u' && String(state.user_id ?? '') !== id) return false;
                if (kind === 'k' && String(state.api_key_id ?? '') !== id) return false;
            }
            if (channelFilter && state.target_channel !== channelFilter) return false;
            if (modelFilter && state.model !== modelFilter && state.target_model !== modelFilter) return false;
            if (endpointFilter && state.endpoint !== endpointFilter) return false;
            return true;
        });
    }, [running, userKeyFilter, channelFilter, modelFilter, endpointFilter]);

    // 默认按已耗时倒序：开始最早（=跑得最久）的排最前。
    const sorted = useMemo(() => {
        return [...filtered].sort((a, b) => {
            const ta = new Date(a.started_at).getTime();
            const tb = new Date(b.started_at).getTime();
            return ta - tb;
        });
    }, [filtered]);

    const selectClass = 'h-9 min-w-0 flex-1 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none sm:flex-none sm:w-36';

    return (
        <div className="flex min-h-0 flex-1 flex-col gap-3">
            <div className="flex flex-none flex-wrap items-center gap-2 rounded-2xl border border-border bg-card p-3 shadow-sm sm:px-4">
                {isAdmin && (
                    <select value={userKeyFilter} onChange={(event) => setUserKeyFilter(event.target.value)} aria-label={t('filterUserKey')} className={selectClass}>
                        <option value="">{t('filterUserKey')}</option>
                        {userKeyOptions.map((opt) => <option key={opt.value} value={opt.value}>{opt.label}</option>)}
                    </select>
                )}
                <select value={channelFilter} onChange={(event) => setChannelFilter(event.target.value)} aria-label={t('filterChannel')} className={selectClass}>
                    <option value="">{t('filterChannel')}</option>
                    {channelOptions.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
                <select value={modelFilter} onChange={(event) => setModelFilter(event.target.value)} aria-label={t('filterModel')} className={selectClass}>
                    <option value="">{t('filterModel')}</option>
                    {modelOptions.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
                <select value={endpointFilter} onChange={(event) => setEndpointFilter(event.target.value)} aria-label={t('filterEndpoint')} className={selectClass}>
                    <option value="">{t('filterEndpoint')}</option>
                    {endpointOptions.map((v) => <option key={v} value={v}>{v}</option>)}
                </select>
                <span className={cn('text-xs tabular-nums text-muted-foreground')}>
                    {t('inFlight', { count: sorted.length })}
                </span>
            </div>
            <div className="min-h-0 flex-1">
                <LiveActivityPanel
                    isAdmin={isAdmin}
                    enabled
                    fullPage
                    requestStates={sorted}
                    requestStateConnected={requestStateConnected}
                    historyLogIDs={new Set()}
                />
            </div>
        </div>
    );
}
