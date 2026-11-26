'use client';

import { useEffect, useState, type ReactNode } from 'react';
import { Copy, Loader2, Save, Trash2, WandSparkles } from 'lucide-react';
import type {
    PromptAudience,
    PromptRule,
    PromptRuleChannelMode,
    PromptRuleModelMode,
    PromptRulePosition,
} from '@/api/endpoints/prompt-rule';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { cn } from '@/lib/utils';
import { InfoTip } from '../dashboard/parts';
import { appendIdentityPreset } from './prompt-identity';
import {
    type PromptRuleDraft,
    type PromptRuleDraftState,
    blankPromptRuleDraft,
    coApplicableRules,
    promptRuleFromSaved,
    promptRuleModelsTextSync,
    promptRuleOneLineSummary,
    promptRuleSaveProblem,
    promptRuleValuesEqual,
} from './rule-model';

// 规则页右栏：一条规则的编辑视图（规则名、文案、方案/渠道/模型作用域、对谁、放哪里、保存）。
// 所有修改都先落在草稿里，点「保存」才发 create/update；删除需要二次确认，复制一条直接在服务端建副本。

function SegControl<T extends string>({
    value,
    options,
    onChange,
    label,
    disabled,
}: {
    value: T;
    options: ReadonlyArray<{ value: T; label: string }>;
    onChange: (value: T) => void;
    label: string;
    disabled?: boolean;
}) {
    return (
        <div
            role="radiogroup"
            aria-label={label}
            className={cn('inline-flex min-h-11 max-w-full items-center overflow-x-auto rounded-xl border border-input bg-muted/40 p-0.5 text-xs', disabled && 'opacity-50')}
        >
            {options.map((option) => (
                <button
                    key={option.value}
                    type="button"
                    role="radio"
                    aria-checked={value === option.value}
                    disabled={disabled}
                    onClick={() => onChange(option.value)}
                    title={option.label}
                    className={cn(
                        'min-h-11 shrink-0 rounded-lg px-2.5 transition-colors',
                        value === option.value ? 'bg-background font-medium text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground',
                        disabled && 'cursor-not-allowed',
                    )}
                >
                    {option.label}
                </button>
            ))}
        </div>
    );
}

function ToggleChip({
    active,
    onClick,
    title,
    children,
}: {
    active: boolean;
    onClick: () => void;
    title?: string;
    children: ReactNode;
}) {
    return (
        <button
            type="button"
            role="checkbox"
            aria-checked={active}
            title={title}
            onClick={onClick}
            className={cn(
                'min-h-11 shrink-0 rounded-full border px-3 text-[13px] transition-colors',
                active
                    ? 'border-primary/50 bg-primary/10 font-semibold text-primary before:content-["✓_"]'
                    : 'border-border bg-card text-muted-foreground hover:text-foreground',
            )}
        >
            {children}
        </button>
    );
}

const SEGMENT_POSITION_OPTIONS: ReadonlyArray<{ value: PromptRulePosition; label: string }> = [
    { value: 'before', label: '放在前面' },
    { value: 'after', label: '放在后面' },
];

const CHANNEL_MODE_OPTIONS: ReadonlyArray<{ value: PromptRuleChannelMode; label: string }> = [
    { value: 'all', label: '所有渠道' },
    { value: 'include', label: '只在这些' },
    { value: 'exclude', label: '除了这些' },
];

const MODEL_MODE_OPTIONS: ReadonlyArray<{ value: PromptRuleModelMode; label: string }> = [
    { value: 'all', label: '所有模型' },
    { value: 'selected', label: '指定' },
];

const AUDIENCE_OPTIONS: ReadonlyArray<{ value: PromptAudience; label: string }> = [
    { value: 'all', label: '所有用户' },
    { value: 'admin_only', label: '仅管理员 Key' },
];

const SEG_ROW_GRID = 'grid grid-cols-[minmax(5.5rem,auto)_1fr] items-start gap-x-3 border-t border-border/70 py-3';

function ScopeRow({ label, tip, children }: { label: string; tip?: string; children: ReactNode }) {
    return (
        <div className={SEG_ROW_GRID}>
            <div className="flex min-h-6 flex-wrap items-center gap-1">
                <span className="text-[13px] font-semibold">{label}</span>
                <InfoTip label={`${label}说明`}>{tip}</InfoTip>
            </div>
            <div className="min-w-0">{children}</div>
        </div>
    );
}

