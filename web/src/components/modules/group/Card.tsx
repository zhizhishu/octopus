'use client';

import { useState, useMemo, useCallback, useEffect, useRef } from 'react';
import { Trash2, X, Pencil, RefreshCw, AlertTriangle } from 'lucide-react';
import { motion, AnimatePresence } from 'motion/react';
import { type Group, useDeleteGroup, useUpdateGroup } from '@/api/endpoints/group';
import { useModelChannelList } from '@/api/endpoints/model';
import { useChannelList } from '@/api/endpoints/channel';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import { toast } from '@/components/common/Toast';
import { CopyIconButton } from '@/components/common/CopyButton';
import { SafeText } from '@/components/common/SafeText';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import type { SelectedMember } from './ItemList';
import { MemberList } from './ItemList';
import { GroupEditor, type GroupEditorValues } from './Editor';
import { activeModelChannelKeySet, activeModelChannels, buildChannelNameByModelKey, matchGroupModelChannels, memberKey, modelChannelKey, MODE_LABELS } from './utils';
import { SELECTABLE_GROUP_MODES, normalizeGroupMode, type GroupUpdateRequest } from '@/api/endpoints/group';
import {
    MorphingDialog,
    MorphingDialogClose,
    MorphingDialogContainer,
    MorphingDialogContent,
    MorphingDialogDescription,
    MorphingDialogTitle,
    MorphingDialogTrigger,
    useMorphingDialog,
} from '@/components/ui/morphing-dialog';

interface EditDialogContentProps {
    group: Group;
    displayMembers: SelectedMember[];
    isSubmitting: boolean;
    onSubmit: (values: GroupEditorValues, onDone?: () => void) => void;
}

function EditDialogContent({ group, displayMembers, isSubmitting, onSubmit }: EditDialogContentProps) {
    const { setIsOpen } = useMorphingDialog();
    const t = useTranslations('group');
    return (
        <>
            <MorphingDialogTitle className="shrink-0">
                <header className="mb-3 flex items-center justify-between">
                    <h2 className="text-2xl font-bold text-card-foreground">
                        {t('detail.actions.edit')}
                    </h2>
                    <MorphingDialogClose className="relative right-0 top-0" />
                </header>
            </MorphingDialogTitle>
            <MorphingDialogDescription className="flex-1 min-h-0 overflow-hidden">
                <GroupEditor
                    key={`edit-group-${group.id}`}
                    initial={{
                        name: group.name,
                        match_regex: group.match_regex ?? '',
                        mode: group.mode,
                        mode_locked: group.mode_locked ?? false,
                        first_token_time_out: group.first_token_time_out ?? 0,
                        session_keep_time: group.session_keep_time ?? 0,
                        max_concurrent: group.max_concurrent ?? 0,
                        rpm_limit: group.rpm_limit ?? 0,
                        members: displayMembers,
                    }}
                    submitText={t('detail.actions.save')}
                    submittingText={t('create.submitting')}
                    isSubmitting={isSubmitting}
                    onCancel={() => setIsOpen(false)}
                    onSubmit={(v) => onSubmit(v, () => setIsOpen(false))}
                />
            </MorphingDialogDescription>
        </>
    );
}

