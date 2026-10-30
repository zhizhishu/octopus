'use client';

import { ChevronLeft, ChevronRight } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import { pageButtons } from '../lib/user-filters';
import { iconBtn } from './ui-bits';

// 表格底部分页：共 N 条 · 每页 M 条 + 页码（用户页、API Key 页共用）。
export function Pager({ page, pages, total, size, onPage, summary }: { page: number; pages: number; total: number; size: number; onPage: (p: number) => void; summary?: string }) {
    const t = useTranslations('user.pager');
    return (
        <div className="flex items-center justify-between gap-3 px-4 py-3 text-xs text-muted-foreground">
            <span>{summary ?? t('summary', { total, size })}</span>
            <div className="flex items-center gap-1">
                <button type="button" aria-label={t('prev')} className={iconBtn} disabled={page <= 1} onClick={() => onPage(page - 1)}><ChevronLeft className="size-4" /></button>
                {pageButtons(page, pages).map((n, i) => n === 0
                    ? <span key={`gap-${i}`} className="px-1">…</span>
                    : (
                        <button key={n} type="button" onClick={() => onPage(n)} className={cn('inline-flex size-8 items-center justify-center rounded-lg', n === page ? 'bg-muted font-medium text-foreground' : 'hover:bg-muted/60')}>{n}</button>
                    ))}
                <button type="button" aria-label={t('next')} className={iconBtn} disabled={page >= pages} onClick={() => onPage(page + 1)}><ChevronRight className="size-4" /></button>
            </div>
        </div>
    );
}
