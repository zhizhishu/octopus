'use client';

import {
    type ReactNode,
    useCallback,
    useEffect,
    useMemo,
    useRef,
    useState,
} from 'react';
import { useVirtualizer } from '@tanstack/react-virtual';

const BREAKPOINTS = {
    sm: 640,
    md: 768,
    lg: 960,
    xl: 1280,
    '2xl': 1536,
} as const;

type Breakpoint = keyof typeof BREAKPOINTS;
type ResponsiveColumns = Partial<Record<Breakpoint | 'default', number>>;

// 列表元素数 ≤ 此阈值时改为「平铺渲染」，绕开虚拟化。
// 原因：变高行 + 列表实时增删（日志 SSE 往头部插入、翻页刷新）会让 @tanstack/react-virtual
// 的「按 key 测量缓存」残留旧 key 的测量项，getVirtualItems() 返回重复 index 的虚拟行，
// 两套行被绝对定位叠在一起 → 表现为「文字重叠 / 看着像乱码」。日志/渠道等页每页仅几十条，
// 平铺渲染零测量、绝不重叠；虚拟化只在真·大列表（> 阈值）时启用。
const NON_VIRTUAL_MAX = 80;

interface VirtualizedGridProps<T> {
    items: T[];
    layout?: 'grid' | 'list';
    columns: ResponsiveColumns;
    estimateItemHeight: number;
    gap?: number;
    overscan?: number;
    getItemKey: (item: T, index: number) => string | number;
    renderItem: (item: T, index: number) => ReactNode;
    footer?: ReactNode;
    onReachEnd?: () => void;
    reachEndEnabled?: boolean;
    reachEndOffset?: number;
}

function getColumnsForWidth(
    width: number,
    columns: ResponsiveColumns,
): number {
    if (width >= BREAKPOINTS['2xl'] && columns['2xl'] !== undefined) return columns['2xl'];
    if (width >= BREAKPOINTS.xl && columns.xl !== undefined) return columns.xl;
    if (width >= BREAKPOINTS.lg && columns.lg !== undefined) return columns.lg;
    if (width >= BREAKPOINTS.md && columns.md !== undefined) return columns.md;
    if (width >= BREAKPOINTS.sm && columns.sm !== undefined) return columns.sm;
    return columns.default ?? 1;
}

