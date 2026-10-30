import type { ReactNode } from 'react';
import { useTranslations } from 'next-intl';
import { Ban } from 'lucide-react';
import { LogImageChip } from './LogImageChip';
import {
    omitReasonKey,
    parseLogMediaPlaceholder,
    splitSavedImagePlaceholders,
    type OmittedMediaPlaceholder,
} from './log-image';

/** 没存下来的图片 / 音频 / 文件：灰色虚线小标签，写明原因，不可点。 */
export function LogMediaOmittedBadge({ placeholder }: { placeholder: OmittedMediaPlaceholder }) {
    const t = useTranslations('log.card.logImage');
    const what = placeholder.media === 'image' ? t(omitReasonKey(placeholder.reason)) : t('omitMedia');
    return (
        <span
            title={placeholder.raw}
            className="inline-flex items-center gap-1.5 rounded-md border border-dashed border-border bg-muted/60 px-2 py-0.5 align-middle font-sans text-[11px] text-muted-foreground"
        >
            <Ban className="size-3 shrink-0" />
            <span>{what}</span>
            <span className="font-mono text-[10px]">({placeholder.mime} · {placeholder.size})</span>
        </span>
    );
}

/**
 * 给 JsonView.String 的 render：值恰好是占位符时换成卡片 / 标签，其余返回 undefined
 * 走 JsonView 默认渲染。
 */
export function renderLogImageJsonString(
    _props: unknown,
    result: { type: 'type' | 'value'; value?: unknown },
): ReactNode | undefined {
    const placeholder = parseLogMediaPlaceholder(result.value);
    if (!placeholder) return undefined;
    if (result.type === 'type') return <span />;
    if (placeholder.kind === 'saved') return <LogImageChip placeholder={placeholder} />;
    return <LogMediaOmittedBadge placeholder={placeholder} />;
}

/** 纯文本视图：占位符换成卡片，其余文字原样输出（放在 <pre> 里）。 */
export function LogImagePlaceholderText({ text }: { text: string }) {
    const segments = splitSavedImagePlaceholders(text);
    return (
        <>
            {segments.map((seg, i) => seg.type === 'text'
                ? <span key={i}>{seg.text}</span>
                : <LogImageChip key={i} placeholder={seg.placeholder} />)}
        </>
    );
}
