import { useState } from 'react';
import { ChevronDown } from 'lucide-react';
import { cn } from '@/lib/utils';
import type { RelayProbeFinding } from '@/api/endpoints/log';
import { describeProbeEvidence } from './probe-evidence';

/**
 * 详情弹窗里的「硬事实校验」证据区。
 *
 * 为什么不放在日志卡片上：卡片上的叙述框是按用户要求删掉的，在那儿加一段
 * 预览文字等于把删掉的东西装回去。卡片只留徽标（扫的时候看），结论摊开放在
 * 弹窗里（查的时候看）。
 *
 * 首条默认展开、其余折成「+N」：多数日志最多命中一两条，真命中多条的时候
 * 第一条通常就是最想看的那个；一次性铺开会把下面的请求/响应面板挤走。
 */
/**
 * 置信度文案。undefined 是升级前落库的旧行——显示"未评估"，绝不许当成"高"。
 */
function confidenceLabel(confidence: RelayProbeFinding['confidence']): string {
    switch (confidence) {
        case 'high':
            return '高（数字全部同源可比）';
        case 'medium':
            return '中（存在有界的不可计算量，见下方因素）';
        case 'low':
            return '低（偏差贴着阈值，仅供参考）';
        default:
            return '未评估（旧数据）';
    }
}

export function ProbeEvidenceList({ probes }: { probes: RelayProbeFinding[] }) {
    const [expanded, setExpanded] = useState(false);
    if (probes.length === 0) return null;

    const COLLAPSED_COUNT = 2;
    const visible = expanded ? probes : probes.slice(0, COLLAPSED_COUNT);
    const hidden = probes.length - visible.length;

    return (
        <div className="min-w-0 rounded-xl border border-border bg-muted/20 px-3.5 py-2.5">
            <div className="flex items-baseline justify-between gap-2">
                <p className="text-sm font-medium text-foreground">硬事实校验</p>
                <span className="shrink-0 text-[11px] text-muted-foreground">
                    共 {probes.length} 处
                </span>
            </div>
            <p className="mt-0.5 text-[11px] leading-5 text-muted-foreground">
                全部只用这一条日志本身算出来，没有额外请求、没有消耗 token。
                命中值得去查；没命中只说明这里没发现矛盾，不代表模型是真的。
            </p>
            <ul className="mt-2 space-y-2">
                {visible.map((probe, index) => {
                    const rows = describeProbeEvidence(probe.code, probe.evidence);
                    // 醒目只给"严重且可信"的：低置信只是线索，不值得用告警色。
                    const highlight = probe.severity === 'medium' && probe.confidence !== 'low';
                    return (
                        <li
                            key={`${probe.code}-${index}`}
                            className={cn(
                                'rounded-lg border px-2.5 py-2',
                                highlight
                                    ? 'border-amber-500/40 bg-amber-500/5'
                                    : 'border-border bg-background/60'
                            )}
                        >
                            <p className="text-xs leading-5 text-foreground">{probe.title}</p>
                            <p className="mt-1 text-[11px] text-muted-foreground">
                                置信度：{confidenceLabel(probe.confidence)}
                            </p>
                            {rows.length > 0 && (
                                <div className="mt-1.5 space-y-0.5">
                                    {rows.map((row) => (
                                        <div
                                            key={row.label}
                                            className="flex flex-wrap items-baseline gap-x-1.5 text-[11px]"
                                        >
                                            <span className="shrink-0 text-muted-foreground">{row.label}</span>
                                            <span className="font-mono tabular-nums text-foreground">{row.value}</span>
                                            {row.formula && (
                                                <span className="text-muted-foreground/70">（{row.formula}）</span>
                                            )}
                                        </div>
                                    ))}
                                </div>
                            )}
                        </li>
                    );
                })}
            </ul>
            {probes.length > COLLAPSED_COUNT && (
                <button
                    type="button"
                    onClick={() => setExpanded((v) => !v)}
                    aria-expanded={expanded}
                    className="mt-2 inline-flex items-center gap-1 text-[11px] text-muted-foreground transition-colors hover:text-foreground"
                >
                    <ChevronDown className={cn('size-3 transition-transform', expanded && 'rotate-180')} />
                    {expanded ? '收起' : `展开其余 ${hidden} 处`}
                </button>
            )}
        </div>
    );
}
