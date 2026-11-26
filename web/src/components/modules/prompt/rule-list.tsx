'use client';

import { useMemo, useState } from 'react';
import { ArrowDown, ArrowUp, Loader2, Plus, Search, X } from 'lucide-react';
import type { PromptRule } from '@/api/endpoints/prompt-rule';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Switch } from '@/components/ui/switch';
import { cn } from '@/lib/utils';
import type { ChannelOption } from './rule-editor';
import {
    matchesRuleSearch,
    promptRuleCanMove,
    promptRuleChannelAllows,
    promptRuleOrdered,
    promptRulePlanAllows,
    promptRuleScopeTags,
} from './rule-model';
// 规则页左栏：搜索 + 新建 + 筛选 chip + 规则卡片列表。编辑器在右栏（rule-editor.tsx）。

const TAG_STYLES: Record<string, string> = {
    plan: 'bg-primary/10 text-primary',
    channel: 'bg-sky-500/10 text-sky-700 dark:text-sky-300',
    model: 'bg-muted text-muted-foreground',
    audience: 'bg-amber-500/10 text-amber-700 dark:text-amber-300',
};

/** 规则卡片：名字、启用开关、放前面/放后面、文本一行预览、范围小标签、同类里上移下移。 */
function RuleCard({
    rule,
    selected,
    hasDraft,
    canMoveUp,
    canMoveDown,
    moveDisabled,
    onSelect,
    onToggle,
    onMove,
    togglePending,
    planNames,
    channelNames,
}: {
    rule: PromptRule;
    selected: boolean;
    hasDraft: boolean;
    canMoveUp: boolean;
    canMoveDown: boolean;
    moveDisabled: boolean;
    onSelect: () => void;
    onToggle: (enabled: boolean) => void;
    onMove: (direction: 'up' | 'down') => void;
    togglePending: boolean;
    planNames: Map<number, string>;
    channelNames: Map<number, string>;
}) {
    const tags = promptRuleScopeTags(rule, planNames, channelNames);
    const preview = rule.text.trim();
    return (
        <article
            className={cn(
                'rounded-2xl border transition-colors',
                selected ? 'border-primary/60 bg-primary/5' : 'border-border bg-background/60 hover:border-border',
            )}
        >
            <div className="flex items-start gap-2 p-3">
                <button
                    type="button"
                    onClick={onSelect}
                    aria-pressed={selected}
                    aria-label={`选中规则 ${rule.name}`}
                    className="min-h-11 min-w-0 flex-1 py-0.5 text-left"
                >
                    <span className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                        <span className="min-w-0 truncate text-sm font-semibold">{rule.name}</span>
                        <span className="shrink-0 text-xs text-muted-foreground">{rule.position === 'after' ? '放在后面' : '放在前面'}</span>
                        {hasDraft ? <Badge variant="outline" className="shrink-0 border-amber-500/50 text-amber-600">未保存</Badge> : null}
                    </span>
                    {preview ? (
                        <span className={cn('mt-0.5 block truncate text-xs', rule.enabled ? 'text-muted-foreground' : 'text-muted-foreground/60 line-through decoration-muted-foreground/40')}>
                            {preview}
                        </span>
                    ) : (
                        <span className="mt-0.5 block truncate text-xs text-muted-foreground/50">（还没写内容）</span>
                    )}
                    <span className="mt-1.5 flex flex-wrap gap-1">
                        {tags.map((tag) => (
                            <span key={`${tag.kind}-${tag.label}`} title={tag.title} className={cn('rounded-lg px-1.5 py-0.5 text-[11px] font-semibold', TAG_STYLES[tag.kind])}>
                                {tag.label}
                            </span>
                        ))}
                    </span>
                </button>
                <div className="flex shrink-0 flex-col items-center gap-1">
                    <label className="flex cursor-pointer items-center" title={rule.enabled ? '已启用，点开关关闭后请求原样转发' : '已关闭，打开后按位置加进系统提示词'}>
                        <Switch
                            checked={rule.enabled}
                            disabled={togglePending}
                            onCheckedChange={onToggle}
                            aria-label={`${rule.enabled ? '关闭' : '启用'}规则 ${rule.name}`}
                        />
                    </label>
                    <div className="flex items-center gap-0.5">
                        <button
                            type="button"
                            onClick={() => onMove('up')}
                            disabled={!canMoveUp || moveDisabled}
                            aria-label={`把规则 ${rule.name} 往前挪`}
                            title={!canMoveUp ? '上面没有同类规则了' : moveDisabled ? '清空搜索和筛选后才能调顺序' : '和上面同类规则换位置'}
                            className="inline-flex min-h-11 min-w-11 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:text-foreground disabled:cursor-not-allowed disabled:opacity-30"
                        >
                            <ArrowUp className="size-4" />
                        </button>
                        <button
                            type="button"
                            onClick={() => onMove('down')}
                            disabled={!canMoveDown || moveDisabled}
                            aria-label={`把规则 ${rule.name} 往后挪`}
                            title={!canMoveDown ? '下面没有同类规则了' : moveDisabled ? '清空搜索和筛选后才能调顺序' : '和下面同类规则换位置'}
                            className="inline-flex min-h-11 min-w-11 items-center justify-center rounded-lg text-muted-foreground transition-colors hover:text-foreground disabled:cursor-not-allowed disabled:opacity-30"
                        >
                            <ArrowDown className="size-4" />
                        </button>
                    </div>
                </div>
            </div>
        </article>
    );
}

