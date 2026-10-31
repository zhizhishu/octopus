'use client';

import { useCallback, useEffect, useState } from 'react';
import {
    LOG_AUTO_REFRESH_STORAGE_KEY,
    logAutoRefreshPauseReason,
    parseLogAutoRefresh,
    type LogAutoRefresh,
    type LogAutoRefreshPauseReason,
} from '@/lib/log-auto-refresh';

function readStoredMode(): LogAutoRefresh {
    if (typeof window === 'undefined') return 'off';
    try {
        return parseLogAutoRefresh(window.localStorage.getItem(LOG_AUTO_REFRESH_STORAGE_KEY));
    } catch {
        return 'off';
    }
}

/** 页面里是否开着任何弹窗/浮层（日志详情、重试明细 Popover、日历……都挂在 body 下且带 role=dialog）。 */
function anyDialogOpen(): boolean {
    return typeof document !== 'undefined' && document.querySelector('[role="dialog"],[role="alertdialog"]') !== null;
}

function hasTextSelection(): boolean {
    if (typeof window === 'undefined') return false;
    const selection = window.getSelection();
    return !!selection && !selection.isCollapsed && selection.toString().trim().length > 0;
}

/**
 * 日志页自动刷新：本地记住所选节奏，并实时算出「现在该不该往列表里插新行」。
 * 暂停条件：标签页在后台 / 详情或任何弹窗开着 / 正在选文字 / 列表已往下翻。
 * 只读观察 DOM，不改任何请求；关着（off）时不挂任何监听。
 */
export function useLogAutoRefresh(options: {
    /** 列表滚动容器（VirtualizedGrid 的 scrollerRef）。 */
    scrollerRef: { current: HTMLDivElement | null };
    /** 额外的「弹窗开着」信号；详情抽屉本身带 role=dialog，DOM 观察已能覆盖。 */
    detailOpen?: boolean;
}) {
    const { detailOpen = false, scrollerRef } = options;
    const [mode, setModeState] = useState<LogAutoRefresh>(readStoredMode);
    const setMode = useCallback((next: LogAutoRefresh) => {
        setModeState(next);
        try {
            window.localStorage.setItem(LOG_AUTO_REFRESH_STORAGE_KEY, next);
        } catch {
            // 隐私模式等写不进去：本次会话仍按所选节奏生效。
        }
    }, []);

    const [signals, setSignals] = useState({ hidden: false, dialogOpen: false, selecting: false, scrollTop: 0 });
    const enabled = mode !== 'off';

    useEffect(() => {
        if (!enabled) return;
        const read = () => {
            const next = {
                hidden: document.visibilityState === 'hidden',
                dialogOpen: anyDialogOpen(),
                selecting: hasTextSelection(),
                scrollTop: scrollerRef.current?.scrollTop ?? 0,
            };
            setSignals((prev) =>
                prev.hidden === next.hidden &&
                prev.dialogOpen === next.dialogOpen &&
                prev.selecting === next.selecting &&
                // 滚动只关心「离没离顶」，不为每个像素重渲染。
                (prev.scrollTop > 8) === (next.scrollTop > 8)
                    ? prev
                    : next
            );
        };
        // 订阅外部事件，回调里同步 DOM 状态（首次也走一次异步回调，不在 effect 体里 setState）。
        const first = window.setTimeout(read, 0);
        document.addEventListener('visibilitychange', read);
        document.addEventListener('selectionchange', read);
        // 列表滚动容器可能晚于本 hook 挂载 / 被替换：在捕获阶段统一听 scroll，读当前容器。
        document.addEventListener('scroll', read, { capture: true, passive: true });
        // 弹窗/浮层都 portal 到 body 直接子节点：只看 body 的子节点增减，不做全树观察。
        const observer = new MutationObserver(read);
        observer.observe(document.body, { childList: true });
        return () => {
            window.clearTimeout(first);
            document.removeEventListener('visibilitychange', read);
            document.removeEventListener('selectionchange', read);
            document.removeEventListener('scroll', read, { capture: true });
            observer.disconnect();
        };
    }, [enabled, scrollerRef]);

    const pauseReason: LogAutoRefreshPauseReason | null = enabled
        ? logAutoRefreshPauseReason({ ...signals, dialogOpen: detailOpen || signals.dialogOpen })
        : null;

    return { mode, setMode, enabled, pauseReason };
}