export function VirtualizedGrid<T>({
    items,
    layout = 'grid',
    columns,
    estimateItemHeight,
    gap = 16,
    overscan = 4,
    getItemKey,
    renderItem,
    footer = null,
    onReachEnd,
    reachEndEnabled = false,
    reachEndOffset = 1,
}: VirtualizedGridProps<T>) {
    'use no memo';

    const [containerWidth, setContainerWidth] = useState(() =>
        typeof window === 'undefined' ? 1024 : window.innerWidth
    );
    const containerRef = useRef<HTMLDivElement | null>(null);
    const reachEndTriggeredRef = useRef(false);

    useEffect(() => {
        const el = containerRef.current;
        if (!el) return;

        const updateWidth = () => {
            const nextWidth = el.clientWidth;
            setContainerWidth((prev) => (prev === nextWidth ? prev : nextWidth));
        };

        updateWidth();

        if (typeof ResizeObserver === 'undefined') return;
        const observer = new ResizeObserver(updateWidth);
        observer.observe(el);

        return () => {
            observer.disconnect();
        };
    }, []);

    const columnCount = useMemo(() => {
        if (layout === 'list') return 1;
        return Math.max(1, getColumnsForWidth(containerWidth, columns));
    }, [layout, containerWidth, columns]);

    const itemRowCount = useMemo(
        () => (items.length === 0 ? 0 : Math.ceil(items.length / columnCount)),
        [items.length, columnCount]
    );
    const hasFooterRow = footer !== null;
    const rowCount = itemRowCount + (hasFooterRow ? 1 : 0);
    const getVirtualRowKey = useCallback((rowIndex: number) => {
        if (hasFooterRow && rowIndex === itemRowCount) {
            return '__virtual-footer__';
        }

        const rowStartIndex = rowIndex * columnCount;
        const firstItem = items[rowStartIndex];
        if (!firstItem) return `row-empty-${rowIndex}`;

        // Keep row keys stable across prepend/append updates (especially log stream updates),
        // otherwise virtualizer measurements are constantly invalidated and spacing falls back to estimates.
        return `row-${String(getItemKey(firstItem, rowStartIndex))}`;
    }, [hasFooterRow, itemRowCount, columnCount, items, getItemKey]);

    // eslint-disable-next-line react-hooks/incompatible-library
    const rowVirtualizer = useVirtualizer({
        count: rowCount,
        getScrollElement: () => containerRef.current,
        getItemKey: getVirtualRowKey,
        estimateSize: () => estimateItemHeight + gap,
        // Use layout height (not transformed visual height) to avoid scale-animation
        // shrinking measurements during page enter transitions.
        measureElement: (element) =>
            element instanceof HTMLElement
                ? element.offsetHeight
                : element.getBoundingClientRect().height,
        overscan,
    });

    const virtualRows = rowVirtualizer.getVirtualItems();

    useEffect(() => {
        if (!onReachEnd || !reachEndEnabled || itemRowCount === 0) return;

        const lastVirtualIndex = virtualRows.length > 0 ? virtualRows[virtualRows.length - 1]!.index : -1;
        const triggerIndex = Math.max(0, itemRowCount - 1 - reachEndOffset);
        if (lastVirtualIndex < triggerIndex) {
            reachEndTriggeredRef.current = false;
            return;
        }
        if (reachEndTriggeredRef.current) return;

        reachEndTriggeredRef.current = true;
        onReachEnd();
    }, [onReachEnd, reachEndEnabled, itemRowCount, reachEndOffset, virtualRows]);

    // 小列表走平铺渲染（见 NON_VIRTUAL_MAX 注释）：彻底避开虚拟化的测量-缓存腐坏，绝不重叠。
    const shouldVirtualize = items.length > NON_VIRTUAL_MAX;

    // 平铺模式下用滚动位置近似触发「到底加载更多」，行为对齐虚拟模式的 onReachEnd。
    const handlePlainScroll = useCallback(() => {
        const el = containerRef.current;
        if (!el || !onReachEnd || !reachEndEnabled) return;
        const remaining = el.scrollHeight - el.scrollTop - el.clientHeight;
        if (remaining <= Math.max(1, reachEndOffset) * estimateItemHeight) {
            if (reachEndTriggeredRef.current) return;
            reachEndTriggeredRef.current = true;
            onReachEnd();
        } else {
            reachEndTriggeredRef.current = false;
        }
    }, [onReachEnd, reachEndEnabled, reachEndOffset, estimateItemHeight]);

    if (!shouldVirtualize) {
        return (
            <div className="relative h-full min-h-0 w-full">
                <div
                    ref={containerRef}
                    onScroll={handlePlainScroll}
                    className="relative h-full w-full overflow-y-auto overscroll-contain rounded-t-3xl"
                >
                    {items.length > 0 && (
                        <div
                            className="grid"
                            style={{
                                gridTemplateColumns: `repeat(${columnCount}, minmax(0, 1fr))`,
                                gap: `${gap}px`,
                            }}
                        >
                            {items.map((item, index) => (
                                <div key={String(getItemKey(item, index))} className="min-w-0">
                                    {renderItem(item, index)}
                                </div>
                            ))}
                        </div>
                    )}
                    {footer}
                </div>
            </div>
        );
    }

    return (
        <div className="relative h-full min-h-0 w-full">
            <div
                ref={containerRef}
                className="relative h-full w-full overflow-y-auto overscroll-contain rounded-t-3xl"
            >
                {rowCount === 0 ? null : (
                    <div className="relative w-full" style={{ height: `${rowVirtualizer.getTotalSize()}px` }}>
                        {virtualRows.map((virtualRow) => {
                            if (hasFooterRow && virtualRow.index === itemRowCount) {
                                return (
                                    <div
                                        key={virtualRow.key}
                                        data-index={virtualRow.index}
                                        ref={rowVirtualizer.measureElement}
                                        className="absolute left-0 top-0 w-full"
                                        style={{
                                            transform: `translateY(${virtualRow.start}px)`,
                                        }}
                                    >
                                        {footer}
                                    </div>
                                );
                            }

                            const rowStartIndex = virtualRow.index * columnCount;
                            const rowEndIndex = Math.min(rowStartIndex + columnCount, items.length);
                            const rowItems = items.slice(rowStartIndex, rowEndIndex);
                            const rowPaddingBottom = virtualRow.index === itemRowCount - 1 && !hasFooterRow ? 0 : gap;

                            return (
                                <div
                                    key={virtualRow.key}
                                    data-index={virtualRow.index}
                                    ref={rowVirtualizer.measureElement}
                                    className="absolute left-0 top-0 w-full"
                                    style={{
                                        transform: `translateY(${virtualRow.start}px)`,
                                    }}
                                >
                                    <div
                                        className="grid"
                                        style={{
                                            gridTemplateColumns: `repeat(${columnCount}, minmax(0, 1fr))`,
                                            gap: `${gap}px`,
                                            paddingBottom: `${rowPaddingBottom}px`,
                                        }}
                                    >
                                        {rowItems.map((item, columnIndex) => {
                                            const itemIndex = rowStartIndex + columnIndex;
                                            return (
                                                <div key={String(getItemKey(item, itemIndex))} className="min-w-0">
                                                    {renderItem(item, itemIndex)}
                                                </div>
                                            );
                                        })}
                                    </div>
                                </div>
                            );
                        })}
                    </div>
                )}
            </div>
        </div>
    );
}
