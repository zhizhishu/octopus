import type { ChannelLatestAudit } from '@/api/endpoints/model-audit';
import type { ChipKind } from '@/lib/model-audit-verdict';

export type ModelAuditChip = {
    model: string;
    chip: ChipKind;
    chipLabel: string;
    time?: string;
    headline?: string;
    summary?: string;
    points: string[];
    resultId?: number;
    jobId?: number;
};

export function mergeModelsWithLatest(
    models: string[],
    latest: ChannelLatestAudit[] | undefined,
): ModelAuditChip[] {
    const byModel = new Map<string, ChannelLatestAudit>();
    for (const row of latest ?? []) {
        if (!byModel.has(row.model)) byModel.set(row.model, row);
    }
    return models.map((model) => {
        const row = byModel.get(model);
        if (!row) {
            return {
                model,
                chip: 'none' as ChipKind,
                chipLabel: '没检测过',
                points: ['这个模型还没检测过'],
            };
        }
        return {
            model,
            chip: (row.chip as ChipKind) || 'none',
            chipLabel: row.chip_label || '没检测过',
            time: row.finished_at,
            headline: row.headline,
            summary: row.summary,
            points: row.points?.length ? row.points : row.error_msg ? [row.error_msg] : [],
            resultId: row.id,
            jobId: row.job_id,
        };
    });
}

export type ChipFilter = 'all' | 'warn' | 'fail' | 'none';

export function countChips(items: ModelAuditChip[]) {
    const c = { ok: 0, warn: 0, fail: 0, none: 0 };
    for (const item of items) c[item.chip]++;
    return c;
}
