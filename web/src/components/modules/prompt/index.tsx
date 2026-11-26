'use client';

import { type Dispatch, type SetStateAction, useEffect, useMemo, useRef, useState } from 'react';
import { GitBranch, KeyRound, Loader2, MessageSquareText, Radio } from 'lucide-react';
import type { LucideIcon } from 'lucide-react';
import type { ApiError } from '@/api/types';
import { useAccessPlanList } from '@/api/endpoints/access-plan';
import { useChannelList } from '@/api/endpoints/channel';
import {
    useCreatePromptRule,
    useDeletePromptRule,
    usePromptRuleList,
    useReorderPromptRules,
    useTogglePromptRule,
    useUpdatePromptRule,
    type PromptRule,
} from '@/api/endpoints/prompt-rule';
import { useAuthStore } from '@/api/endpoints/user';
import { PageWrapper } from '@/components/common/PageWrapper';
import { toast } from '@/components/common/Toast';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { useNavStore } from '../navbar/nav-store';
import { cn } from '@/lib/utils';
import { sortChannelsStable } from './prompt-rows';
import { PromptIdentityBuilder } from './PromptIdentityBuilder';
import { RedTeamLab } from './RedTeamLab';
import { buildIdentityPreset, upsertIdentityPreset, type IdentityPresetMode } from './prompt-identity';
import { RuleList } from './rule-list';
import { RuleEditor, type ChannelOption, type PlanOption } from './rule-editor';
import { MergePreview } from './merge-preview';
import {
    NEW_RULE_KEY,
    type PromptRuleDraftMap,
    acceptRuleDraftSave,
    blankPromptRuleDraft,
    dirtyRuleDraftKeys,
    dropRuleDraft,
    editRuleDraft,
    promptRuleCopyName,
    promptRuleDraftPayload,
    promptRuleFromSaved,
    promptRuleReorderIDs,
    promptRuleValuesEqual,
} from './rule-model';

// 提示词页：管理员按「一条规则 = 一段文字 + 管哪些方案/渠道/模型 + 放在前面还是后面」
// 管理；普通用户只看到由管理员统一管理的说明。命中与合并效果都以后端结果为准。

function apiErrorMessage(error: unknown): string | undefined {
    return (error as ApiError | undefined)?.message;
}

function LayerCard({
    icon: Icon,
    title,
    description,
    enabled,
}: {
    icon: LucideIcon;
    title: string;
    description: string;
    enabled: boolean;
}) {
    return (
        <div className="rounded-3xl border border-border bg-card p-4">
            <div className="flex items-start gap-3">
                <div className={cn(
                    'flex size-10 items-center justify-center rounded-2xl border',
                    enabled ? 'border-primary/30 bg-primary/10 text-primary' : 'border-border bg-muted/30 text-muted-foreground'
                )}>
                    <Icon className="size-5" />
                </div>
                <div className="min-w-0 flex-1">
                    <div className="flex items-center gap-2">
                        <h3 className="text-sm font-semibold">{title}</h3>
                        <Badge variant={enabled ? 'default' : 'outline'}>{enabled ? '已启用' : '未启用'}</Badge>
                    </div>
                    <p className="mt-1 text-xs leading-5 text-muted-foreground">{description}</p>
                </div>
            </div>
        </div>
    );
}

function UserPromptView() {
    const setActiveItem = useNavStore((state) => state.setActiveItem);

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-5 rounded-t-3xl pb-24 md:pb-4">
            <section className="rounded-3xl border border-border bg-card p-6">
                <div className="flex flex-wrap items-start justify-between gap-4">
                    <div className="max-w-2xl">
                        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-border bg-muted/30 px-3 py-1 text-xs text-muted-foreground">
                            <MessageSquareText className="size-3.5" />
                            个人侧提示词
                        </div>
                        <h2 className="text-2xl font-bold">提示词由管理员统一管理</h2>
                        <p className="mt-2 text-sm leading-6 text-muted-foreground">
                            你的请求仍然可以自带 system / developer 提示词；管理员用规则决定哪些请求要在前面或后面加提示词，你自带的不会被删。为避免泄露管理策略，普通用户不展示完整覆盖内容。
                        </p>
                    </div>
                    <Button type="button" className="rounded-xl" onClick={() => setActiveItem('key')}>
                        <KeyRound className="size-4" />
                        查看 API Key
                    </Button>
                </div>
            </section>

            <div className="grid gap-4 md:grid-cols-3">
                <LayerCard
                    icon={KeyRound}
                    title="管理员策略"
                    description="管理员按方案、渠道、模型配置规则，命中请求会按位置加系统提示词，不会删掉你自带的。"
                    enabled
                />
                <LayerCard
                    icon={GitBranch}
                    title="模型路由"
                    description="如果你的 API Key 绑定了不同方案，同一个请求模型可能应用不同的提示词策略。"
                    enabled
                />
                <LayerCard
                    icon={Radio}
                    title="渠道约束"
                    description="最终转发到不同上游渠道时，渠道级规则排在最外面。"
                    enabled
                />
            </div>
        </PageWrapper>
    );
}

