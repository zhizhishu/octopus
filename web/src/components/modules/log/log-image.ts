/**
 * 「真实出站请求体」里的图片占位符。
 *
 * 后端（internal/relay/log_image_capture.go）把内联图片另存成文件，正文里只留：
 *   [image: image/png, 245KB, id=<64 位 sha256>]             ← 已保存，可看原图
 *   [image omitted: image/png, 12MB, too large]              ← 没存，后面是原因
 *   [image omitted: image/png, 12MB]                         ← 旧格式 / 不是可识别的图片
 * 这里只做纯解析，不碰 React，方便单测。
 */

export type LogImageOmitReason =
    | 'too large'
    | 'too many images'
    | 'request image limit reached'
    | 'save queue full';

export type SavedImagePlaceholder = {
    kind: 'saved';
    mime: string;
    size: string;
    hash: string;
    raw: string;
};

export type OmittedMediaPlaceholder = {
    kind: 'omitted';
    media: 'image' | 'audio' | 'video' | 'file';
    mime: string;
    size: string;
    reason: LogImageOmitReason | null;
    raw: string;
};

export type LogMediaPlaceholder = SavedImagePlaceholder | OmittedMediaPlaceholder;

const MIME = '[A-Za-z0-9.+/-]{1,64}';
const SIZE = '\\d+(?:B|KB|MB|GB)';
const HASH = '[0-9a-f]{64}';

const SAVED_RE = new RegExp(`^\\[image: (${MIME}), (${SIZE}), id=(${HASH})\\]$`);
const SAVED_GLOBAL_RE = new RegExp(`\\[image: (${MIME}), (${SIZE}), id=(${HASH})\\]`, 'g');
const OMITTED_RE = new RegExp(
    `^\\[(image|audio|video|file) omitted: (${MIME}), (${SIZE})(?:, (too large|too many images|request image limit reached|save queue full))?\\]$`,
);
const HASH_RE = new RegExp(`^${HASH}$`);

export function isLogImageHash(value: unknown): value is string {
    return typeof value === 'string' && HASH_RE.test(value);
}

/** 整个字符串恰好是一个占位符时才认；正文里顺带提到的同样文字不算（那是 JSON 树的一个值）。 */
export function parseLogMediaPlaceholder(value: unknown): LogMediaPlaceholder | null {
    if (typeof value !== 'string' || value.length > 200 || value[0] !== '[') return null;
    const saved = SAVED_RE.exec(value);
    if (saved) {
        return { kind: 'saved', mime: saved[1], size: saved[2], hash: saved[3], raw: value };
    }
    const omitted = OMITTED_RE.exec(value);
    if (omitted) {
        return {
            kind: 'omitted',
            media: omitted[1] as OmittedMediaPlaceholder['media'],
            mime: omitted[2],
            size: omitted[3],
            reason: (omitted[4] as LogImageOmitReason | undefined) ?? null,
            raw: value,
        };
    }
    return null;
}

export type LogTextSegment =
    | { type: 'text'; text: string }
    | { type: 'image'; placeholder: SavedImagePlaceholder };

/**
 * 纯文本视图（截断后不是合法 JSON 时）里把已保存图片的占位符切出来，其余文字原样。
 * 被截断在占位符中间的那一张认不出来，就当普通文字显示。
 */
export function splitSavedImagePlaceholders(text: string): LogTextSegment[] {
    if (!text || !text.includes('[image: ')) return text ? [{ type: 'text', text }] : [];
    const out: LogTextSegment[] = [];
    let last = 0;
    for (const m of text.matchAll(SAVED_GLOBAL_RE)) {
        const start = m.index ?? 0;
        if (start > last) out.push({ type: 'text', text: text.slice(last, start) });
        out.push({ type: 'image', placeholder: { kind: 'saved', mime: m[1], size: m[2], hash: m[3], raw: m[0] } });
        last = start + m[0].length;
    }
    if (last < text.length) out.push({ type: 'text', text: text.slice(last) });
    return out;
}

export function containsSavedImagePlaceholder(text: string | undefined | null): boolean {
    if (!text || !text.includes('[image: ')) return false;
    SAVED_GLOBAL_RE.lastIndex = 0;
    return SAVED_GLOBAL_RE.test(text);
}

/** 原因 → 文案 key（log.card.logImage.*）。 */
export function omitReasonKey(reason: LogImageOmitReason | null): string {
    switch (reason) {
        case 'too large':
            return 'omitTooLarge';
        case 'too many images':
            return 'omitTooMany';
        case 'request image limit reached':
            return 'omitOverBudget';
        case 'save queue full':
            return 'omitQueueFull';
        default:
            return 'omitNotSaved';
    }
}

const EXT_BY_MIME: Record<string, string> = {
    'image/png': 'png',
    'image/jpeg': 'jpg',
    'image/gif': 'gif',
    'image/webp': 'webp',
};

/** 下载原图时的文件名。 */
export function logImageFileName(p: Pick<SavedImagePlaceholder, 'hash' | 'mime'>): string {
    return `${p.hash}.${EXT_BY_MIME[p.mime] ?? 'img'}`;
}

export function shortLogImageHash(hash: string): string {
    return hash.length > 12 ? `${hash.slice(0, 12)}…` : hash;
}
