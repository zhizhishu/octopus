import { useState, type ReactNode } from 'react';
import { Dialog, DialogContent, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { cn } from '@/lib/utils';

/**
 * 移动端可展开的"完整值"。
 *
 * 解决的问题：卡片上大量字段用 `title=` 承载完整内容，鼠标悬停能看到，**触屏
 * 上根本打不开**——手机上这些信息等于不存在。这里把 title 换成"点一下从底部
 * 升起抽屉"，桌面端保持原样（hover 更好用，不该被点开打扰）。
 *
 * 不做 `isMobile` 判断：靠 CSS 分叉。`hidden md:inline` 的浏览器原生 title 与
 * `md:hidden` 的触发按钮互斥，服务端渲染也没有水合不一致的风险。
 *
 * 尺寸按 44px 触控标准（iOS HIG / Material 都定这个数），底部留 safe-area，
 * 否则 iPhone 的 Home 指示条会压在按钮上。
 */
export function ExpandableValue({
    children,
    label,
    value,
    className,
}: {
    /** 触发区域里显示的内容，通常是截断后的文本。 */
    children: ReactNode;
    /** 抽屉标题，说明这个值是什么。 */
    label: string;
    /** 完整值。为空时不渲染抽屉（没什么可展开的）。 */
    value: string | undefined | null;
    className?: string;
}) {
    const [open, setOpen] = useState(false);

    if (!value) return <>{children}</>;

    return (
        <>
            {/* 桌面：保留原生 title，悬停即见，不需要交互成本。 */}
            <span className={cn('hidden md:inline', className)} title={value}>
                {children}
            </span>
            {/* 移动：点开抽屉。包成 button 是为了有可聚焦语义，不是装饰。 */}
            <button
                type="button"
                onClick={(event) => {
                    // 卡片整体可点开详情，这里必须拦住，否则点全文会同时打开详情弹窗。
                    event.stopPropagation();
                    setOpen(true);
                }}
                className={cn(
                    'inline-flex min-h-11 min-w-0 items-center text-left md:hidden',
                    className
                )}
            >
                {children}
            </button>
            <Dialog open={open} onOpenChange={setOpen}>
                <DialogContent
                    // 底部抽屉：贴底、只圆上方两角、限高可滚、底部让出安全区。
                    className={cn(
                        'max-sm:top-auto max-sm:bottom-0 max-sm:max-h-[85dvh] max-sm:max-w-full',
                        'max-sm:translate-y-0 max-sm:rounded-b-none max-sm:rounded-t-2xl',
                        'max-sm:pb-[max(1rem,env(safe-area-inset-bottom))]',
                        '[&_[data-slot=dialog-close]]:size-11'
                    )}
                >
                    <DialogHeader>
                        <DialogTitle className="text-sm">{label}</DialogTitle>
                    </DialogHeader>
                    {/* select-all：长按复制整段，不用费力框选。 */}
                    <p className="min-w-0 break-all font-mono text-xs leading-6 text-foreground select-all">
                        {value}
                    </p>
                </DialogContent>
            </Dialog>
        </>
    );
}
