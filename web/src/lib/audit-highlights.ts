/**
 * 检测报告第二期的两组「人话」数据：要点行（highlights）与渠道来源（provenance）。
 *
 * 纯逻辑（不引 React、不引 api 类型，只复用同样纯的 passive-verdict 排序）：组件和
 * node 测试共用同一份读法，否则迟早出现「卡片说一套、要点行说一套」。入参只读结论
 * 用得到的那几个字段，所以后端 ViewReport 能直接传进来，测试也不用造一整份报告。
 *
 * 🔴 两条硬约束：
 *   1. 百分比只透传后端给的 percent，前端绝不自己算——算出来的数字就是编的；
 *   2. 线索只能提示「疑似」，不能证明是官方正品。判定在后台给，这里只做收敛，
 *      不做任何「验真」推断。
 */

import { sortPassiveHints } from './passive-verdict';

export type AuditHighlightTone = 'ok' | 'warn' | 'info';

/** 后端 highlights[] 的元素（字段全是可选的：旧报告/后端演进都不能把页面打崩）。 */
export type AuditHighlightInput = {
    kind?: string;
    text?: string;
    tone?: string;
    /** 后端算好的百分比。前端只展示，不参与任何计算。 */
    percent?: number;
};

/** 渲染用的一行要点。 */
export type AuditHighlightRow = {
    /** React key：同 kind 可能重复，配上原始下标保证稳定。 */
    key: string;
    text: string;
    tone: AuditHighlightTone;
    /** 只有后端给了 percent 才有；没有就是 undefined（界面据此不渲染数字）。 */
    percentText?: string;
};

export type AuditHighlightsInput = {
    highlights?: readonly (AuditHighlightInput | null)[] | null;
};

export type ProvenanceStrength = 'strong' | 'medium' | 'weak';

/** 后端 provenance.hints[] 的元素。 */
export type AuditProvenanceHintInput = {
    platform?: string;
    strength?: string;
    code?: string;
    evidence?: string;
};

export type AuditProvenanceInput = {
    provenance?: {
        hints?: readonly (AuditProvenanceHintInput | null)[] | null;
        header_families?: readonly (string | null)[] | null;
        observed?: number;
    } | null;
};

/** 一条渠道来源线索（排序后、字段已收窄）。 */
export type AuditProvenanceHint = {
    platform: string;
    strength: ProvenanceStrength;
    /** 机器码，翻译映射见 passive-verdict 的 PASSIVE_REASON_LABEL_KEY。 */
    code: string;
    evidence?: string;
};

/**
 * 渠道来源卡的判定结果：
 *   suspicious = 有 strong/medium 线索（weak 只列出，不够格说「疑似」）；
 *   observed   = 看过多少份回复（>0 才出这张卡，见 provenanceView）。
 */
export type AuditProvenanceView = {
    suspicious: boolean;
    hints: AuditProvenanceHint[];
    headerFamilies: string[];
    observed: number;
};

/** 要点行左边那个小圆点。warn 琥珀 / ok 绿 / info 灰，和卡片里的写法保持一致。 */
export const TONE_DOT_CLASS: Record<AuditHighlightTone, string> = {
    ok: 'bg-emerald-500',
    warn: 'bg-amber-500',
    info: 'bg-muted-foreground/60',
};

/** 未知 tone 一律当 info：宁可少喊一嗓子，也不许把没标色的行渲染成警告。 */
function normalizeTone(tone: string | null | undefined): AuditHighlightTone {
    if (tone === 'ok' || tone === 'warn') return tone;
    return 'info';
}

/** 强度未知一律当 weak：不认识的东西不许触发「疑似」。 */
function normalizeStrength(strength: string | null | undefined): ProvenanceStrength {
    if (strength === 'strong' || strength === 'medium') return strength;
    return 'weak';
}

/** 只透传后端给的百分比，绝不自己算。 */
function percentText(percent: number | null | undefined): string | undefined {
    if (typeof percent !== 'number' || !Number.isFinite(percent)) return undefined;
    return `${percent}%`;
}

/**
 * 读要点行。没有 highlights（旧报告 / 后端没算）→ 空数组，界面据此整块不渲染。
 * 空文字的条目直接丢掉：渲染一个只有圆点的空行比不渲染更糟。
 */
export function highlightRows(report: AuditHighlightsInput | null | undefined): AuditHighlightRow[] {
    const raw = report?.highlights;
    if (!Array.isArray(raw)) return [];

    const rows: AuditHighlightRow[] = [];
    raw.forEach((item, index) => {
        if (!item || typeof item !== 'object') return;
        const text = typeof item.text === 'string' ? item.text.trim() : '';
        if (!text) return;
        const row: AuditHighlightRow = {
            key: `${item.kind || 'highlight'}-${index}`,
            text,
            tone: normalizeTone(item.tone),
        };
        const pct = percentText(item.percent);
        if (pct) row.percentText = pct;
        rows.push(row);
    });
    return rows;
}

/** 线索收窄 + 按强度排序（strong→medium→weak，同强度保持后端给的顺序）。 */
function readHints(raw: readonly (AuditProvenanceHintInput | null)[] | null | undefined): AuditProvenanceHint[] {
    if (!Array.isArray(raw)) return [];
    const hints: AuditProvenanceHint[] = [];
    for (const item of raw) {
        if (!item || typeof item !== 'object') continue;
        // 没有机器码就没法翻译成一句人话，列出来只会是空白。
        const code = typeof item.code === 'string' ? item.code.trim() : '';
        if (!code) continue;
        const hint: AuditProvenanceHint = {
            platform: typeof item.platform === 'string' ? item.platform.trim() : '',
            strength: normalizeStrength(item.strength),
            code,
        };
        if (typeof item.evidence === 'string' && item.evidence.trim()) hint.evidence = item.evidence.trim();
        hints.push(hint);
    }
    // 复用被动徽章的排序，两处的强度顺序不会各说各话。
    return sortPassiveHints(hints);
}

/**
 * 读渠道来源。provenance 缺失或 observed 不是正数（后端没看够样本 / 旧报告）→ null，
 * 界面据此不渲染这张卡：只看了一份回复就断言「没发现线索」是误导。
 */
export function provenanceView(report: AuditProvenanceInput | null | undefined): AuditProvenanceView | null {
    const provenance = report?.provenance;
    if (!provenance) return null;

    const observed = typeof provenance.observed === 'number' && Number.isFinite(provenance.observed)
        ? provenance.observed
        : 0;
    if (!(observed > 0)) return null;

    const hints = readHints(provenance.hints);
    const rawFamilies = provenance.header_families;
    const headerFamilies = Array.isArray(rawFamilies)
        ? rawFamilies
            .filter((item): item is string => typeof item === 'string')
            .map((item) => item.trim())
            .filter((item) => item.length > 0)
        : [];

    const suspicious = hints.some((hint) => hint.strength === 'strong' || hint.strength === 'medium');
    return { suspicious, hints, headerFamilies, observed };
}