/** 模型 pattern：一行一个（逗号也认），去空、去重、保序。 */
function parseModelPatterns(input: string): string[] {
    return input.split(/[\n,，]/).map((p) => p.trim()).filter((p, i, list) => p && list.indexOf(p) === i);
}

export type PlanOption = { id: number; name: string };
export type ChannelOption = { id: number; name: string };

export type RuleEditorProps = {
    /** 被编辑的已保存规则；null = 新建草稿（值来自 draft）。 */
    saved: PromptRule | null;
    /** 选中条目的草稿；null = 还没改过（直接用 saved）。 */
    draft: PromptRuleDraftState | null;
    saving: boolean;
    copying: boolean;
    deleting: boolean;
    plans: PlanOption[];
    channels: ChannelOption[];
    /** 方案 id → 显示名。 */
    planNames: Map<number, string>;
    /** 渠道 id → 显示名。 */
    channelNames: Map<number, string>;
    savedRules: PromptRule[];
    /** 品牌助手设置生成的模板文字；空串时禁用「插入模板」。 */
    identityPreset: string;
    onChange: (mutate: (current: PromptRuleDraft) => PromptRuleDraft) => void;
    onSave: () => void;
    onDelete: () => void;
    onCopy: () => void;
    onJumpToPreview: () => void;
};