export function RuleList({
    rules,
    plans,
    channels,
    loading,
    selectedKey,
    togglePendingId,
    dirtyKeys,
    onSelect,
    onNew,
    onToggle,
    onMove,
    reorderPending,
    planNames,
    channelNames,
}: {
    rules: PromptRule[];
    plans: Array<{ id: number; name: string }>;
    channels: ChannelOption[];
    loading: boolean;
    /** 当前选中的规则 key：规则 id 字符串或 NEW_RULE_KEY；null = 没选中。 */
    selectedKey: string | null;
    togglePendingId: number | null;
    dirtyKeys: Set<string>;
    onSelect: (key: string) => void;
    onNew: () => void;
    onToggle: (rule: PromptRule, enabled: boolean) => void;
    onMove: (rule: PromptRule, direction: 'up' | 'down') => void;
    reorderPending: boolean;
    planNames: Map<number, string>;
    channelNames: Map<number, string>;
}) {
    const [query, setQuery] = useState('');
    const [planFilter, setPlanFilter] = useState<'all' | number>('all');
    const [channelFilter, setChannelFilter] = useState<'all' | number>('all');
    const [onlyEnabled, setOnlyEnabled] = useState(false);

    const filtered = useMemo(
        () => rules.filter((rule) => (planFilter === 'all' || promptRulePlanAllows(rule, planFilter))
            && (channelFilter === 'all' || promptRuleChannelAllows(rule, channelFilter))
            && (!onlyEnabled || rule.enabled)
            && matchesRuleSearch(query, rule)),
        [rules, planFilter, channelFilter, onlyEnabled, query],
    );
    // 列表顺序就是真实叠加顺序（全局 → 方案 → 模型 → 渠道；同级按 sort、id），不另排一套。
    const visible = useMemo(() => promptRuleOrdered(filtered), [filtered]);
    // 有搜索/筛选时列表不是全量，上移下移会跳过看不见的规则，所以先禁用并说明原因。
    const moveDisabled = query.trim() !== '' || planFilter !== 'all' || channelFilter !== 'all' || onlyEnabled || reorderPending;

    const chip = (active: boolean) => cn(
        'min-h-11 shrink-0 rounded-full border px-3 text-[13px] transition-colors',
        active ? 'border-primary/50 bg-primary/10 font-semibold text-primary' : 'border-border bg-card text-muted-foreground hover:text-foreground',
    );

    return (
        <section aria-label="规则列表" className="min-w-0">
            <div className="flex items-center gap-2">
                <label className="relative min-w-0 flex-1">
                    <Search className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                    <input
                        value={query}
                        onChange={(event) => setQuery(event.target.value)}
                        placeholder="搜索名字、内容"
                        aria-label="搜索规则"
                        className="min-h-11 w-full rounded-xl border border-input bg-background pl-9 pr-9 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    />
                    {query ? (
                        <button
                            type="button"
                            aria-label="清空搜索"
                            onClick={() => setQuery('')}
                            className="absolute right-1 top-1/2 inline-flex size-11 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground hover:text-foreground"
                        >
                            <X className="size-4" />
                        </button>
                    ) : null}
                </label>
                <Button type="button" size="sm" className="min-h-11 shrink-0 rounded-xl px-3" onClick={onNew}>
                    <Plus className="size-4" />
                    新建
                </Button>
            </div>

            <div className="mt-2 flex flex-wrap gap-1.5" role="group" aria-label="筛选规则">
                <button type="button" className={chip(planFilter === 'all' && channelFilter === 'all' && !onlyEnabled)} onClick={() => { setPlanFilter('all'); setChannelFilter('all'); setOnlyEnabled(false); }}>
                    全部 {rules.length}
                </button>
                {plans.map((plan) => (
                    <button key={plan.id} type="button" className={chip(planFilter === plan.id)} onClick={() => setPlanFilter((current) => (current === plan.id ? 'all' : plan.id))}>
                        {plan.name}
                    </button>
                ))}
                <label className={cn(chip(false), 'cursor-pointer gap-1 inline-flex items-center')} title="按最后真正转发到的渠道筛选">
                    <span className="sr-only">按渠道筛选</span>
                    <select
                        value={channelFilter === 'all' ? '' : String(channelFilter)}
                        onChange={(event) => setChannelFilter(event.target.value === '' ? 'all' : Number(event.target.value))}
                        aria-label="按渠道筛选"
                        className="min-h-11 max-w-32 cursor-pointer bg-transparent text-[13px] outline-none"
                    >
                        <option value="">按渠道…</option>
                        {channels.map((channel) => (
                            <option key={channel.id} value={channel.id}>#{channel.id} {channel.name}</option>
                        ))}
                    </select>
                </label>
                <button type="button" className={chip(onlyEnabled)} aria-pressed={onlyEnabled} onClick={() => setOnlyEnabled((v) => !v)}>
                    只看开着的
                </button>
            </div>

            <div className="mt-3 grid content-start gap-2">
                {loading ? (
                    <div className="flex justify-center py-10 text-muted-foreground">
                        <Loader2 className="size-5 animate-spin" />
                    </div>
                ) : visible.length === 0 ? (
                    <div className="rounded-2xl border border-dashed border-border px-4 py-8 text-center text-sm text-muted-foreground">
                        {rules.length === 0 ? '还没有规则，点「新建」写第一条' : '没有符合条件的规则'}
                    </div>
                ) : (
                    visible.map((rule) => (
                        <RuleCard
                            key={rule.id}
                            rule={rule}
                            selected={selectedKey === String(rule.id)}
                            hasDraft={dirtyKeys.has(String(rule.id))}
                            canMoveUp={promptRuleCanMove(rules, rule.id, 'up')}
                            canMoveDown={promptRuleCanMove(rules, rule.id, 'down')}
                            moveDisabled={moveDisabled}
                            onSelect={() => onSelect(String(rule.id))}
                            onToggle={(enabled) => onToggle(rule, enabled)}
                            onMove={(direction) => onMove(rule, direction)}
                            togglePending={togglePendingId === rule.id}
                            planNames={planNames}
                            channelNames={channelNames}
                        />
                    ))
                )}
            </div>
            <p className="mt-2 px-1 text-xs leading-5 text-muted-foreground">
                列表从上到下就是加进系统提示词的先后：越靠下的规则越靠外。同一类规则之间用 ↑ ↓ 调先后；
                范围选「所有方案 + 所有渠道 + 所有模型」就是那条全局规则。
            </p>
        </section>
    );
}
