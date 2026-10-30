'use client';

import type { ReactNode } from 'react';
import { Loader2 } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog';
import { btn, btnDanger, btnPrimary } from './ui-bits';

export function ConfirmDialog({ open, onOpenChange, title, description, confirmLabel, danger, pending, onConfirm, children }: {
    open: boolean;
    onOpenChange: (open: boolean) => void;
    title: string;
    description?: ReactNode;
    confirmLabel: string;
    danger?: boolean;
    pending?: boolean;
    onConfirm: () => void;
    children?: ReactNode;
}) {
    const t = useTranslations('user.common');
    return (
        <Dialog open={open} onOpenChange={onOpenChange}>
            <DialogContent className="sm:max-w-[440px]">
                <DialogHeader>
                    <DialogTitle>{title}</DialogTitle>
                    {description && <DialogDescription>{description}</DialogDescription>}
                </DialogHeader>
                {children}
                <DialogFooter>
                    <button type="button" className={btn} onClick={() => onOpenChange(false)}>{t('cancel')}</button>
                    <button type="button" className={danger ? btnDanger : btnPrimary} disabled={pending} onClick={onConfirm}>
                        {pending && <Loader2 className="size-4 animate-spin" />}
                        {confirmLabel}
                    </button>
                </DialogFooter>
            </DialogContent>
        </Dialog>
    );
}
