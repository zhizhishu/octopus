'use client';

import { memo, useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Pencil, Trash2, ArrowDownToLine, ArrowUpFromLine, Lock, Timer } from 'lucide-react';
import { motion, AnimatePresence } from 'motion/react';
import { useTranslations } from 'next-intl';
import { useUpdateModel, useDeleteModel, useRestoreModelPrice, type LLMInfo } from '@/api/endpoints/model';
import { getModelIcon } from '@/lib/model-icons';
import { toast } from '@/components/common/Toast';
import { Tooltip, TooltipContent, TooltipTrigger } from '@/components/animate-ui/components/animate/tooltip';
import { SafeText } from '@/components/common/SafeText';
import { ModelDeleteOverlay, ModelEditOverlay } from './ItemOverlays';
import { cn } from '@/lib/utils';
import { buildModelUpdatePayload, toPriceEditValues } from './price-edit';
import { displaySource, isZeroPrice, libraryPlaceholder } from './price-sync';
import { Hint } from './price-sync-ui';
import { createPortal } from 'react-dom';

interface ModelItemProps {
    model: LLMInfo;
    layout?: 'grid' | 'list';
}

export const ModelItem = memo(function ModelItem({ model, layout = 'grid' }: ModelItemProps) {
    const t = useTranslations('model');
    const isListLayout = layout === 'list';
    const [isEditOpen, setIsEditOpen] = useState(false);
    const [confirmDelete, setConfirmDelete] = useState(false);
    const [overlayRect, setOverlayRect] = useState<{ top: number; left: number; width: number } | null>(null);
    const instanceId = useId();
    const editLayoutId = `edit-btn-${model.name}-${instanceId}`;
    const deleteLayoutId = `delete-btn-${model.name}-${instanceId}`;
    const cardRef = useRef<HTMLElement | null>(null);
    const editButtonRef = useRef<HTMLButtonElement | null>(null);
    const editOverlayRef = useRef<HTMLDivElement | null>(null);
    const [editValues, setEditValues] = useState(() => toPriceEditValues(model));

    const updateModel = useUpdateModel();
    const deleteModel = useDeleteModel();
    const restorePrice = useRestoreModelPrice();
    const source = displaySource(model);
    const placeholder = libraryPlaceholder(model);
    const shown = placeholder ?? model;
    const perCall = model.per_call ?? 0;
    const perCallOnly = perCall > 0 && isZeroPrice(model);

    const { Avatar: ModelAvatar, color: brandColor } = useMemo(() => getModelIcon(model.name), [model.name]);

    const updateOverlayRect = useCallback(() => {
        const card = cardRef.current;
        if (!card) return;
        const rect = card.getBoundingClientRect();
        setOverlayRect((prev) => {
            if (prev && prev.top === rect.top && prev.left === rect.left && prev.width === rect.width) {
                return prev;
            }
            return { top: rect.top, left: rect.left, width: rect.width };
        });
    }, []);

    const closeEdit = useCallback(() => {
        setIsEditOpen(false);
    }, []);

    const handleEditClick = () => {
        setConfirmDelete(false);
        setEditValues(toPriceEditValues(model));
        // Ensure first open already has anchor geometry so layout animation can run.
        updateOverlayRect();
        setIsEditOpen(true);
    };

    const handleCancelEdit = () => {
        closeEdit();
    };

    const handleSaveEdit = () => {
        updateModel.mutate(buildModelUpdatePayload(model.name, editValues), {
            onSuccess: () => {
                closeEdit();
                toast.success(t('toast.updated'));
            },
            onError: (error) => {
                toast.error(t('toast.updateFailed'), { description: error.message });
            }
        });
    };

    const handleRestore = () => {
        restorePrice.mutate(model.name, {
            onSuccess: () => {
                closeEdit();
                toast.success(t('overlay.toastRestored'));
            },
            onError: (error) => {
                toast.error(t('overlay.toastRestoreFailed'), { description: error.message });
            },
        });
    };

    const handleDeleteClick = () => {
        closeEdit();
        setConfirmDelete(true);
    };
    const handleCancelDelete = () => setConfirmDelete(false);
    const handleConfirmDelete = () => {
        deleteModel.mutate(model.name, {
            onSuccess: () => {
                setConfirmDelete(false);
                toast.success(t('toast.deleted'));
            },
            onError: (error) => {
                setConfirmDelete(false);
                toast.error(t('toast.deleteFailed'), { description: error.message });
            }
        });
    };

    useEffect(() => {
        if (!isEditOpen) return;

        const handlePointerDown = (event: PointerEvent) => {
            const target = event.target as Node | null;
            if (!target) return;
            if (editOverlayRef.current?.contains(target)) return;
            if (editButtonRef.current?.contains(target)) return;
            closeEdit();
        };

        const handleKeyDown = (event: KeyboardEvent) => {
            if (event.key === 'Escape') closeEdit();
        };

        updateOverlayRect();
        window.addEventListener('resize', updateOverlayRect);
        window.addEventListener('scroll', updateOverlayRect, true);
        document.addEventListener('pointerdown', handlePointerDown);
        document.addEventListener('keydown', handleKeyDown);

        return () => {
            window.removeEventListener('resize', updateOverlayRect);
            window.removeEventListener('scroll', updateOverlayRect, true);
            document.removeEventListener('pointerdown', handlePointerDown);
            document.removeEventListener('keydown', handleKeyDown);
        };
    }, [isEditOpen, updateOverlayRect, closeEdit]);

    const shouldRenderEditPortal = isEditOpen || overlayRect !== null;

    return (
        <article
            ref={cardRef}
            className={cn(
                'group relative rounded-3xl border border-border bg-card transition-all duration-300 flex items-center gap-3 p-4',
                (isEditOpen || confirmDelete) && 'z-50'
            )}
        >
            <ModelAvatar size={52} />

            <div className="flex-1 min-w-0 flex flex-col justify-center gap-2">
                <div className="flex min-w-0 items-center gap-1.5">
                    {source === 'manual' && (
                        <Hint content={t('card.sourceManualTip')}>
                            <span
                                aria-label={t('card.sourceManual')}
                                className="inline-flex size-5 shrink-0 items-center justify-center rounded-full bg-amber-500/12 text-amber-700 ring-1 ring-amber-500/25 dark:text-amber-400"
                            >
                                <Lock className="size-3" />
                            </span>
                        </Hint>
                    )}
                    <Tooltip side="top" sideOffset={10} align="start">
                        <TooltipTrigger className='min-w-0 text-base font-semibold text-card-foreground leading-tight'>
                            <SafeText value={model.name} className="block" />
                        </TooltipTrigger>
                        <TooltipContent key={model.name}>
                            <SafeText value={model.name} mode="wrap" className="max-w-72" />
                        </TooltipContent>
                    </Tooltip>
                </div>

                {source === 'none' ? (
                    <div className="min-w-0">
                        <p className="text-sm font-medium text-destructive">{t('card.noPrice')}</p>
                        {!isListLayout && <p className="truncate text-xs text-muted-foreground">{t('card.noPriceSub')}</p>}
                    </div>
                ) : perCallOnly ? (
                    <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                        <Timer className="size-3.5" style={{ color: brandColor }} />
                        {t('card.perCall')}
                        <span className="tabular-nums">${perCall.toFixed(2)}{t('card.perCallUnit')}</span>
                    </p>
                ) : isListLayout ? (
                    <p className="flex items-center gap-2 overflow-hidden text-sm text-muted-foreground whitespace-nowrap">
                        <span className="inline-flex items-center gap-1">
                            <ArrowDownToLine className="size-3.5 shrink-0" style={{ color: brandColor }} />
                            {t('card.inputCache')}
                            <span className="tabular-nums">{shown.input.toFixed(2)}/{shown.cache_read.toFixed(2)}$</span>
                        </span>
                        <span className="text-muted-foreground/60">|</span>
                        <span className="inline-flex items-center gap-1 overflow-hidden">
                            <ArrowUpFromLine className="size-3.5 shrink-0" style={{ color: brandColor }} />
                            {t('card.outputCache')}
                            <span className="tabular-nums truncate">{shown.output.toFixed(2)}/{shown.cache_write.toFixed(2)}$</span>
                        </span>
                        {placeholder && (
                            <Hint content={t('card.placeholderTip')}>
                                <span className="text-xs text-muted-foreground/80 underline decoration-dotted">{t('card.placeholder')}</span>
                            </Hint>
                        )}
                    </p>
                ) : (
                    <>
                        <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                            <ArrowDownToLine className="size-3.5" style={{ color: brandColor }} />
                            {t('card.inputCache')}
                            <span className="tabular-nums">{shown.input.toFixed(2)}/{shown.cache_read.toFixed(2)}$</span>
                        </p>

                        <p className="flex items-center gap-1.5 text-sm text-muted-foreground">
                            <ArrowUpFromLine className="size-3.5" style={{ color: brandColor }} />
                            {t('card.outputCache')}
                            <span className="tabular-nums">{shown.output.toFixed(2)}/{shown.cache_write.toFixed(2)}$</span>
                            {placeholder && (
                                <Hint content={t('card.placeholderTip')}>
                                    <span className="ml-1 text-xs text-muted-foreground/80 underline decoration-dotted">{t('card.placeholder')}</span>
                                </Hint>
                            )}
                        </p>
                    </>
                )}
            </div>

            <div
                className={cn(
                    isListLayout
                        ? 'shrink-0 flex items-center gap-2 self-center'
                        : 'shrink-0 flex flex-col justify-between self-stretch',
                    (isEditOpen || confirmDelete) && 'invisible pointer-events-none'
                )}
            >
                <motion.button
                    ref={editButtonRef}
                    layoutId={editLayoutId}
                    type="button"
                    onClick={handleEditClick}
                    disabled={isEditOpen || confirmDelete}
                    className="h-9 w-9 flex items-center justify-center rounded-lg bg-muted/60 text-muted-foreground transition-colors hover:bg-muted disabled:opacity-50"
                    title={t('card.edit')}
                >
                    <Pencil className="size-4" />
                </motion.button>

                <motion.button
                    layoutId={deleteLayoutId}
                    type="button"
                    onClick={handleDeleteClick}
                    disabled={isEditOpen || confirmDelete}
                    className="h-9 w-9 flex items-center justify-center rounded-lg bg-destructive/10 text-destructive transition-colors hover:bg-destructive hover:text-destructive-foreground disabled:opacity-50"
                    title={t('card.delete')}
                >
                    <Trash2 className="size-4" />
                </motion.button>
            </div>

            <AnimatePresence>
                {confirmDelete && (
                    <ModelDeleteOverlay
                        layoutId={deleteLayoutId}
                        isPending={deleteModel.isPending}
                        onCancel={handleCancelDelete}
                        onConfirm={handleConfirmDelete}
                    />
                )}
            </AnimatePresence>

            {shouldRenderEditPortal && typeof document !== 'undefined'
                ? createPortal(
                    <AnimatePresence onExitComplete={() => setOverlayRect(null)}>
                        {isEditOpen && overlayRect && (
                            <div
                                ref={editOverlayRef}
                                className="fixed z-[90]"
                                style={{
                                    top: `${overlayRect.top}px`,
                                    left: `${overlayRect.left}px`,
                                    width: `${overlayRect.width}px`,
                                }}
                            >
                                <div className="relative">
                                    <ModelEditOverlay
                                        layoutId={editLayoutId}
                                        modelName={model.name}
                                        brandColor={brandColor}
                                        editValues={editValues}
                                        isPending={updateModel.isPending || restorePrice.isPending}
                                        isManual={source === 'manual'}
                                        library={model.library ?? null}
                                        onRestore={handleRestore}
                                        onChange={setEditValues}
                                        onCancel={handleCancelEdit}
                                        onSave={handleSaveEdit}
                                    />
                                </div>
                            </div>
                        )}
                    </AnimatePresence>,
                    document.body
                )
                : null}
        </article>
    );
});
