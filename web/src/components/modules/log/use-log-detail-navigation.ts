import { useCallback, useEffect, useRef, useState } from 'react';

/**
 * 详情弹窗的「左右键连着翻」。
 *
 * 排查日志本质上是**扫**而不是查：看到一条可疑的，往往要顺着往下看旁边几条。
 * 传统做法是关弹窗 → 翻页 → 再点开，三次操作换一条。这里把键盘左右键接上，
 * 翻到一页边界时自动拉下一页，把分页缝掉。
 *
 * 两条刻意的取舍：
 *
 * 1. **不抢输入框的按键**。用户在搜索框里按左键只是移动光标，不能跳日志。
 *    判断方式是看事件源是不是可编辑元素——比给每个输入框加 stopPropagation
 *    可靠得多，因为输入框是随时会新增的。
 *
 * 2. **翻到边界先试试加载，再决定能不能翻**。按钮的可用状态由外部传入的
 *    `hasMore` 决定，但用户按键时不该因为「下一页还没拉」就卡住——所以边界
 *    处先触发一次加载，等数据到了下一次按键就能过去。
 */
export function useLogDetailNavigation({
    logs,
    hasMore,
    loadMore,
    isLoadingMore,
}: {
    logs: { id: number }[];
    hasMore: boolean;
    loadMore: () => void | Promise<unknown>;
    isLoadingMore: boolean;
}) {
    const [activeId, setActiveId] = useState<number | null>(null);
    const requestInFlight = useRef(false);
    // 回调身份保持稳定，日志卡片的默认浅比较才不会因为「列表插了一行」而整表重绘。
    // 调用时读 ref，所以按钮上抓住的仍是打开弹窗时那个函数，看到的却是最新列表。
    const logsRef = useRef(logs);
    const hasMoreRef = useRef(hasMore);
    const loadMoreRef = useRef(loadMore);
    const isLoadingMoreRef = useRef(isLoadingMore);
    const activeIdRef = useRef(activeId);
    useEffect(() => {
        logsRef.current = logs;
        hasMoreRef.current = hasMore;
        loadMoreRef.current = loadMore;
        isLoadingMoreRef.current = isLoadingMore;
        activeIdRef.current = activeId;
    }, [activeId, hasMore, isLoadingMore, loadMore, logs]);

    const index = activeId === null ? -1 : logs.findIndex((log) => log.id === activeId);
    const activeLog = index >= 0 ? logs[index] : null;

    const open = useCallback((id: number) => setActiveId(id), []);
    const close = useCallback((isOpen: boolean) => {
        if (!isOpen) setActiveId(null);
    }, []);

    const canPrev = index > 0;
    // 下一条：本页还有，或者还有下一页可拉。
    const canNext = index >= 0 && (index < logs.length - 1 || hasMore);

    const navigate = useCallback(
        async (direction: -1 | 1) => {
            const logsNow = logsRef.current;
            const active = activeIdRef.current;
            const indexNow = active === null ? -1 : logsNow.findIndex((log) => log.id === active);
            if (indexNow < 0) return;
            const target = indexNow + direction;

            if (target < 0) return;

            if (target >= logsNow.length) {
                // 走到本页末尾：先拉下一页，等它进 logs 后再翻。
                if (!hasMoreRef.current || isLoadingMoreRef.current || requestInFlight.current) return;
                requestInFlight.current = true;
                try {
                    await loadMoreRef.current();
                } finally {
                    requestInFlight.current = false;
                }
                return;
            }

            setActiveId(logsNow[target].id);
        },
        []
    );

    useEffect(() => {
        if (activeId === null) return;

        const onKeyDown = (event: KeyboardEvent) => {
            if (event.key !== 'ArrowLeft' && event.key !== 'ArrowRight') return;
            if (isEditableTarget(event.target)) return;
            // 输入法组合中不响应，否则中文输入选词会被打断。
            if (event.isComposing) return;

            event.preventDefault();
            void navigate(event.key === 'ArrowLeft' ? -1 : 1);
        };

        window.addEventListener('keydown', onKeyDown);
        return () => window.removeEventListener('keydown', onKeyDown);
    }, [activeId, navigate]);

    // 当前这条被实时刷新挤掉时（比如新日志插入导致分页位移），不要盲目跳，
    // 保持 id 不变；找不到就退化成"没有可翻的"，而不是切换到别的日志。
    return {
        activeLog,
        activeId,
        open,
        close,
        navigate,
        canPrev,
        canNext,
    };
}

/** 事件源是不是可以打字的地方。 */
function isEditableTarget(target: EventTarget | null): boolean {
    if (!(target instanceof HTMLElement)) return false;
    const tag = target.tagName.toLowerCase();
    if (tag === 'input' || tag === 'textarea' || tag === 'select') return true;
    return target.isContentEditable;
}
