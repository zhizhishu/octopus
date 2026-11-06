'use client';

import { TriangleAlert } from 'lucide-react';
import { useTranslations } from 'next-intl';
import { cn } from '@/lib/utils';
import { PASSIVE_REASON_LABEL_KEY, PASSIVE_REASON_UNKNOWN_KEY } from '@/lib/passive-verdict';
import type { AuditProvenanceView, ProvenanceStrength } from '@/lib/audit-highlights';

/**
 * 渠道来源卡：把「这份回复像谁家」的线索摊开给人看，并说清边界。
 *
 * 三条硬约定，照抄后端与第一期被动徽标的措辞，一个字都不放宽：
 *   1. 线索码的文案不在这里另写一套——一律复用 PASSIVE_REASON_LABEL_KEY，
 *      在 log.card 命名空间里翻译，避免同一条线索在日志页和检测页说法不一；
 *   2. weak 只列出，不参与「疑似」那一句；
 *   3. 结尾必须保留「只能发现线索，不能证明是官方正品」。
 */

const STRENGTH_LABEL: Record<ProvenanceStrength, string> = {
    strong: '强',
    medium: '中',
    weak: '弱',
};

const STRENGTH_CLASS: Record<ProvenanceStrength, string> = {
    strong: 'bg-amber-500/15 text-amber-800 dark:text-amber-300',
    medium: 'bg-amber-500/10 text-amber-700 dark:text-amber-300',
    weak: 'bg-muted text-muted-foreground',
};

export function ProvenanceCard({ view }: { view: AuditProvenanceView }) {
    const t = useTranslations('log.card');
    // 翻译 key 一律走第一期的映射表；认不出的码落到「有异常线索」，绝不显示空白。
    const labelOf = (code: string) => t(PASSIVE_REASON_LABEL_KEY[code] || PASSIVE_REASON_UNKNOWN_KEY);

    // 「疑似 X」只取第一条够格的线索（strong 在前，见 provenanceView 的排序）。
    const lead = view.hints.find((hint) => hint.strength === 'strong' || hint.strength === 'medium');

    return (
        <div data-provenance-card className="overflow-hidden rounded-2xl border border-border bg-card">
            <div className="px-4 pt-3 pb-4">
                <p className="text-[11px] font-medium tracking-[0.16em] text-muted-foreground">渠道来源</p>

                {view.suspicious && lead ? (
                    <>
                        <p className="mt-3 flex items-start gap-1.5 text-sm font-medium text-amber-800 dark:text-amber-300">
                            <TriangleAlert className="mt-0.5 size-4 shrink-0" />
                            {/* 翻译文案本身已经带「疑似…」的措辞，这里不再重复加前缀。 */}
                            <span className="min-w-0">{labelOf(lead.code)}</span>
                        </p>
                        <ul className="mt-2 space-y-1.5">
                            {view.hints.map((hint, index) => (
                                <li
                                    key={`${hint.code}-${index}`}
                                    className="border-t border-border/60 pt-1.5 first:border-t-0 first:pt-0"
                                >
                                    <div className="flex items-start justify-between gap-2">
                                        <span className="min-w-0 text-xs">{labelOf(hint.code)}</span>
                                        <span
                                            className={cn(
                                                'shrink-0 rounded-full px-1.5 py-px text-[10px] font-medium',
                                                STRENGTH_CLASS[hint.strength],
                                            )}
                                        >
                                            {STRENGTH_LABEL[hint.strength]}
                                        </span>
                                    </div>
                                    {hint.evidence && (
                                        <p className="mt-0.5 break-all font-mono text-[11px] text-muted-foreground">
                                            {hint.evidence}
                                        </p>
                                    )}
                                </li>
                            ))}
                        </ul>
                    </>
                ) : (
                    <p className="mt-3 text-sm text-muted-foreground">
                        看了 {view.observed} 份回复，没发现转售线索
                    </p>
                )}

                {view.headerFamilies.length > 0 && (
                    <div className="mt-3 border-t border-border pt-2">
                        <p className="text-[11px] text-muted-foreground">响应头类别</p>
                        <p className="mt-1 break-all font-mono text-[11px]">{view.headerFamilies.join(' · ')}</p>
                    </div>
                )}

                <p className="mt-2 border-t border-border pt-2 text-[11px] leading-5 text-muted-foreground/80">
                    只能发现线索，不能证明是官方正品。
                </p>
            </div>
        </div>
    );
}
