import type { LLMInfo } from '@/api/endpoints/model';

/** String form of every editable price field, as held by the edit dialog. */
export type PriceEditValues = {
    input: string;
    output: string;
    cache_read: string;
    cache_write: string;
    per_call: string;
};

export function toPriceEditValues(model: LLMInfo): PriceEditValues {
    return {
        input: model.input.toString(),
        output: model.output.toString(),
        cache_read: model.cache_read.toString(),
        cache_write: model.cache_write.toString(),
        per_call: (model.per_call ?? 0).toString(),
    };
}

/**
 * Build the /model/update body. Always carries per_call: the backend merges
 * onto the stored row, but sending it keeps the dialog the source of truth and
 * lets an admin clear it.
 */
export function buildModelUpdatePayload(name: string, values: PriceEditValues): LLMInfo {
    const num = (value: string) => parseFloat(value) || 0;
    return {
        name,
        input: num(values.input),
        output: num(values.output),
        cache_read: num(values.cache_read),
        cache_write: num(values.cache_write),
        per_call: num(values.per_call),
    };
}