const SECTION = 'rounded-3xl border border-border bg-card p-4 sm:p-5';

type WorkspaceTab = 'rules' | 'preview' | 'redteam';

/** 管理员工作区：规则 / 合并预览 / 破甲实验 三个 tab。三个面板都保持挂载，切 tab 不丢草稿。 */
function AdminPromptWorkspace() {
    const [tab, setTab] = useState<WorkspaceTab>('rules');
    const [drafts, setDrafts] = useState<PromptRuleDraftMap>({});
    const [selectedKey, setSelectedKey] = useState<string | null>(null);
    const { data: plans = [], isLoading: plansLoading } = useAccessPlanList();
    const { data: channelItems = [], isLoading: channelsLoading } = useChannelList();
    const { data: rules = [], isLoading: rulesLoading } = usePromptRuleList();
    const channels = useMemo(() => sortChannelsStable(channelItems.map((item) => item.raw)), [channelItems]);
    const dirtyCount = dirtyRuleDraftKeys(drafts).length;
    // 规则的任何保存/开关/调顺序都会改变这个串，合并预览靠它重算。
    const rulesVersion = useMemo(
        () => rules.map((rule) => `${rule.id}:${rule.sort}:${rule.enabled}:${rule.text.length}:${rule.updated_at}`).join('|'),
        [rules],
    );
    // 离开页面/关页面前要用的最新脏草稿数：事件回调和卸载清理只认这个 ref。
    const dirtyCountRef = useRef(dirtyCount);
    useEffect(() => {
        dirtyCountRef.current = dirtyCount;
    });

    useEffect(() => {
        const warnBeforeUnload = (event: BeforeUnloadEvent) => {
            if (dirtyCountRef.current <= 0) return;
            event.preventDefault();
            // 兼容旧浏览器：有返回值才会弹确认框。
            event.returnValue = '';
        };
        window.addEventListener('beforeunload', warnBeforeUnload);
        return () => {
            window.removeEventListener('beforeunload', warnBeforeUnload);
            // 切到别的页 / 退出登录会直接卸载本页，草稿没地方存：至少提醒一句。
            if (dirtyCountRef.current > 0) {
                toast.warning(`有 ${dirtyCountRef.current} 条规则的改动没保存，已经随离开丢掉了`);
            }
        };
    }, []);

    const switchTab = (next: WorkspaceTab) => {
        if (next === tab) return;
        if (tab === 'rules' && dirtyCount > 0) {
            toast.warning(`规则页有 ${dirtyCount} 条没保存，改动留在草稿里，回「规则」页保存才生效。`);
        }
        setTab(next);
    };

    const tabs: Array<{ value: WorkspaceTab; label: string }> = [
        { value: 'rules', label: '规则' },
        { value: 'preview', label: '合并预览' },
        { value: 'redteam', label: '破甲实验' },
    ];

    const dataLoading = plansLoading || channelsLoading || rulesLoading;

    return (
        <div className="flex h-full min-h-0 flex-col">
            <div role="tablist" aria-label="提示词管理工作区" className="mx-3 mb-2 mt-2 flex shrink-0 flex-wrap items-center gap-2 rounded-2xl border border-border bg-card p-1.5 sm:mx-4">
                {tabs.map((item) => (
                    <button
                        key={item.value}
                        type="button"
                        role="tab"
                        aria-selected={tab === item.value}
                        onClick={() => switchTab(item.value)}
                        className={cn('min-h-11 flex-1 rounded-xl px-4 text-sm font-medium sm:flex-none', tab === item.value ? 'bg-primary text-primary-foreground' : 'text-muted-foreground hover:bg-muted/50')}
                    >
                        {item.label}
                    </button>
                ))}
            </div>
            {/* 三个面板常驻挂载：切 tab、切条目都不丢未保存草稿。 */}
            <div className="min-h-0 flex-1" hidden={tab !== 'rules'}>
                <AdminRulesView
                    rules={rules}
                    rulesLoading={rulesLoading}
                    dataLoading={dataLoading}
                    drafts={drafts}
                    setDrafts={setDrafts}
                    selectedKey={selectedKey}
                    setSelectedKey={setSelectedKey}
                    onJumpToPreview={() => switchTab('preview')}
                />
            </div>
            <div className="min-h-0 flex-1" hidden={tab !== 'preview'}>
                <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-4 rounded-t-3xl pb-[calc(7rem+env(safe-area-inset-bottom))] md:pb-4">
                    <MergePreview plans={plans} channels={channels} loading={dataLoading} rulesVersion={rulesVersion} />
                </PageWrapper>
            </div>
            <div className="min-h-0 flex-1" hidden={tab !== 'redteam'}>
                <RedTeamLab />
            </div>
        </div>
    );
}

