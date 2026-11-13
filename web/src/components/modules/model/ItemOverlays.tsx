'use client';

import { Check, Loader, RotateCcw, Trash2, X } from 'lucide-react';
import { motion } from 'motion/react';
import { useTranslations } from 'next-intl';
import { DialogContent, DialogDescription, DialogTitle } from '@/components/ui/dialog';
import { Input } from '@/components/ui/input';
import type { PriceEditValues } from './price-edit';
import type { TokenPrice } from './price-sync';
import { Hint } from './price-sync-ui';

type EditValues = PriceEditValues;

type ModelDeleteOverlayProps = {
    layoutId: string;
    isPending: boolean;
    onCancel: () => void;
    onConfirm: () => void;
};

export function ModelDeleteOverlay({
    layoutId,
    isPending,
    onCancel,
    onConfirm,
}: ModelDeleteOverlayProps) {
    const t = useTranslations('model.overlay');
    return (
        <motion.div
            layoutId={layoutId}
            className="absolute inset-0 flex items-center justify-center gap-3 bg-destructive p-4 rounded-2xl"
            transition={{ type: 'spring', stiffness: 400, damping: 30 }}
        >
            <button
                type="button"
                onClick={onCancel}
                className="h-9 px-4 flex items-center justify-center gap-1.5 rounded-xl bg-destructive-foreground/20 text-destructive-foreground text-sm font-medium transition-all hover:bg-destructive-foreground/30 active:scale-[0.98]"
            >
                <X className="size-4" />
                {t('cancel')}
            </button>
            <button
                type="button"
                onClick={onConfirm}
                disabled={isPending}
                className="h-9 px-4 flex items-center justify-center gap-1.5 rounded-xl bg-destructive-foreground text-destructive text-sm font-medium transition-all hover:bg-destructive-foreground/90 active:scale-[0.98] disabled:opacity-50 disabled:cursor-not-allowed"
            >
                {isPending ? (
                    <Loader className="size-4 animate-spin" />
                ) : (
                    <Trash2 className="size-4" />
                )}
                {isPending ? t('deleting') : t('confirmDelete')}
            </button>
        </motion.div>
    );
}

type ModelEditOverlayProps = {
    modelName: string;
    brandColor: string;
    editValues: EditValues;
    isPending: boolean;
    /** 手动行才显示「恢复上游价」 */
    isManual?: boolean;
    library?: TokenPrice | null;
    onRestore?: () => void;
    onChange: (next: EditValues) => void;
    onCancel: () => void;
    onSave: () => void;
};

/**
 * 编辑弹窗走 @/components/ui/dialog 的 Radix Dialog：真正视口居中、focus trap、
 * ESC/点外关闭（pending 时由 Item.tsx 的 onOpenChange 受控拦截）、窄屏可滚动。
 * 本组件只负责弹窗内容，外壳样式集中在 DialogContent 的 className 上。
 */
export function ModelEditOverlay({
    modelName,
    brandColor,
    editValues,
    isPending,
    isManual = false,
    library = null,
    onRestore,
    onChange,
    onCancel,
    onSave,
}: ModelEditOverlayProps) {
    const t = useTranslations('model.overlay');
    return (
        <DialogContent
            showCloseButton={false}
            className="gap-0 rounded-3xl border-border bg-card custom-shadow p-5 sm:max-w-sm"
            onEscapeKeyDown={(event) => {
                // pending 时阻止 ESC 直接关闭，与 onOpenChange 的受控拦截双保险。
                if (isPending) event.preventDefault();
            }}
            onInteractOutside={(event) => {
                if (isPending) event.preventDefault();
            }}
        >
            <DialogTitle className="text-sm font-semibold text-card-foreground mb-1">
                {modelName}
            </DialogTitle>
            <DialogDescription className="mb-3 text-xs text-muted-foreground">
                {t('manualHint')}
            </DialogDescription>

            <div className="grid grid-cols-2 gap-2">
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {t('input')}
                    <Input
                        type="number"
                        step="any"
                        value={editValues.input}
                        onChange={(e) => onChange({ ...editValues, input: e.target.value })}
                        className="h-9 text-sm rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {t('output')}
                    <Input
                        type="number"
                        step="any"
                        value={editValues.output}
                        onChange={(e) => onChange({ ...editValues, output: e.target.value })}
                        className="h-9 text-sm rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {t('cacheRead')}
                    <Input
                        type="number"
                        step="any"
                        value={editValues.cache_read}
                        onChange={(e) => onChange({ ...editValues, cache_read: e.target.value })}
                        className="h-9 text-sm rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {t('cacheWrite')}
                    <Input
                        type="number"
                        step="any"
                        value={editValues.cache_write}
                        onChange={(e) => onChange({ ...editValues, cache_write: e.target.value })}
                        className="h-9 text-sm rounded-xl"
                    />
                </label>
                <label className="grid gap-1 text-xs text-muted-foreground">
                    {t('perCall')}
                    <Input
                        type="number"
                        step="any"
                        min="0"
                        value={editValues.per_call}
                        onChange={(e) => onChange({ ...editValues, per_call: e.target.value })}
                        className="h-9 text-sm rounded-xl"
                    />
                </label>
                <p className="col-span-2 text-[11px] leading-4 text-muted-foreground">{t('perCallTip')}</p>
            </div>

            {isManual && onRestore && (
                <Hint
                    content={library
                        ? t('restoreTip', { price: `${library.input} / ${library.output} / ${library.cache_read} / ${library.cache_write}` })
                        : t('restoreNone')}
                >
                    <button
                        type="button"
                        onClick={library ? onRestore : undefined}
                        disabled={isPending || !library}
                        className="mt-3 flex h-9 items-center justify-center gap-1.5 rounded-xl border border-border text-sm font-medium text-muted-foreground transition-all hover:bg-muted active:scale-[0.98] disabled:opacity-50"
                    >
                        <RotateCcw className="size-4" />
                        {t('restore')}
                    </button>
                </Hint>
            )}

            <div className="flex gap-2 pt-2 mt-3">
                <button
                    type="button"
                    onClick={onCancel}
                    disabled={isPending}
                    className="flex-1 h-9 flex items-center justify-center gap-1.5 rounded-xl bg-muted text-muted-foreground text-sm font-medium transition-all hover:bg-muted/80 active:scale-[0.98] disabled:opacity-50"
                >
                    <X className="size-4" />
                    {t('cancel')}
                </button>
                <button
                    type="button"
                    onClick={onSave}
                    disabled={isPending}
                    className="flex-1 h-9 flex items-center justify-center gap-1.5 rounded-xl text-sm font-medium transition-all active:scale-[0.98] disabled:opacity-50"
                    style={{ backgroundColor: brandColor, color: '#fff' }}
                >
                    {isPending ? <Loader className="size-4 animate-spin" /> : <Check className="size-4" />}
                    {t('save')}
                </button>
            </div>
        </DialogContent>
    );
}
