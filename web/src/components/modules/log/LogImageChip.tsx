'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { useQuery } from '@tanstack/react-query';
import { useTranslations } from 'next-intl';
import { useCopyToClipboard } from '@uidotdev/usehooks';
import { Copy, Download, ImageIcon, ImageOff, Loader2, Search, X } from 'lucide-react';
import { fetchLogImage } from '@/api/endpoints/log';
import { toast } from '@/components/common/Toast';
import { cn } from '@/lib/utils';
import { logImageFileName, shortLogImageHash, type SavedImagePlaceholder } from './log-image';

/**
 * 已保存图片的缩略图小卡片 + 「查看原图」灯箱。
 *
 * 原图要带登录 token 才能取（GET /api/v1/log/image/:hash，只给管理员），
 * 所以用 apiClient 拉成 Blob 再转 object URL；同一张图在 react-query 里只取一次。
 * 卡片滚进视口才开始取，弹窗里几十张图也不会一打开就全拉下来。
 */
export function LogImageChip({ placeholder }: { placeholder: SavedImagePlaceholder }) {
    const t = useTranslations('log.card.logImage');
    const { hash, mime, size } = placeholder;
    const chipRef = useRef<HTMLSpanElement>(null);
    // 没有 IntersectionObserver 的环境（老浏览器）直接当作可见。
    const [visible, setVisible] = useState(() => typeof IntersectionObserver === 'undefined');
    const [open, setOpen] = useState(false);

    useEffect(() => {
        const el = chipRef.current;
        if (!el || visible) return;
        const io = new IntersectionObserver((entries) => {
            if (entries.some((e) => e.isIntersecting)) {
                setVisible(true);
                io.disconnect();
            }
        }, { rootMargin: '200px' });
        io.observe(el);
        return () => io.disconnect();
    }, [visible]);

    const query = useQuery({
        queryKey: ['relay-log-image', hash],
        queryFn: ({ signal }) => fetchLogImage(hash, signal),
        enabled: visible,
        staleTime: Infinity,
        gcTime: 5 * 60 * 1000,
        retry: false,
        refetchOnWindowFocus: false,
    });

    const url = useObjectURL(query.data);
    const missing = isNotFound(query.error);
    const failed = !!query.error;

    const statusText = failed
        ? (missing ? t('cleaned') : t('loadFailed'))
        : null;

    return (
        <>
            <span
                ref={chipRef}
                className={cn(
                    'my-0.5 inline-flex max-w-full items-center gap-2 rounded-lg border border-border bg-card py-[3px] pl-[3px] pr-1 align-middle font-sans shadow-xs',
                    url ? 'cursor-pointer hover:border-primary' : '',
                )}
                onClick={url ? () => setOpen(true) : undefined}
                title={placeholder.raw}
            >
                <span className="grid size-10 shrink-0 place-items-center overflow-hidden rounded-md bg-muted">
                    {url ? (
                        // eslint-disable-next-line @next/next/no-img-element -- blob: URL，next/image 管不了
                        <img src={url} alt={t('thumbAlt')} className="size-10 object-cover" />
                    ) : failed ? (
                        <ImageOff className="size-4 text-muted-foreground" />
                    ) : query.isFetching ? (
                        <Loader2 className="size-4 animate-spin text-muted-foreground" />
                    ) : (
                        <ImageIcon className="size-4 text-muted-foreground" />
                    )}
                </span>
                <span className="flex min-w-0 flex-col leading-tight">
                    <span className="truncate font-mono text-[11px] text-foreground">{mime} · {size}</span>
                    <span className="truncate font-mono text-[10px] text-muted-foreground">
                        {statusText ?? `id ${shortLogImageHash(hash)}`}
                    </span>
                </span>
                {url ? (
                    <button
                        type="button"
                        onClick={(e) => { e.stopPropagation(); setOpen(true); }}
                        className="ml-1 inline-flex shrink-0 items-center gap-1 rounded-md bg-primary px-2 py-[3px] text-[11px] font-medium text-primary-foreground hover:bg-primary/90"
                    >
                        <Search className="size-3" />
                        {t('viewOriginal')}
                    </button>
                ) : null}
            </span>
            {open && url ? (
                <LogImageLightbox placeholder={placeholder} url={url} onClose={() => setOpen(false)} />
            ) : null}
        </>
    );
}

