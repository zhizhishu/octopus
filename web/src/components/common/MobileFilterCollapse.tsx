'use client';

import { type ReactNode, useState } from 'react';
import { ChevronDown, ChevronUp, SlidersHorizontal } from 'lucide-react';
import { Badge } from '@/components/ui/badge';
import { buttonVariants } from '@/components/ui/button';
import { cn } from '@/lib/utils';

/**
 * 手机端把「次要的顶部控制面板」折叠成一个可点开的紧凑条，让主内容（列表/画布）拿到屏幕高度。
 *
 * - 手机（< sm）：默认只显示一个「筛选」开关（可带生效计数徽标）+ 可选的 trailing 常驻控件；
 *   点开才把完整控件就地展开。收起时高度让给列表。
 * - 桌面（sm:+）：开关隐藏，children 始终内联展示，布局与改造前完全一致（不动桌面）。
 *
 * children 即原来的完整控件；调用方把「生效筛选药丸 / 计数 / 分页」这类想常驻的摘要放在本组件之外。
 */
export function MobileFilterCollapse({
    label,
    activeCount = 0,
    trailing,
    layout = 'stack',
    contentClassName,
    children,
}: {
    /** 开关上的文案（如「筛选」）。 */
    label: ReactNode;
    /** 生效筛选数量；> 0 时在开关上显示徽标。 */
    activeCount?: number;
    /** 手机端开关同一行右侧常驻的控件/摘要（如刷新按钮、最后更新时间）。 */
    trailing?: ReactNode;
    /** 展开区排版：'stack' = flex 纵向 gap-2（多个控件块）；'plain' = 保留 children 自身间距。 */
    layout?: 'stack' | 'plain';
    contentClassName?: string;
    children: ReactNode;
}) {
    const [open, setOpen] = useState(false);

    const contentBase = layout === 'plain'
        ? cn(open ? 'block' : 'hidden', 'sm:block')
        : cn(open ? 'flex' : 'hidden', 'flex-col gap-2 sm:flex');

    return (
        <>
            <div className="flex items-center gap-2 sm:hidden">
                <button
                    type="button"
                    aria-expanded={open}
                    onClick={() => setOpen((v) => !v)}
                    className={buttonVariants({ variant: 'outline', size: 'sm', className: 'rounded-xl' })}
                >
                    <SlidersHorizontal className="size-4" />
                    <span className="truncate">{label}</span>
                    {activeCount > 0 && (
                        <Badge variant="secondary" className="h-5 min-w-5 justify-center px-1 text-[10px]">
                            {activeCount}
                        </Badge>
                    )}
                    {open ? <ChevronUp className="size-4" /> : <ChevronDown className="size-4" />}
                </button>
                {trailing}
            </div>
            <div className={cn('min-w-0', contentBase, contentClassName)}>
                {children}
            </div>
        </>
    );
}