export function GroupCard({ group, duplicateCount = 1 }: { group: Group; duplicateCount?: number }) {
    const t = useTranslations('group');
    const updateGroup = useUpdateGroup();
    const deleteGroup = useDeleteGroup();
    const { data: modelChannels = [] } = useModelChannelList();
    const { data: channelRows = [] } = useChannelList();
    const visibleModelChannels = useMemo(() => activeModelChannels(modelChannels), [modelChannels]);
    const visibleModelKeys = useMemo(() => activeModelChannelKeySet(modelChannels), [modelChannels]);
    // channel_id -> 渠道优先级(Channel.Priority)，供成员行展示每个渠道自身的 P 值。
    const channelPriorityById = useMemo(() => {
        const map = new Map<number, number>();
        channelRows.forEach(({ raw }) => map.set(raw.id, raw.priority));
        return map;
    }, [channelRows]);

    const [confirmDelete, setConfirmDelete] = useState(false);
    const [members, setMembers] = useState<SelectedMember[]>([]);
    const [isRefreshingMatched, setIsRefreshingMatched] = useState(false);
    const isDragging = useRef(false);
    const weightTimerRef = useRef<NodeJS.Timeout | null>(null);
    const membersRef = useRef<SelectedMember[]>([]);

    const channelNameByKey = useMemo(() => buildChannelNameByModelKey(modelChannels), [modelChannels]);
    const rawItemCount = group.items?.length ?? 0;
    const displayMembers = useMemo((): SelectedMember[] =>
        [...(group.items || [])]
            .sort((a, b) => a.priority - b.priority)
            .filter((item) => visibleModelKeys.has(modelChannelKey(item.channel_id, item.model_name)))
            .map((item) => ({
                id: modelChannelKey(item.channel_id, item.model_name),
                name: item.model_name,
                enabled: true,
                channel_id: item.channel_id,
                channel_name: channelNameByKey.get(modelChannelKey(item.channel_id, item.model_name)) ?? `Channel ${item.channel_id}`,
                item_id: item.id,
                weight: item.weight,
            })),
        [group.items, channelNameByKey, visibleModelKeys]
    );
    const isEffectivelyEmpty = displayMembers.length === 0;

    const { matchedModelChannels, regexError } = useMemo(
        () => matchGroupModelChannels(visibleModelChannels, group.name, group.match_regex ?? ''),
        [visibleModelChannels, group.name, group.match_regex]
    );

    const memberPanelHeight = `clamp(5.5rem, ${Math.max(1, members.length) * 2.35 + 1.25}rem, min(16vh, 14rem))`;
    const canRefreshMatched = Boolean(group.id) && !regexError && matchedModelChannels.length > 0 && !updateGroup.isPending && !isRefreshingMatched;

    useEffect(() => {
        if (isDragging.current) return;
        // Local members are a drag/weight-edit buffer; sync it from the server snapshot when idle.
        // eslint-disable-next-line react-hooks/set-state-in-effect
        setMembers([...displayMembers]);
    }, [displayMembers]);

    useEffect(() => {
        membersRef.current = members;
    }, [members]);

    useEffect(() => {
        return () => { if (weightTimerRef.current) clearTimeout(weightTimerRef.current); };
    }, []);

    const onSuccess = useCallback(() => toast.success(t('toast.updated')), [t]);
    const onError = useCallback((error: Error) => toast.error(t('toast.updateFailed'), { description: error.message }), [t]);

    // Avoid UI flicker: drag-reorder also uses the same mutation, so only "mode switch" should lock mode buttons.
    const isUpdatingMode = (() => {
        if (!updateGroup.isPending) return false;
        const v = updateGroup.variables;
        if (typeof v !== 'object' || v === null) return false;
        return 'mode' in v && typeof (v as { mode?: unknown }).mode === 'number';
    })();

    const priorityByItemId = useMemo(() => {
        const map = new Map<number, number>();
        (group.items || []).forEach((item) => {
            if (item.id !== undefined) map.set(item.id, item.priority);
        });
        return map;
    }, [group.items]);

    const handleDragStart = useCallback(() => { isDragging.current = true; }, []);
    const handleDragFinish = useCallback(() => { isDragging.current = false; }, []);

    const handleDropReorder = useCallback((nextMembers: SelectedMember[]) => {
        const itemsToUpdate = nextMembers
            .map((m, i) => ({ member: m, newPriority: i + 1 }))
            .filter(({ member, newPriority }) => {
                if (!member.item_id) return false;
                const origPriority = priorityByItemId.get(member.item_id);
                return origPriority !== undefined && origPriority !== newPriority;
            })
            .map(({ member, newPriority }) => ({ id: member.item_id!, priority: newPriority, weight: member.weight ?? 1 }));
        if (itemsToUpdate.length > 0) updateGroup.mutate({ id: group.id!, items_to_update: itemsToUpdate }, { onSuccess, onError });
    }, [group.id, priorityByItemId, updateGroup, onSuccess, onError]);

    const handleRemoveMember = useCallback((id: string) => {
        const member = members.find((m) => m.id === id);
        if (member?.item_id !== undefined) updateGroup.mutate({ id: group.id!, items_to_delete: [member.item_id] }, { onSuccess, onError });
    }, [members, group.id, updateGroup, onSuccess, onError]);

    const handleWeightChange = useCallback((id: string, weight: number) => {
        setMembers((prev) => prev.map((m) => m.id === id ? { ...m, weight } : m));
        if (weightTimerRef.current) clearTimeout(weightTimerRef.current);
        weightTimerRef.current = setTimeout(() => {
            const member = membersRef.current.find((m) => m.id === id);
            if (!member?.item_id) return;
            const priority = priorityByItemId.get(member.item_id);
            if (!priority) return;
            updateGroup.mutate(
                { id: group.id!, items_to_update: [{ id: member.item_id, priority, weight }] },
                { onSuccess, onError }
            );
        }, 500);
    }, [group.id, priorityByItemId, updateGroup, onSuccess, onError]);

    const handleRefreshMatchedRoutes = useCallback(() => {
        if (!group.id) return;
        if (regexError) {
            toast.error(t('toast.updateFailed'), { description: regexError });
            return;
        }
        if (matchedModelChannels.length === 0) return;

        const originalItems = [...(group.items || [])].sort((a, b) => a.priority - b.priority);
        const originalByKey = new Map<string, { id?: number; priority: number; weight: number }>();
        originalItems.forEach((item) => {
            originalByKey.set(modelChannelKey(item.channel_id, item.model_name), {
                id: item.id,
                priority: item.priority,
                weight: item.weight,
            });
        });

        const matchedKeys = new Set(matchedModelChannels.map(memberKey));
        const items_to_delete = originalItems
            .filter((item) => !matchedKeys.has(modelChannelKey(item.channel_id, item.model_name)))
            .map((item) => item.id)
            .filter((id): id is number => typeof id === 'number');

        const items_to_add = matchedModelChannels
            .map((mc, index) => ({ mc, priority: index + 1 }))
            .filter(({ mc }) => !originalByKey.has(memberKey(mc)))
            .map(({ mc, priority }) => ({
                channel_id: mc.channel_id,
                model_name: mc.name,
                priority,
                weight: 1,
            }));

        const items_to_update = matchedModelChannels
            .map((mc, index) => ({ mc, priority: index + 1 }))
            .map(({ mc, priority }) => {
                const original = originalByKey.get(memberKey(mc));
                if (!original?.id) return null;
                const weight = original.weight || 1;
                if (original.priority === priority && original.weight === weight) return null;
                return { id: original.id, priority, weight };
            })
            .filter((item): item is { id: number; priority: number; weight: number } => item !== null);

        const payload: GroupUpdateRequest = { id: group.id };
        if (items_to_add.length) payload.items_to_add = items_to_add;
        if (items_to_update.length) payload.items_to_update = items_to_update;
        if (items_to_delete.length) payload.items_to_delete = items_to_delete;

        if (Object.keys(payload).length === 1) {
            onSuccess();
            return;
        }

        setIsRefreshingMatched(true);
        updateGroup.mutate(payload, {
            onSuccess,
            onError,
            onSettled: () => setIsRefreshingMatched(false),
        });
    }, [group.id, group.items, matchedModelChannels, regexError, t, updateGroup, onSuccess, onError]);

    const handleSubmitEdit = useCallback((values: GroupEditorValues, onDone?: () => void) => {
        if (!group.id) return;

        const originalItems = [...(group.items || [])].sort((a, b) => a.priority - b.priority);
        const originalById = new Map<number, { priority: number; weight: number }>();
        const originalIds = new Set<number>();
        originalItems.forEach((it) => {
            if (typeof it.id === 'number') {
                originalIds.add(it.id);
                originalById.set(it.id, { priority: it.priority, weight: it.weight });
            }
        });

        const newIds = new Set<number>();
        values.members.forEach((m) => { if (typeof m.item_id === 'number') newIds.add(m.item_id); });

        const items_to_delete = Array.from(originalIds).filter((id) => !newIds.has(id));

        const items_to_add = values.members
            .map((m, idx) => ({ m, priority: idx + 1 }))
            .filter(({ m }) => typeof m.item_id !== 'number')
            .map(({ m, priority }) => ({
                channel_id: m.channel_id,
                model_name: m.name,
                priority,
                weight: m.weight ?? 1,
            }));

        const items_to_update = values.members
            .map((m, idx) => ({ m, priority: idx + 1 }))
            .filter(({ m }) => typeof m.item_id === 'number')
            .map(({ m, priority }) => {
                const id = m.item_id!;
                const orig = originalById.get(id);
                const weight = m.weight ?? 1;
                if (!orig) return null;
                if (orig.priority === priority && orig.weight === weight) return null;
                return { id, priority, weight };
            })
            .filter((x): x is { id: number; priority: number; weight: number } => x !== null);

        const payload: GroupUpdateRequest = { id: group.id };
        const nextName = values.name.trim();
        const nextRegex = (values.match_regex ?? '').trim();
        const nextFirstTokenTimeOut = values.first_token_time_out ?? 0;
        const nextSessionKeepTime = values.session_keep_time ?? 0;
        const nextMaxConcurrent = values.max_concurrent ?? 0;
        const nextRpmLimit = values.rpm_limit ?? 0;

        if (nextName && nextName !== group.name) payload.name = nextName;
        if (values.mode !== group.mode) payload.mode = values.mode;
        if (values.mode_locked !== (group.mode_locked ?? false)) payload.mode_locked = values.mode_locked;
        if (nextRegex !== (group.match_regex ?? '')) payload.match_regex = nextRegex;
        if (nextFirstTokenTimeOut !== (group.first_token_time_out ?? 0)) payload.first_token_time_out = nextFirstTokenTimeOut;
        if (nextSessionKeepTime !== (group.session_keep_time ?? 0)) payload.session_keep_time = nextSessionKeepTime;
        if (nextMaxConcurrent !== (group.max_concurrent ?? 0)) payload.max_concurrent = nextMaxConcurrent;
        if (nextRpmLimit !== (group.rpm_limit ?? 0)) payload.rpm_limit = nextRpmLimit;
        if (items_to_add.length) payload.items_to_add = items_to_add;
        if (items_to_update.length) payload.items_to_update = items_to_update;
        if (items_to_delete.length) payload.items_to_delete = items_to_delete;

        if (Object.keys(payload).length === 1) {
            onDone?.();
            return;
        }

        updateGroup.mutate(payload, {
            onSuccess: () => {
                onSuccess();
                onDone?.();
            },
            onError,
        });
    }, [group.first_token_time_out, group.session_keep_time, group.max_concurrent, group.rpm_limit, group.id, group.items, group.match_regex, group.mode, group.mode_locked, group.name, onSuccess, onError, updateGroup]);

    return (
        <article
            className={cn(
                'flex flex-col rounded-2xl border bg-card text-card-foreground p-3 custom-shadow transition-colors',
                isEffectivelyEmpty
                    ? 'border-dashed border-border/80 bg-muted/15 text-muted-foreground'
                    : 'border-border'
            )}
            data-empty={isEffectivelyEmpty}
            data-hidden-members={rawItemCount > 0 && isEffectivelyEmpty}
        >
            <header className="flex items-start justify-between mb-3 relative overflow-visible rounded-xl -mx-1 px-1 -my-1 py-1">
                <div className="relative flex-1 mr-2 min-w-0 group/title">
                    <Tooltip side="top" sideOffset={10} align="center">
                        <TooltipTrigger asChild>
                            <h3 className="text-lg font-bold">
                                <SafeText value={group.name} className="block" />
                            </h3>
                        </TooltipTrigger>
                        <TooltipContent key={group.name}>
                            <SafeText value={group.name} mode="wrap" className="max-w-64" />
                        </TooltipContent>
                    </Tooltip>
                    {duplicateCount > 1 && (
                        <span className="mt-1 inline-flex w-fit items-center gap-1 rounded-full bg-destructive/10 px-2 py-0.5 text-[11px] font-semibold text-destructive ring-1 ring-destructive/25">
                            <AlertTriangle className="size-3" /> 同名 ×{duplicateCount}
                        </span>
                    )}
                </div>

                <div className="flex items-center gap-1 shrink-0">
                    <Tooltip side="top" sideOffset={10} align="center">
                        <TooltipTrigger asChild>
                            <button
                                type="button"
                                onClick={handleRefreshMatchedRoutes}
                                disabled={!canRefreshMatched}
                                className="p-1.5 rounded-lg transition-colors hover:bg-muted text-muted-foreground hover:text-foreground disabled:cursor-not-allowed disabled:opacity-40"
                            >
                                <RefreshCw className={cn('size-4', isRefreshingMatched && 'animate-spin')} />
                            </button>
                        </TooltipTrigger>
                        <TooltipContent>
                            {regexError ? regexError : t('detail.actions.refreshMatched')}
                        </TooltipContent>
                    </Tooltip>

                    <MorphingDialog>
                        <MorphingDialogTrigger className="p-1.5 rounded-lg transition-colors hover:bg-muted text-muted-foreground hover:text-foreground">
                            <Tooltip side="top" sideOffset={10} align="center">
                                <TooltipTrigger asChild>
                                    <Pencil className="size-4" />
                                </TooltipTrigger>
                                <TooltipContent>{t('detail.actions.edit')}</TooltipContent>
                            </Tooltip>
                        </MorphingDialogTrigger>

                        <MorphingDialogContainer>
                            <MorphingDialogContent className="relative flex h-[calc(100dvh-1rem)] w-[calc(100vw-1rem)] max-w-full flex-col overflow-hidden rounded-3xl bg-card px-4 py-4 text-card-foreground md:max-w-4xl md:px-6">
                                <EditDialogContent
                                    group={group}
                                    displayMembers={displayMembers}
                                    isSubmitting={updateGroup.isPending}
                                    onSubmit={handleSubmitEdit}
                                />
                            </MorphingDialogContent>
                        </MorphingDialogContainer>
                    </MorphingDialog>

                    <Tooltip side="top" sideOffset={10} align="center">
                        <TooltipTrigger>
                            <CopyIconButton
                                text={group.name}
                                className="p-1.5 rounded-lg transition-colors hover:bg-muted text-muted-foreground hover:text-foreground"
                                copyIconClassName="size-4"
                                checkIconClassName="size-4 text-primary"
                            />
                        </TooltipTrigger>
                        <TooltipContent>{t('detail.actions.copyName')}</TooltipContent>
                    </Tooltip>
                    {!confirmDelete && (
                        <Tooltip side="top" sideOffset={10} align="center">
                            <TooltipTrigger>
                                <motion.button layoutId={`delete-btn-group-${group.id}`} type="button" onClick={() => setConfirmDelete(true)} className="p-1.5 rounded-lg hover:bg-destructive/10 text-muted-foreground hover:text-destructive transition-colors">
                                    <Trash2 className="size-4" />
                                </motion.button>
                            </TooltipTrigger>
                            <TooltipContent>{t('detail.actions.delete')}</TooltipContent>
                        </Tooltip>
                    )}
                </div>

                <AnimatePresence>
                    {confirmDelete && (
                        <motion.div layoutId={`delete-btn-group-${group.id}`} className="absolute inset-0 flex items-center justify-center gap-2 bg-destructive p-2 rounded-xl" transition={{ type: 'spring', stiffness: 400, damping: 30 }}>
                            <button type="button" onClick={() => setConfirmDelete(false)} className="flex h-7 w-7 items-center justify-center rounded-lg bg-destructive-foreground/20 text-destructive-foreground transition-all hover:bg-destructive-foreground/30 active:scale-95">
                                <X className="size-4" />
                            </button>
                            <button type="button" onClick={() => group.id && deleteGroup.mutate(group.id, { onSuccess: () => toast.success(t('toast.deleted')) })} disabled={deleteGroup.isPending} className="flex-1 h-7 flex items-center justify-center gap-2 rounded-lg bg-destructive-foreground text-destructive text-sm font-semibold transition-all hover:bg-destructive-foreground/90 active:scale-[0.98] disabled:opacity-50 disabled:cursor-not-allowed">
                                <Trash2 className="size-3.5" />
                                {t('detail.actions.confirmDelete')}
                            </button>
                        </motion.div>
                    )}
                </AnimatePresence>
            </header>

            {/* Mode: quick switch (no need to enter Edit) */}
            <div className="flex gap-1 mb-3">
                {SELECTABLE_GROUP_MODES.map((m) => (
                    <button
                        key={m}
                        type="button"
                        aria-disabled={isUpdatingMode || !group.id}
                        onClick={() => {
                            if (isUpdatingMode || !group.id) return;
                            if (m === normalizeGroupMode(group.mode)) return;
                            updateGroup.mutate({ id: group.id!, mode: m }, { onSuccess, onError });
                        }}
                        className={cn(
                            'flex-1 py-1 text-xs rounded-lg transition-colors',
                            normalizeGroupMode(group.mode) === m ? 'bg-primary text-primary-foreground' : 'bg-muted hover:bg-muted/80',
                            // Keep visuals stable (no opacity/disabled flicker) while still preventing double-submit via onClick guard.
                            (!group.id) && 'cursor-not-allowed opacity-50'
                        )}
                    >
                        {t(`mode.${MODE_LABELS[m]}`)}
                    </button>
                ))}
            </div>

            <section
                className={cn(
                    'relative overflow-hidden rounded-xl border border-border/50 transition-[height] duration-200',
                    isEffectivelyEmpty ? 'bg-muted/20' : 'bg-muted/30'
                )}
                style={{ height: memberPanelHeight }}
            >
                <MemberList
                    members={members}
                    onReorder={setMembers}
                    onRemove={handleRemoveMember}
                    onWeightChange={handleWeightChange}
                    onDragStart={handleDragStart}
                    onDrop={handleDropReorder}
                    onDragFinish={handleDragFinish}
                    autoScrollOnAdd={false}
                    showWeight={false}
                    layoutScope={`card-${group.id ?? 'unknown'}`}
                    channelPriorityById={channelPriorityById}
                />
            </section>
        </article >
    );
}
