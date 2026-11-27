'use client';

// 日志页「实时」标签（fix-ui1127a）：正在进行的请求占满整页，不再是折叠面板。
// 数据与 LiveActivityPanel 同一套（SSE 在途状态 + 救援干预），顶部一排筛选
// （用户/Key 管理员可见、渠道、模型、端点），默认按已耗时倒序（开始最早的在最前），
// 请求结束就从列表移走（SSE 状态翻转后不再是 running，自然掉出过滤）。

import { useMemo, useState } from 'react';
import { useTranslations } from 'next-intl';
import { useRequestStateStream } from '@/api/endpoints/log';
import { useAuthStore } from '@/api/endpoints/user';
import { LiveActivityPanel } from './LiveActivityPanel';
import { cn } from '@/lib/utils';

export function LiveTab() {
    const t = useTranslations('log.liveTab');
    const isAdmin = useAuthStore((state) => state.user?.role === 'admin');
    const { states: requestStates, isConnected: requestStateConnected } = useRequestStateStream(true);

    const [userKeyFilter, setUserKeyFilter] = useState('');
    const [channelFilter, setChannelFilter] = useState('');
    const [modelFilter, setModelFilter] = useState('');
    const [endpointFilter, setEndpointFilter] = useState('');

    const filtered = useMemo(() => {
        const userKey = userKeyFilter.trim().toLowerCase();
        const channel = channelFilter.trim().toLowerCase();
        const model = modelFilter.trim().toLowerCase();
        const endpoint = endpointFilter.trim().toLowerCase();
        return requestStates.filter((state) => {
            if (state.status !== 'running') return false;
            if (userKey) {
                const userHit = String(state.user_id ?? '').includes(userKey);
                const keyHit = String(state.api_key_id ?? '').includes(userKey);
                if (!userHit && !keyHit) return false;
            }
            if (channel && !(state.target_channel ?? '').toLowerCase().includes(channel)) return false;
            if (model && !(state.model ?? '').toLowerCase().includes(model) && !(state.target_model ?? '').toLowerCase().includes(model)) return false;
            if (endpoint && !(state.endpoint ?? '').toLowerCase().includes(endpoint)) return false;
            return true;
        });
    }, [requestStates, userKeyFilter, channelFilter, modelFilter, endpointFilter]);

    // 默认按已耗时倒序：开始最早（=跑得最久）的排最前。
    const sorted = useMemo(() => {
        return [...filtered].sort((a, b) => {
            const ta = new Date(a.started_at).getTime();
            const tb = new Date(b.started_at).getTime();
            return ta - tb;
        });
    }, [filtered]);

    const inputClass = 'h-9 min-w-0 flex-1 rounded-lg border border-input bg-background px-2 text-xs text-foreground outline-none placeholder:text-muted-foreground sm:flex-none sm:w-36';

    return (
        <div className="flex min-h-0 flex-1 flex-col gap-3">
            <div className="flex flex-none flex-wrap items-center gap-2 rounded-2xl border border-border bg-card p-3 shadow-sm sm:px-4">
                {isAdmin && (
                    <input
                        type="text"
                        value={userKeyFilter}
                        onChange={(event) => setUserKeyFilter(event.target.value)}
                        placeholder={t('filterUserKey')}
                        aria-label={t('filterUserKey')}
                        className={inputClass}
                    />
                )}
                <input
                    type="text"
                    value={channelFilter}
                    onChange={(event) => setChannelFilter(event.target.value)}
                    placeholder={t('filterChannel')}
                    aria-label={t('filterChannel')}
                    className={inputClass}
                />
                <input
                    type="text"
                    value={modelFilter}
                    onChange={(event) => setModelFilter(event.target.value)}
                    placeholder={t('filterModel')}
                    aria-label={t('filterModel')}
                    className={inputClass}
                />
                <input
                    type="text"
                    value={endpointFilter}
                    onChange={(event) => setEndpointFilter(event.target.value)}
                    placeholder={t('filterEndpoint')}
                    aria-label={t('filterEndpoint')}
                    className={inputClass}
                />
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