function AdminRulesView({
    rules,
    rulesLoading,
    dataLoading,
    drafts,
    setDrafts,
    selectedKey,
    setSelectedKey,
    onJumpToPreview,
}: {
    rules: PromptRule[];
    rulesLoading: boolean;
    dataLoading: boolean;
    drafts: PromptRuleDraftMap;
    setDrafts: Dispatch<SetStateAction<PromptRuleDraftMap>>;
    selectedKey: string | null;
    setSelectedKey: (key: string | null) => void;
    onJumpToPreview: () => void;
}) {
    const { data: plans = [] } = useAccessPlanList();
    const { data: channelItems = [] } = useChannelList();
    const create = useCreatePromptRule();
    const update = useUpdatePromptRule();
    const remove = useDeletePromptRule();
    const toggle = useTogglePromptRule();
    const reorder = useReorderPromptRules();
    const channels = useMemo(() => sortChannelsStable(channelItems.map((item) => item.raw)), [channelItems]);

    const [identityMode, setIdentityMode] = useState<IdentityPresetMode>('brand');
    const [identityName, setIdentityName] = useState('Octopus 助手');
    const [identityStyle, setIdentityStyle] = useState('专业、清晰、简洁');
    const identityPreset = useMemo(
        () => buildIdentityPreset({ mode: identityMode, displayName: identityName, style: identityStyle }),
        [identityMode, identityName, identityStyle],
    );

    const planOptions = useMemo<PlanOption[]>(() => plans.map((plan) => ({ id: plan.id, name: plan.display_name || plan.slug })), [plans]);
    const channelOptions = useMemo<ChannelOption[]>(() => channels.map((channel) => ({ id: channel.id, name: channel.name })), [channels]);
    const planNames = useMemo(() => new Map(planOptions.map((plan) => [plan.id, plan.name])), [planOptions]);
    const channelNames = useMemo(() => new Map(channelOptions.map((channel) => [channel.id, channel.name])), [channelOptions]);

    const savedRule = selectedKey && selectedKey !== NEW_RULE_KEY ? rules.find((rule) => String(rule.id) === selectedKey) ?? null : null;
    const draft = selectedKey ? drafts[selectedKey] ?? null : null;
    const dirtyKeys = useMemo(() => new Set(dirtyRuleDraftKeys(drafts)), [drafts]);
    const dirtyCount = dirtyKeys.size;

    const select = (key: string) => {
        if (key === selectedKey) return;
        const previous = selectedKey ? drafts[selectedKey] : undefined;
        if (previous && !promptRuleValuesEqual(previous.value, previous.baseline)) {
            const name = savedRule?.name ?? '新建规则';
            toast.warning(`「${name}」改了还没保存，改动留在草稿里`);
        }
        setSelectedKey(key);
    };

    const startNew = () => {
        if (selectedKey === NEW_RULE_KEY) return;
        setDrafts((map) => map[NEW_RULE_KEY] ? map : { ...map, [NEW_RULE_KEY]: { value: blankPromptRuleDraft(), baseline: blankPromptRuleDraft() } });
        // 同样走 select 的脏草稿提醒，别让「新建」成了免提示的旁路。
        select(NEW_RULE_KEY);
    };

    const submit = async () => {
        const key = selectedKey;
        if (!key || !draft) return;
        // sort 用服务端当前值：草稿里的 sort 可能已经被上移下移调旧了。
        const serverSort = draft.value.id ? rules.find((rule) => rule.id === draft.value.id)?.sort : undefined;
        const payload = promptRuleDraftPayload(draft.value, serverSort);
        try {
            if (payload.id) {
                await update.mutateAsync({ ...payload, id: payload.id });
                setDrafts((map) => acceptRuleDraftSave(map, key, { ...payload }));
                toast.success(`「${payload.name}」已保存`);
            } else {
                const created = await create.mutateAsync(payload);
                setDrafts((map) => dropRuleDraft(map, key));
                setSelectedKey(created.id ? String(created.id) : null);
                toast.success(`「${created.name || payload.name}」已创建`);
            }
        } catch (error) {
            toast.error('保存失败', { description: apiErrorMessage(error) });
        }
    };

    const removeCurrent = async () => {
        if (!savedRule) return;
        try {
            await remove.mutateAsync(savedRule.id);
            setDrafts((map) => dropRuleDraft(map, String(savedRule.id)));
            setSelectedKey(null);
            toast.success(`「${savedRule.name}」已删除`);
        } catch (error) {
            toast.error('删除失败', { description: apiErrorMessage(error) });
        }
    };

    const copyCurrent = async () => {
        if (!savedRule) return;
        const source = promptRuleFromSaved(savedRule);
        const copy = { ...source, id: undefined, name: promptRuleCopyName(source.name) };
        try {
            const created = await create.mutateAsync(promptRuleDraftPayload(copy));
            toast.success(`已复制为「${copy.name}」，可以继续改`);
            setSelectedKey(created.id ? String(created.id) : null);
        } catch (error) {
            toast.error('复制失败', { description: apiErrorMessage(error) });
        }
    };

    const toggleRule = (rule: PromptRule, enabled: boolean) => {
        const key = String(rule.id);
        const state = drafts[key];
        if (state && !promptRuleValuesEqual(state.value, state.baseline)) {
            toast.warning(`「${rule.name}」有未保存的草稿，先保存再开关`);
            return;
        }
        toggle.mutateAsync({ id: rule.id, enabled })
            .then(() => {
                // 服务端已经改过了：草稿里的 enabled 要跟上，否则右栏再保存会把开关开/关回去。
                // 只动 enabled，其它字段（包括保存后又打的字）原样保留。
                setDrafts((map) => {
                    const entry = map[key];
                    if (!entry) return map;
                    return {
                        ...map,
                        [key]: { value: { ...entry.value, enabled }, baseline: { ...entry.baseline, enabled } },
                    };
                });
                toast.success(enabled ? `「${rule.name}」已启用` : `「${rule.name}」已关闭`);
            })
            .catch((error) => toast.error('开关失败', { description: apiErrorMessage(error) }));
    };

    /** 同一类规则里上移/下移一条：提交全量顺序，换完把已保存的草稿基线刷成新顺序。 */
    const moveRule = (rule: PromptRule, direction: 'up' | 'down') => {
        const ids = promptRuleReorderIDs(rules, rule.id, direction);
        if (!ids) return;
        reorder.mutateAsync({ ids })
            .then(() => {
                // 只丢掉「没改动」的草稿，让它们的基线重新取服务端的新 sort；脏草稿留着不动。
                setDrafts((map) => {
                    const next: PromptRuleDraftMap = {};
                    for (const [key, state] of Object.entries(map)) {
                        if (key === NEW_RULE_KEY || !promptRuleValuesEqual(state.value, state.baseline)) next[key] = state;
                    }
                    return next;
                });
                toast.success(`「${rule.name}」已${direction === 'up' ? '往前' : '往后'}挪`);
            })
            .catch((error) => toast.error('调顺序失败', { description: apiErrorMessage(error) }));
    };

    /** 品牌模板落到「全局那条」：范围全开的那条规则；一条都没有就落进新建草稿。 */
    const applyIdentityToGlobalRule = () => {
        if (!identityPreset.trim()) {
            toast.warning('先填助手名称，才会生成可插入的模板');
            return;
        }
        const globalish = rules.find((rule) => rule.plan_mode === 'all' && rule.channel_mode === 'all' && rule.model_mode === 'all');
        const key = globalish ? String(globalish.id) : NEW_RULE_KEY;
        const baseText = globalish
            ? (drafts[key]?.value.text ?? globalish.text)
            : (drafts[NEW_RULE_KEY]?.value.text ?? '');
        let nextText: string;
        try {
            nextText = upsertIdentityPreset(baseText, identityPreset);
        } catch (error) {
            toast.error('未修改草稿', { description: apiErrorMessage(error) ?? String(error) });
            return;
        }
        setDrafts((map) => {
            const baselineDraft = globalish ? promptRuleFromSaved(globalish) : (map[NEW_RULE_KEY]?.baseline ?? blankPromptRuleDraft());
            return editRuleDraft(map, key, baselineDraft, (value) => ({ ...value, text: nextText, enabled: true }));
        });
        setSelectedKey(key);
        toast.success(globalish ? `已填入「${globalish.name}」草稿，核对范围后点保存` : '已填入新建规则草稿，起个名字再保存');
    };

    return (
        <PageWrapper className="h-full min-h-0 overflow-y-auto overscroll-contain space-y-4 rounded-t-3xl pb-[calc(7rem+env(safe-area-inset-bottom))] md:pb-4">
            <PromptIdentityBuilder
                mode={identityMode}
                onModeChange={setIdentityMode}
                displayName={identityName}
                onDisplayNameChange={setIdentityName}
                style={identityStyle}
                onStyleChange={setIdentityStyle}
                preview={identityPreset}
                onApplyGlobal={applyIdentityToGlobalRule}
            />
            {dirtyCount > 0 ? (
                <p className="rounded-xl border border-amber-500/30 bg-amber-500/5 px-3 py-2 text-sm">
                    有 {dirtyCount} 条规则的改动还没保存；在页内切页签、换条不会丢，离开这个页面前记得先点「保存」。
                </p>
            ) : null}
            {dataLoading && !rulesLoading ? (
                <section className={cn(SECTION, 'flex justify-center py-10 text-muted-foreground')}>
                    <Loader2 className="size-5 animate-spin" />
                </section>
            ) : (
                <div className="grid items-start gap-3 lg:grid-cols-[minmax(0,4fr)_minmax(0,7fr)]">
                    <RuleList
                        rules={rules}
                        plans={planOptions}
                        channels={channelOptions}
                        loading={rulesLoading}
                        selectedKey={selectedKey}
                        togglePendingId={toggle.isPending ? toggle.variables?.id ?? null : null}
                        dirtyKeys={dirtyKeys}
                        onSelect={select}
                        onNew={startNew}
                        onToggle={toggleRule}
                        onMove={moveRule}
                        reorderPending={reorder.isPending}
                        planNames={planNames}
                        channelNames={channelNames}
                    />
                    <RuleEditor
                        key={selectedKey ?? 'none'}
                        saved={savedRule}
                        draft={draft}
                        saving={update.isPending || create.isPending}
                        copying={create.isPending && Boolean(savedRule)}
                        deleting={remove.isPending}
                        plans={planOptions}
                        channels={channelOptions}
                        planNames={planNames}
                        channelNames={channelNames}
                        savedRules={rules}
                        identityPreset={identityPreset}
                        onChange={(mutate) => {
                            if (!selectedKey) return;
                            setDrafts((map) => editRuleDraft(map, selectedKey, savedRule ?? blankPromptRuleDraft(), mutate));
                        }}
                        onSave={() => void submit()}
                        onDelete={() => void removeCurrent()}
                        onCopy={() => void copyCurrent()}
                        onJumpToPreview={onJumpToPreview}
                    />
                </div>
            )}
        </PageWrapper>
    );
}

export function PromptManagement() {
    const role = useAuthStore((state) => state.user?.role);
    return role === 'admin' ? <AdminPromptWorkspace /> : <UserPromptView />;
}
