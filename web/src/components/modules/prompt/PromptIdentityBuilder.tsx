'use client';

import type { IdentityPresetMode } from './prompt-identity';
import { cn } from '@/lib/utils';
import { Button } from '@/components/ui/button';
import { ArrowDownToLine, Sparkles } from 'lucide-react';

/** 仅生成本地文本草稿。管理员必须选择作用层并显式保存，绝不自动修改运行策略。 */
export function PromptIdentityBuilder({
    mode,
    onModeChange,
    displayName,
    onDisplayNameChange,
    style,
    onStyleChange,
    preview,
    onApplyGlobal,
}: {
    mode: IdentityPresetMode;
    onModeChange: (mode: IdentityPresetMode) => void;
    displayName: string;
    onDisplayNameChange: (value: string) => void;
    style: string;
    onStyleChange: (value: string) => void;
    preview: string;
    /** Only fills a local draft; a separate explicit save is mandatory. */
    onApplyGlobal: () => void;
}) {
    return (
        <section className="rounded-3xl border border-border bg-card p-4 sm:p-5">
            <div className="mb-4 flex flex-wrap items-start gap-3">
                <span className="flex size-9 shrink-0 items-center justify-center rounded-xl border border-border bg-muted/40 text-foreground">
                    <Sparkles className="size-4.5" />
                </span>
                <div className="min-w-0 flex-1">
                    <h3 className="text-sm font-semibold">品牌助手设置</h3>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">
                        设置客户看到的助手名称和回答风格。可一键填入全局草稿，也能逐层应用到方案、路由或渠道。保存前绝不影响实际请求。
                    </p>
                </div>
            </div>
            <div className="grid gap-4 lg:grid-cols-2">
                <div className="grid content-start gap-3">
                    <div className="flex flex-wrap gap-1 rounded-xl border border-input bg-muted/40 p-1" role="radiogroup" aria-label="助手模板类型">
                        {([
                            { value: 'brand' as const, label: '品牌助手' },
                            { value: 'neutral' as const, label: '中性助手' },
                        ]).map((option) => (
                            <button
                                type="button"
                                role="radio"
                                aria-checked={mode === option.value}
                                key={option.value}
                                onClick={() => onModeChange(option.value)}
                                className={cn(
                                    'min-h-9 flex-1 rounded-lg px-3 text-sm transition-colors',
                                    mode === option.value ? 'bg-background font-medium text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                                )}
                            >
                                {option.label}
                            </button>
                        ))}
                    </div>
                    <label className="grid gap-1 text-xs font-medium text-muted-foreground">
                        助手显示名称
                        <input
                            value={displayName}
                            maxLength={80}
                            disabled={mode === 'neutral'}
                            onChange={(event) => onDisplayNameChange(event.target.value)}
                            placeholder="Octopus 助手"
                            aria-label="助手显示名称"
                            className="h-10 w-full rounded-xl border border-input bg-background px-3 text-sm text-foreground outline-none disabled:opacity-50 focus-visible:ring-2 focus-visible:ring-ring"
                        />
                    </label>
                    <label className="grid gap-1 text-xs font-medium text-muted-foreground">
                        回答风格
                        <input
                            value={style}
                            maxLength={160}
                            onChange={(event) => onStyleChange(event.target.value)}
                            placeholder="专业、清楚、简洁"
                            aria-label="回答风格"
                            className="h-10 w-full rounded-xl border border-input bg-background px-3 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                        />
                    </label>
                </div>
                <div className="min-w-0">
                    <p className="mb-2 text-xs font-medium text-muted-foreground">模板草稿预览</p>
                    <div aria-label="助手模板预览" className="min-h-32 whitespace-pre-wrap break-words rounded-xl border border-border bg-muted/30 px-3 py-2.5 text-sm leading-6">
                        {preview || '填写助手名称后会显示可插入的文字模板。'}
                    </div>
                    <p className="mt-2 text-xs leading-5 text-muted-foreground">
                        这里只控制普通对话的品牌称呼，不改变底层模型身份。模型检测与独立破甲实验不使用品牌规则；中性模式不会删除原有 system/developer 指令。
                    </p>
                </div>
            </div>
            <div className="mt-4 flex flex-wrap items-center justify-between gap-2 border-t border-border/70 pt-4">
                <p className="min-w-0 flex-1 text-xs leading-5 text-muted-foreground">
                    建议先在全局启用，再按需对 VIP/SVIP/渠道做差异化设置；应用后可继续编辑，必须手动保存才会生效。
                </p>
                <Button type="button" className="min-h-11 rounded-xl" disabled={!preview.trim()} onClick={onApplyGlobal} aria-label="填入全局品牌草稿">
                    <ArrowDownToLine className="mr-1.5 size-4" />
                    填入全局草稿
                </Button>
            </div>
        </section>
    );
}