function isNotFound(error: unknown): boolean {
    return !!error && typeof error === 'object' && 'code' in error && (error as { code?: number }).code === 404;
}

function useObjectURL(blob: Blob | undefined): string | null {
    const url = useMemo(() => (blob ? URL.createObjectURL(blob) : null), [blob]);
    useEffect(() => () => {
        if (url) URL.revokeObjectURL(url);
    }, [url]);
    return url;
}

function LogImageLightbox({ placeholder, url, onClose }: {
    placeholder: SavedImagePlaceholder;
    url: string;
    onClose: () => void;
}) {
    const t = useTranslations('log.card.logImage');
    const [, copyToClipboard] = useCopyToClipboard();
    const [dims, setDims] = useState<string | null>(null);

    // 捕获阶段先拦下按键：Esc 只关灯箱、不关外层日志详情；左右键不翻日志。
    useEffect(() => {
        const onKey = (e: KeyboardEvent) => {
            if (e.key === 'Escape' || e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
                e.stopImmediatePropagation();
                e.preventDefault();
                if (e.key === 'Escape') onClose();
            }
        };
        window.addEventListener('keydown', onKey, true);
        return () => window.removeEventListener('keydown', onKey, true);
    }, [onClose]);

    const copyId = useCallback(async () => {
        try {
            await copyToClipboard(placeholder.hash);
            toast.success(t('copied'));
        } catch (err) {
            toast.error(t('copyFailed'), { description: err instanceof Error ? err.message : String(err) });
        }
    }, [copyToClipboard, placeholder.hash, t]);

    if (typeof document === 'undefined') return null;

    return createPortal(
        <div
            // 外层日志详情弹窗点「外面」就会关；灯箱挂在 body 上，要告诉它这不算外面。
            data-morphing-dialog-ignore-outside=""
            role="dialog"
            aria-modal="true"
            aria-label={t('lightboxTitle')}
            className="fixed inset-0 z-[200] flex flex-col items-center justify-center bg-black/80 p-4 backdrop-blur-[2px]"
            onClick={(e) => { if (e.target === e.currentTarget) onClose(); }}
        >
            <div className="absolute left-5 top-5 text-[13px] text-white/80">{t('lightboxTitle')} · {t('escHint')}</div>
            <button
                type="button"
                onClick={onClose}
                aria-label={t('close')}
                className="absolute right-5 top-4 grid size-9 place-items-center rounded-full bg-white/15 text-white hover:bg-white/25"
            >
                <X className="size-4" />
            </button>
            <div className="rounded-xl bg-neutral-900 p-2 shadow-2xl">
                {/* eslint-disable-next-line @next/next/no-img-element -- blob: URL */}
                <img
                    src={url}
                    alt={t('originalAlt')}
                    className="block max-h-[75dvh] max-w-[min(90vw,1200px)] rounded-md object-contain"
                    onLoad={(e) => setDims(`${e.currentTarget.naturalWidth}×${e.currentTarget.naturalHeight}`)}
                />
            </div>
            <div className="mt-4 flex flex-wrap items-center justify-center gap-3 text-xs text-white/85">
                <span>{placeholder.mime} · {placeholder.size}{dims ? ` · ${dims}` : ''}</span>
                <code className="font-mono text-white/60" title={placeholder.hash}>sha256 {shortLogImageHash(placeholder.hash)}</code>
                <button
                    type="button"
                    onClick={copyId}
                    className="inline-flex items-center gap-1.5 rounded-lg border border-white/25 bg-white/10 px-3 py-1.5 text-white hover:bg-white/20"
                >
                    <Copy className="size-3.5" />
                    {t('copyId')}
                </button>
                <a
                    href={url}
                    download={logImageFileName(placeholder)}
                    className="inline-flex items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-primary-foreground hover:bg-primary/90"
                >
                    <Download className="size-3.5" />
                    {t('download')}
                </a>
            </div>
        </div>,
        document.body,
    );
}
