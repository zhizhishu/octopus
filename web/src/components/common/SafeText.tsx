import * as React from 'react';

import { cn } from '@/lib/utils';

type SafeTextProps = React.HTMLAttributes<HTMLSpanElement> & {
    value?: React.ReactNode;
    mode?: 'truncate' | 'wrap';
    sanitizeHTML?: boolean;
};

function titleFrom(value: React.ReactNode, title?: string) {
    if (title !== undefined) return title;
    if (typeof value === 'string' || typeof value === 'number') return String(value);
    return undefined;
}

function summarizeHTML(value: string): string | undefined {
    if (!/<\/?[a-z][\s\S]*>/i.test(value)) return undefined;
    const title = value.match(/<title[^>]*>([\s\S]*?)<\/title>/i)?.[1]
        || value.match(/<h1[^>]*>([\s\S]*?)<\/h1>/i)?.[1];
    const server = [...value.matchAll(/<center[^>]*>([\s\S]*?)<\/center>/gi)]
        .map((match) => match[1]?.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim())
        .filter(Boolean)
        .at(-1);
    const cleanTitle = title?.replace(/<[^>]+>/g, ' ').replace(/\s+/g, ' ').trim();
    if (cleanTitle && server && server !== cleanTitle) return `${cleanTitle} (${server})`;
    return cleanTitle || 'HTML error page returned by upstream proxy';
}

function displayValue(value: React.ReactNode, sanitizeHTML?: boolean) {
    if (!sanitizeHTML || typeof value !== 'string') return value;
    return summarizeHTML(value) ?? value;
}

export function SafeText({
    value,
    children,
    className,
    title,
    mode = 'truncate',
    sanitizeHTML = false,
    ...props
}: SafeTextProps) {
    const content = value ?? children;
    const safeContent = displayValue(content, sanitizeHTML);

    return (
        <span
            className={cn(
                'inline-block min-w-0 max-w-full align-bottom',
                mode === 'truncate' ? 'truncate' : 'whitespace-pre-wrap break-words [overflow-wrap:anywhere]',
                className
            )}
            title={titleFrom(safeContent, title)}
            {...props}
        >
            {safeContent}
        </span>
    );
}

export function MonoSafeText({ className, ...props }: SafeTextProps) {
    return (
        <SafeText
            className={cn('font-mono tabular-nums', className)}
            {...props}
        />
    );
}

export function ErrorSafeText({ className, ...props }: SafeTextProps) {
    return (
        <SafeText
            mode="wrap"
            className={cn('leading-relaxed text-destructive', className)}
            sanitizeHTML
            {...props}
        />
    );
}
