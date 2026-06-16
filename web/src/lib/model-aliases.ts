export function isStreamRequiredModel(model: string) {
    const value = model.trim().toLowerCase();
    if (!value) return false;
    return /^gpt-5\.5(?:$|[-_:])/i.test(value)
        || value === 'opus[1m]'
        || value.endsWith('[1m]')
        || value.includes('context-1m');
}

export function cleanOneMillionModelName(model: string) {
    const trimmed = model.trim();
    const value = trimmed.toLowerCase();
    if (!value) return '';
    if (value === 'opus[1m]' || value === 'claude-opus-4-8[1m]' || value === 'claude-opus-4.8[1m]' || value === 'claude-opus-4-7[1m]' || value === 'claude-opus-4.7[1m]') {
        return 'claude-opus-4-8';
    }
    if (value === 'fable[1m]' || value === 'claude-fable-5[1m]') {
        return 'claude-fable-5';
    }
    return trimmed.replace(/\[1m\]$/i, '');
}

export function expandOneMillionModelAliases(models: string[]) {
    const result: string[] = [];
    const seen = new Set<string>();
    const add = (model: string) => {
        const trimmed = cleanOneMillionModelName(model);
        if (!trimmed) return;
        const key = trimmed.toLowerCase();
        if (seen.has(key)) return;
        seen.add(key);
        result.push(trimmed);
    };

    for (const model of models) {
        add(model);
    }

    return result;
}
