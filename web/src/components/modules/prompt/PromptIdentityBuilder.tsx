'use client';

import type { IdentityPresetMode } from './prompt-identity';
import { cn } from '@/lib/utils';
import { Sparkles } from 'lucide-react';

/** 仅生成本地文本草稿。管理员必须选择作用层并显式保存，绝不自动修改运行策略。 */
export function PromptIdentityBuilder({
    mode,
    onModeChange,
    displayName,
    onDisplayNameChange,
    style,
    onStyleChange,
    preview,
}: {
    mode: IdentityPresetMode;
    onModeChange: (mode: IdentityPresetMode) => void;
    displayName: string;
    onDisplayNameChange: (value: string) => void;
    style: string;
    onStyleChange: (value: string) => void;
    preview: string;
}) {
    return (
        <section className="rounded-3xl border border-border bg-card p-4 sm:p-5">
            <div className="mb-4 flex flex-wrap items-start gap-3">
                <span className="flex size-9 shrink-0 items-center justify-center rounded-xl border border-border bg-muted/40 text-foreground">
                    <Sparkles className="size-4.5" />
                </span>
                <div className="min-w-0 flex-1">
                    <h3 className="text-sm font-semibold">助手身份与中性模板</h3>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">
                        先生成可编辑文本，再到下方选择全局、方案、路由或渠道插入并手动保存。模板不会自动生效，也不是修改底层模型身份。
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
                        这里只控制普通对话中的文字指令；模型检测探针不使用本模板。中性模式不会删除客户端原有 system/developer 提示词。
                    </p>
                </div>
            </div>
        </section>
    );
}
