'use client';

import { useTranslations } from 'next-intl';
import { Info, Tag, Github, AlertTriangle, Download, Loader2, ExternalLink, RefreshCw } from 'lucide-react';
import { APP_VERSION, GITHUB_REPO } from '@/lib/info';
import { useQueryClient } from '@tanstack/react-query';
import { useBuildInfo, useFutureLatestInfo, useLatestInfo, useNowVersion, useUpdateCore } from '@/api/endpoints/update';
import { Button } from '@/components/ui/button';
import { toast } from '@/components/common/Toast';
import { isOctopusCacheName, isFontCacheName, SW_MESSAGE_TYPE } from '@/lib/sw';

function compactValue(value?: string) {
    if (!value || value === 'unknown') return '';
    return value;
}

function formatDateTime(value?: string) {
    if (!value || value === 'unknown') return '';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return value;
    return date.toLocaleString();
}

export function SettingInfo() {
    const t = useTranslations('setting');
    const queryClient = useQueryClient();
    const latestInfoQuery = useLatestInfo();
    const nowVersionQuery = useNowVersion();
    const buildInfoQuery = useBuildInfo();
    const buildInfo = buildInfoQuery.data;
    const isFutureBuild = !!(buildInfo?.future_build || latestInfoQuery.data?.future_build);
    const futureLatestQuery = useFutureLatestInfo(isFutureBuild);
    const updateCore = useUpdateCore();

    const latestInfo = latestInfoQuery.data;
    const backendNowVersion = nowVersionQuery.data || '';
    const futureLatest = futureLatestQuery.data;
    const currentDisplayVersion = buildInfo?.display_version || backendNowVersion || '';
    const officialLatestVersion = latestInfo?.tag_name || '';
    const latestDisplayVersion = isFutureBuild
        ? (futureLatest?.commit_short ? `future:${futureLatest.commit_short}` : '')
        : officialLatestVersion;
    const currentImage = buildInfo?.image || (isFutureBuild ? 'ghcr.io/zhizhishu/octopus:future' : '');
    const updateUrl = futureLatest?.html_url || latestInfo?.update_url || latestInfo?.update_repo || GITHUB_REPO;
    const currentBuildTime = formatDateTime(buildInfo?.build_time);
    const latestBuildTime = formatDateTime(futureLatest?.updated_at || latestInfo?.published_at);
    const hasKnownFutureLatest = !!futureLatest?.commit_short;

    // 前端版本与后端当前版本不一致 → 浏览器缓存问题
    const isCacheMismatch = !!backendNowVersion && APP_VERSION !== 'unknown' && backendNowVersion !== APP_VERSION;
    // 最新版本与后端当前版本不一致 → 有新版本可更新
    const hasNewVersion = isFutureBuild
        ? !!futureLatest?.update_available
        : Boolean(officialLatestVersion && backendNowVersion && officialLatestVersion !== backendNowVersion);

    const clearCacheAndReload = async () => {
        // 通知 Service Worker 清理缓存
        if ('serviceWorker' in navigator && navigator.serviceWorker.controller) {
            navigator.serviceWorker.controller.postMessage({ type: SW_MESSAGE_TYPE.CLEAR_CACHE });
        }
        // 同时也从主线程清理（双保险），但保留字体缓存
        if ('caches' in window) {
            const names = await caches.keys();
            await Promise.all(
                names
                    .filter((name) => isOctopusCacheName(name) && !isFontCacheName(name))
                    .map((name) => caches.delete(name))
            );
        }
        // 注销当前 SW，下次加载会重新注册
        if ('serviceWorker' in navigator) {
            const registrations = await navigator.serviceWorker.getRegistrations();
            await Promise.all(registrations.map((reg) => reg.unregister()));
        }
        // 强制刷新（跳过缓存）
        window.location.reload();
    };

    const handleForceRefresh = () => {
        clearCacheAndReload();
    };

    const handleUpdate = () => {
        updateCore.mutate(undefined, {
            onSuccess: () => {
                toast.success(t('info.updateSuccess'));
                // 更新成功后清理缓存并刷新
                setTimeout(() => {
                    clearCacheAndReload();
                }, 1500);
            },
            onError: () => {
                toast.error(t('info.updateFailed'));
            }
        });
    };

    const handleCheckUpdate = async () => {
        await Promise.all([
            latestInfoQuery.refetch(),
            nowVersionQuery.refetch(),
            buildInfoQuery.refetch(),
            isFutureBuild ? futureLatestQuery.refetch() : Promise.resolve(),
        ]);
        await queryClient.invalidateQueries({ queryKey: ['update'] });
        toast.success(t('info.checkSuccess'));
    };

    return (
        <div className="rounded-3xl border border-border bg-card p-6 space-y-5">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
                <h2 className="text-lg font-bold text-card-foreground flex items-center gap-2">
                    <Info className="h-5 w-5" />
                    {t('info.title')}
                </h2>
                <Button
                    variant="outline"
                    size="sm"
                    onClick={handleCheckUpdate}
                    disabled={latestInfoQuery.isFetching || buildInfoQuery.isFetching || futureLatestQuery.isFetching}
                    className="rounded-xl self-start sm:self-auto"
                >
                    {(latestInfoQuery.isFetching || buildInfoQuery.isFetching || futureLatestQuery.isFetching) ? (
                        <Loader2 className="size-4 animate-spin" />
                    ) : (
                        <RefreshCw className="size-4" />
                    )}
                    {t('info.checkUpdate')}
                </Button>
            </div>
            {/* GitHub 仓库 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Github className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.github')}</span>
                </div>
                <a
                    href={GITHUB_REPO}
                    target="_blank"
                    rel="noopener noreferrer"
                    className="text-sm text-primary hover:underline"
                >
                    {GITHUB_REPO.replace('https://github.com/', '')}
                </a>
            </div>
            {/* 版本来源 */}
            {latestInfo?.source_repo && (
                <div className="flex items-center justify-between gap-4">
                    <div className="flex items-center gap-3">
                        <Tag className="h-5 w-5 text-muted-foreground" />
                        <span className="text-sm font-medium">{t('info.versionSource')}</span>
                    </div>
                    <a
                        href={latestInfo.source_repo}
                        target="_blank"
                        rel="noopener noreferrer"
                        className="text-sm text-primary hover:underline"
                    >
                        {latestInfo.source_repo.replace('https://github.com/', '')}
                    </a>
                </div>
            )}
            {/* 当前版本 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Tag className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.currentVersion')}</span>
                </div>
                <div className="flex min-w-0 flex-col items-end gap-1 text-right">
                    {nowVersionQuery.isLoading || buildInfoQuery.isLoading ? (
                        <Loader2 className="size-4 animate-spin text-muted-foreground" />
                    ) : (
                        <>
                            <code className="max-w-[52vw] truncate text-sm font-mono text-muted-foreground sm:max-w-none">
                                {currentDisplayVersion || t('info.unknown')}
                            </code>
                            {currentImage && (
                                <code className="max-w-[52vw] truncate text-xs font-mono text-muted-foreground sm:max-w-none">
                                    {currentImage}
                                </code>
                            )}
                            {compactValue(buildInfo?.commit) && (
                                <span className="text-xs text-muted-foreground">
                                    {t('info.commit')}: {buildInfo?.commit}
                                </span>
                            )}
                        </>
                    )}
                </div>
            </div>

            {/* 最新版本 */}
            <div className="flex items-center justify-between gap-4">
                <div className="flex items-center gap-3">
                    <Download className="h-5 w-5 text-muted-foreground" />
                    <span className="text-sm font-medium">{t('info.latestVersion')}</span>
                </div>
                <div className="flex min-w-0 flex-col items-end gap-1 text-right">
                    {latestInfoQuery.isLoading || (isFutureBuild && futureLatestQuery.isLoading) ? (
                        <Loader2 className="size-4 animate-spin text-muted-foreground" />
                    ) : (
                        <>
                            <code className="max-w-[52vw] truncate text-sm font-mono text-muted-foreground sm:max-w-none">
                                {latestDisplayVersion || t('info.unknown')}
                            </code>
                            {isFutureBuild && hasKnownFutureLatest && (
                                <a
                                    href={futureLatest?.html_url}
                                    target="_blank"
                                    rel="noopener noreferrer"
                                    className="inline-flex items-center gap-1 text-xs text-primary hover:underline"
                                >
                                    #{futureLatest?.run_number}
                                    <ExternalLink className="size-3" />
                                </a>
                            )}
                            {isFutureBuild && futureLatest?.message && (
                                <span className="text-xs text-muted-foreground">{futureLatest.message}</span>
                            )}
                        </>
                    )}
                </div>
            </div>

            {(currentBuildTime || latestBuildTime) && (
                <div className="space-y-2 border-t border-border pt-4">
                    {currentBuildTime && (
                        <div className="flex items-center justify-between gap-4 text-sm">
                            <span className="text-muted-foreground">{t('info.currentBuildTime')}</span>
                            <span className="text-right text-muted-foreground">{currentBuildTime}</span>
                        </div>
                    )}
                    {latestBuildTime && (
                        <div className="flex items-center justify-between gap-4 text-sm">
                            <span className="text-muted-foreground">{isFutureBuild ? t('info.latestBuildTime') : t('info.publishedAt')}</span>
                            <span className="text-right text-muted-foreground">{latestBuildTime}</span>
                        </div>
                    )}
                </div>
            )}

            {/* 浏览器缓存问题警告 */}
            {isCacheMismatch && (
                <div className="p-3 bg-destructive/10 border border-destructive/20 rounded-xl space-y-2">
                    <div className="flex items-start gap-3">
                        <AlertTriangle className="h-5 w-5 text-destructive shrink-0 mt-0.5" />
                        <div className="flex-1 space-y-1">
                            <p className="text-sm text-destructive font-medium">
                                {t('info.versionMismatch')}
                            </p>
                            <p className="text-xs text-muted-foreground">
                                {t('info.versionMismatchHint', { frontend: APP_VERSION, backend: backendNowVersion })}
                            </p>
                        </div>
                    </div>
                    <div className="flex justify-end">
                        <Button
                            variant="destructive"
                            size="sm"
                            onClick={handleForceRefresh}
                            className="rounded-xl"
                        >
                            {t('info.forceRefresh')}
                        </Button>
                    </div>
                </div>
            )}

            {/* 有新版本可更新 */}
            {hasNewVersion && (
                <div className="p-3 bg-primary/10 border border-primary/20 rounded-xl space-y-2">
                    <div className="flex items-start gap-3">
                        <Download className="h-5 w-5 text-primary shrink-0 mt-0.5" />
                        <div className="flex-1 space-y-1">
                            <p className="text-sm text-primary font-medium">
                                {t('info.newVersionAvailable')}
                            </p>
                            <p className="text-xs text-muted-foreground">
                                {isFutureBuild ? t('info.futureUpdateHint') : t('info.newVersionAvailableHint')}
                            </p>
                        </div>
                    </div>
                    <div className="flex justify-end">
                        {isFutureBuild ? (
                            <Button asChild variant="default" size="sm" className="rounded-xl">
                                <a href={updateUrl} target="_blank" rel="noopener noreferrer">
                                    <ExternalLink className="size-4" />
                                    {t('info.openFutureUpdate')}
                                </a>
                            </Button>
                        ) : (
                            <Button
                                variant="default"
                                size="sm"
                                onClick={handleUpdate}
                                disabled={updateCore.isPending}
                                className="rounded-xl"
                            >
                                {updateCore.isPending ? t('info.updating') : t('info.updateNow')}
                            </Button>
                        )}
                    </div>
                </div>
            )}
        </div>
    );
}