export function RuleEditor({
    saved,
    draft,
    saving,
    copying,
    deleting,
    plans,
    channels,
    planNames,
    channelNames,
    savedRules,
    identityPreset,
    onChange,
    onSave,
    onDelete,
    onCopy,
    onJumpToPreview,
}: RuleEditorProps) {
    const [confirmDelete, setConfirmDelete] = useState(false);
    // 删除的二次确认 6 秒不点就自动撤销。
    useEffect(() => {
        if (!confirmDelete) return;
        const timer = setTimeout(() => setConfirmDelete(false), 6000);
        return () => clearTimeout(timer);
    }, [confirmDelete]);

    const value = draft?.value ?? (saved ? promptRuleFromSaved(saved) : blankPromptRuleDraft());

    // 模型输入框显示用户敲的原文（尾逗号/尾换行不能被吃掉），解析后的值才进草稿。
    const patternSignature = value.models.join('\n');
    const [patternsText, setPatternsText] = useState(patternSignature);
    const [seenPatternSignature, setSeenPatternSignature] = useState(patternSignature);
    if (patternSignature !== seenPatternSignature) {
        setSeenPatternSignature(patternSignature);
        // 外部值真变了才覆写；只是尾分隔符不同就保留正在敲的原文。
        const next = promptRuleModelsTextSync(patternsText, patternSignature);
        if (next !== null) setPatternsText(next);
    }

    if (!saved && !draft) {
        return (
            <section aria-label="规则编辑器" className="rounded-3xl border border-dashed border-border bg-card/50 px-4 py-10 text-center text-sm text-muted-foreground">
                选一条规则，或点「新建」写第一条。
            </section>
        );
    }

    const dirty = draft ? !promptRuleValuesEqual(draft.value, draft.baseline) : false;
    const saveProblem = promptRuleSaveProblem(value);
    const coRules = coApplicableRules(value, savedRules);

    return (
        <section aria-label="规则编辑器" className="min-w-0 rounded-3xl border border-border bg-card p-4 sm:p-5">
            <div className="flex flex-wrap items-center gap-2">
                <input
                    value={value.name}
                    maxLength={191}
                    disabled={saving}
                    onChange={(event) => onChange((current) => ({ ...current, name: event.target.value }))}
                    placeholder="规则名字，比如：品牌身份"
                    aria-label="规则名字"
                    className="min-h-11 min-w-0 flex-1 rounded-xl border border-input bg-background px-3 text-sm text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                />
                <Badge variant={value.enabled ? 'secondary' : 'outline'} className="shrink-0">
                    {value.enabled ? '已启用' : '未启用'}
                </Badge>
                <Button
                    type="button"
                    size="sm"
                    variant={dirty && !saveProblem ? 'default' : 'outline'}
                    className="min-h-11 shrink-0 rounded-xl px-3"
                    disabled={saving || !dirty || Boolean(saveProblem)}
                    onClick={onSave}
                    title={dirty ? (saveProblem ?? (saved ? '保存这条的改动' : '保存新规则')) : '没有改动'}
                >
                    {saving ? <Loader2 className="size-4 animate-spin" /> : <Save className="size-4" />}
                    保存
                </Button>
            </div>
            {dirty && saveProblem ? <span role="alert" className="mt-1.5 inline-block text-xs font-medium text-destructive">{saveProblem}</span> : null}
            {dirty && !saveProblem ? <span className="mt-1.5 inline-block text-xs font-medium text-amber-600">有未保存草稿</span> : null}

            <div className="mt-3">
                <label className="mb-1.5 block text-xs font-medium text-muted-foreground" htmlFor="prompt-rule-text">内容</label>
                <textarea
                    id="prompt-rule-text"
                    value={value.text}
                    onChange={(event) => onChange((current) => ({ ...current, text: event.target.value }))}
                    placeholder="写一段系统提示词；请求命中时按「放哪里」加进去。"
                    aria-label={saved ? `${saved.name} 内容` : '新规则内容'}
                    className="min-h-40 w-full rounded-xl border border-border bg-background px-3 py-2.5 text-sm leading-6 text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring"
                />
                <div className="mt-1.5 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                    <Button
                        type="button"
                        size="sm"
                        variant="outline"
                        className="min-h-11 shrink-0 rounded-xl px-2.5"
                        disabled={saving || !identityPreset}
                        onClick={() => onChange((current) => ({ ...current, text: appendIdentityPreset(current.text, identityPreset) }))}
                        title="只把模板填进这条草稿，不自动保存、启用或删除原内容"
                    >
                        <WandSparkles className="size-4" />
                        插入模板
                    </Button>
                    <span>{value.text.length} 字</span>
                    {dirty ? <span className="font-medium text-amber-600">有未保存草稿</span> : null}
                    <span className="ml-auto flex items-center gap-2">
                        <Button
                            type="button"
                            size="sm"
                            variant="outline"
                            className="min-h-11 shrink-0 rounded-xl px-2.5 text-destructive"
                            disabled={deleting || !saved}
                            onClick={confirmDelete ? () => { setConfirmDelete(false); onDelete(); } : () => setConfirmDelete(true)}
                            aria-label={confirmDelete ? '确认删除这条规则' : '删除这条规则'}
                            title={!saved ? '这条还没保存过，不用删' : confirmDelete ? '再点一次就删掉' : '删除这条规则需要两次确认'}
                        >
                            {deleting ? <Loader2 className="size-4 animate-spin" /> : <Trash2 className="size-4" />}
                            {confirmDelete ? '确认删除' : '删除'}
                        </Button>
                        <Button
                            type="button"
                            size="sm"
                            variant="outline"
                            className="min-h-11 shrink-0 rounded-xl px-2.5"
                            disabled={saving || copying || !saved}
                            onClick={onCopy}
                            title={!saved ? '先保存这条，才能复制' : '按保存过的内容复制一条新规则'}
                        >
                            {copying ? <Loader2 className="size-4 animate-spin" /> : <Copy className="size-4" />}
                            <span className="hidden sm:inline">复制一条</span>
                        </Button>
                    </span>
                </div>
            </div>

            <div className="mt-3 grid gap-3 border-t border-border/70 pt-3">
                <ScopeRow label="管哪些方案" tip="用户调用时用的方案代号，比如 vip、svip；「没选方案的 Key」管住没绑定方案的 API key。">
                    <div className="flex flex-wrap gap-1.5" role="group" aria-label="管哪些方案">
                        <ToggleChip
                            active={value.plan_mode === 'all'}
                            onClick={() => onChange((current) => ({ ...current, plan_mode: 'all', plan_ids: [], include_no_plan: false }))}
                            title="所有 key（含没选方案的）都管"
                        >
                            所有方案
                        </ToggleChip>
                        {plans.map((plan) => (
                            <ToggleChip
                                key={plan.id}
                                active={value.plan_mode === 'selected' && value.plan_ids.includes(plan.id)}
                                onClick={() => onChange((current) => ({
                                    ...current,
                                    plan_mode: 'selected',
                                    plan_ids: current.plan_mode === 'selected' && current.plan_ids.includes(plan.id)
                                        ? current.plan_ids.filter((id) => id !== plan.id)
                                        : [...current.plan_ids, plan.id],
                                }))}
                                title={plan.name}
                            >
                                {plan.name}
                            </ToggleChip>
                        ))}
                        <ToggleChip
                            active={value.plan_mode === 'selected' && value.include_no_plan}
                            onClick={() => onChange((current) => ({ ...current, plan_mode: 'selected', include_no_plan: !(current.plan_mode === 'selected' && current.include_no_plan) }))}
                            title="没绑定任何方案的 API key 也管上"
                        >
                            没选方案的 Key
                        </ToggleChip>
                    </div>
                </ScopeRow>

                <ScopeRow label="走哪些渠道" tip="按最后真正转发到的渠道算。">
                    <SegControl
                        label="走哪些渠道"
                        disabled={saving}
                        value={value.channel_mode}
                        options={CHANNEL_MODE_OPTIONS}
                        onChange={(mode) => onChange((current) => ({ ...current, channel_mode: mode as PromptRuleChannelMode }))}
                    />
                    {value.channel_mode !== 'all' ? (
                        <div className="mt-2 flex flex-wrap gap-1.5" role="group" aria-label="勾选渠道">
                            {channels.map((channel) => (
                                <ToggleChip
                                    key={channel.id}
                                    active={value.channel_ids.includes(channel.id)}
                                    onClick={() => onChange((current) => ({
                                        ...current,
                                        channel_ids: current.channel_ids.includes(channel.id)
                                            ? current.channel_ids.filter((id) => id !== channel.id)
                                            : [...current.channel_ids, channel.id],
                                    }))}
                                    title={`#${channel.id} ${channel.name}`}
                                >
                                    {channel.name}
                                </ToggleChip>
                            ))}
                        </div>
                    ) : null}
                </ScopeRow>

                <ScopeRow label="哪些模型" tip="精确名或 claude-* 尾星通配，一行一个。">
                    <SegControl
                        label="哪些模型"
                        disabled={saving}
                        value={value.model_mode}
                        options={MODEL_MODE_OPTIONS}
                        onChange={(mode) => onChange((current) => ({ ...current, model_mode: mode as PromptRuleModelMode }))}
                    />
                    {value.model_mode === 'selected' ? (
                        <>
                        <textarea
                            value={patternsText}
                            onChange={(event) => {
                                const raw = event.target.value;
                                // 显示原文，草稿里存解析后的值：占位、保存、脏判定都用后者。
                                setPatternsText(raw);
                                onChange((current) => ({ ...current, models: parseModelPatterns(raw) }));
                            }}
                            placeholder={'一行一个，比如：claude-* / gemini-2.5-pro'}
                            aria-label="模型 pattern（一行一个，支持尾星通配）"
                            className="mt-2 min-h-20 w-full rounded-xl border border-border bg-background px-3 py-2
                                 font-mono text-sm leading-6 text-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring whitespace-pre"
                        />
                        </>
                    ) : null}
                </ScopeRow>

                <ScopeRow label="对谁" tip="先选「仅管理员」自己试，没问题再放开。">
                    <SegControl
                        label="对谁"
                        disabled={saving}
                        value={value.audience}
                        options={AUDIENCE_OPTIONS}
                        onChange={(audience) => onChange((current) => ({ ...current, audience: audience as PromptAudience }))}
                    />
                </ScopeRow>

                <ScopeRow label="放哪里" tip="请求自带的系统提示词保留。">
                    <SegControl
                        label="放哪里"
                        disabled={saving}
                        value={value.position}
                        options={SEGMENT_POSITION_OPTIONS}
                        onChange={(position) => onChange((current) => ({ ...current, position: position as PromptRulePosition }))}
                    />
                </ScopeRow>
            </div>

            <div className="mt-3 rounded-2xl bg-primary/5 px-3 py-2.5 text-sm leading-6">
                <b>一句话：</b>
                {promptRuleOneLineSummary(value, planNames, channelNames)}
                <button
                    type="button"
                    className="ml-2 inline-flex min-h-11 items-center font-semibold text-primary underline-offset-2 hover:underline"
                    onClick={onJumpToPreview}
                >
                    去合并预览看效果 ›
                </button>
            </div>

            {coRules.length ? (
                <div className="mt-2 text-xs leading-5 text-muted-foreground">
                    和另外 {coRules.length} 条规则会同时生效：
                    <ul className="mt-0.5 leading-5">
                        {coRules.map((co) => (
                            <li key={co.id} className="min-w-0">
                                <span className="font-medium text-foreground">「{co.name}」</span>
                                <span className="break-words">{promptRuleOneLineSummary(promptRuleFromSaved(co), planNames, channelNames)}</span>
                            </li>
                        ))}
                    </ul>
                </div>
            ) : (
                <div className="mt-2 text-xs leading-5 text-muted-foreground">和其它规则不会同时命中同一个请求。</div>
            )}
        </section>
    );
}
