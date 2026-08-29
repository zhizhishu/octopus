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

function normalizeDisplayWords(value: string) {
    return value
        .replace(/[_/]+/g, '-')
        .split('-')
        .filter(Boolean)
        .map((word) => {
            const lower = word.toLowerCase();
            if (/^\d+(?:\.\d+)?$/.test(word)) return word;
            if (['gpt', 'glm', 'api', 'tts', 'ocr', 'vl', 'vlm', 'r1', 'v3', 'v4', 'v5'].includes(lower)) return lower.toUpperCase();
            if (lower === 'claude') return 'Claude';
            if (lower === 'gemini') return 'Gemini';
            if (lower === 'deepseek') return 'DeepSeek';
            if (lower === 'qwen') return 'Qwen';
            if (lower === 'kimi') return 'Kimi';
            if (lower === 'grok') return 'Grok';
            if (lower === 'doubao') return 'Doubao';
            if (lower === 'hunyuan') return 'Hunyuan';
            if (lower === 'flash') return 'Flash';
            if (lower === 'pro') return 'Pro';
            if (lower === 'max') return 'Max';
            if (lower === 'turbo') return 'Turbo';
            if (lower === 'opus') return 'Opus';
            if (lower === 'sonnet') return 'Sonnet';
            if (lower === 'haiku') return 'Haiku';
            return lower.charAt(0).toUpperCase() + lower.slice(1);
        })
        .join(' ');
}

export function marketModelName(model: string) {
    const cleanName = cleanOneMillionModelName(model);
    const value = cleanName.toLowerCase();
    if (!value) return '';

    // 牛来（Ox Alpha）：OpenRouter 匿名马甲，真身 GLM-5.3-Flash（智谱）
    if (value.includes('ox-alpha') || value.includes('ox_alpha') || value === 'ox' || value.startsWith('niulai') || value.includes('牛来')) {
        return 'Ox Alpha';
    }

    if (value === 'opus' || value.startsWith('claude-opus')) return normalizeDisplayWords(cleanName.replace(/^claude-/, 'Claude-'));
    if (value.startsWith('claude-sonnet')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('claude-haiku')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('claude-fable')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('gpt-')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('o1') || value.startsWith('o3') || value.startsWith('o4')) return cleanName.toUpperCase();
    if (value.startsWith('gemini-')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('deepseek-')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('qwen')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('glm-') || value.startsWith('zai/glm-') || value.startsWith('z-ai/glm-')) return normalizeDisplayWords(cleanName.replace(/^(zai|z-ai)\//i, ''));
    if (value.startsWith('kimi-')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('grok-')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('doubao-')) return normalizeDisplayWords(cleanName);
    if (value.startsWith('hunyuan-')) return normalizeDisplayWords(cleanName);

    return normalizeDisplayWords(cleanName);
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
