import type {
    AuditJob,
    AuditPlanResponse,
    AuditProbeId,
    CreativeItem,
    HlwyBaselineEntry,
} from '@/api/endpoints/model-audit';

/**
 * 检测页 v6 用到的纯函数：估请求数、自动挑基准、任务卡片的小方块、鹈鹕图的隔离框。
 *
 * 全是纯函数，方便单测；页面只负责把结果摆出来。
 */

/* ------------------------------------------------------------------ 查多细 */

export type AuditPreset = 'quick' | 'full' | 'custom';

/** 每一项检查在任务卡片和自定义列表里的短名字（大白话）。 */
export const PROBE_SHORT_LABEL: Record<string, string> = {
    liveness: '连通',
    echo_rewrite: '回复原样',
    context_canary: '长内容',
    token_delta: 'Token 对比',
    fingerprint: '取数指纹',
    identity: '自报身份',
    system_prompt: '系统提示',
    glitch: '怪词反应',
    signature: 'Claude 签名',
    tokenizer_id: '分词器',
    tokenizer_baseline: '录正品基线',
    hlwy: '数字习惯',
};

export function probeShortLabel(id: string): string {
    return PROBE_SHORT_LABEL[id] ?? id;
}

/** 自定义里「自报身份 / 系统提示 / 怪词」三项合成一个勾。 */
export const IDENTITY_GROUP: readonly AuditProbeId[] = ['identity', 'system_prompt', 'glitch'];

/**
 * 按真实计划算一组检查对一个模型要发几次请求。
 *
 * 计划没读到时每项按 1 次估（宁可说少一点也不编），不适用的项按 0。
 */
export function requestsFor(plan: AuditPlanResponse | undefined | null, ids: readonly string[]): number {
    if (!plan?.probes?.length) return ids.length;
    const byId = new Map(plan.probes.map((row) => [row.id as string, row]));
    return ids.reduce((sum, id) => {
        const row = byId.get(id);
        if (!row) return sum + 1;
        return sum + (row.applicable === false ? 0 : row.requests);
    }, 0);
}

/** 一组模型合起来要发几次：每个模型用它自己的计划，不拿第一个模型乘个数。 */
export function totalRequests(
    models: readonly string[],
    planOf: (model: string) => AuditPlanResponse | undefined | null,
    idsOf: (model: string) => readonly string[],
): number {
    return models.reduce((sum, model) => sum + requestsFor(planOf(model), idsOf(model)), 0);
}

/**
 * 按「这一组模型要跑哪些检查」分组：一次入队只能带一套检查，Claude 和非 Claude
 * 混在一起时签名那项不一样，就分成两个任务提交。顺序保持用户选的顺序。
 */
export function groupModelsByProbes(
    models: readonly string[],
    idsOf: (model: string) => readonly AuditProbeId[],
): { models: string[]; probes: AuditProbeId[] }[] {
    const groups = new Map<string, { models: string[]; probes: AuditProbeId[] }>();
    for (const model of models) {
        const probes = [...idsOf(model)];
        if (probes.length === 0) continue;
        const key = probes.join(',');
        const group = groups.get(key);
        if (group) group.models.push(model);
        else groups.set(key, { models: [model], probes });
    }
    return [...groups.values()];
}

/* ------------------------------------------------------------------ 对比基准 */

function sameModel(a: string | undefined, b: string): boolean {
    const x = (a ?? '').trim().toLowerCase();
    const y = b.trim().toLowerCase();
    return x !== '' && x === y;
}

/**
 * 「自动」挑对比基准：同模型优先用自己录的（local_capture），没有就用同模型的
 * hlwy 内置；再没有就用第一份能用的 hlwy 内置（对照报告里照样会列出它和所有
 * 候选的相似度）。校验没过的件一律不选。一份都没有返回 null。
 */
export function autoBaselineFor(model: string, baselines: readonly HlwyBaselineEntry[]): HlwyBaselineEntry | null {
    const usable = baselines.filter((entry) => entry.unavailable !== true);
    return (
        usable.find((entry) => entry.source_kind === 'local_capture' && sameModel(entry.declared_model, model)) ??
        usable.find((entry) => entry.source_kind === 'builtin_hlwy' && sameModel(entry.declared_model, model)) ??
        usable.find((entry) => entry.source_kind === 'builtin_hlwy') ??
        null
    );
}

/* ------------------------------------------------------------------ 任务卡片 */

export type BlockState = 'done' | 'running' | 'waiting' | 'failed';

export type TaskBlock = {
    id: string;
    label: string;
    state: BlockState;
    /** 方块第二行：✓ 3/3、采样 60/100、等待… */
    detail: string;
};

export function isActiveJob(job: Pick<AuditJob, 'status'>): boolean {
    return job.status === 'queued' || job.status === 'running';
}

/** 进行中的任务按队列顺序排（worker 先跑 id 小的），算出「排第几」。 */
export function queueOrder(jobs: readonly AuditJob[]): { job: AuditJob; position: number }[] {
    const active = jobs.filter(isActiveJob).sort((a, b) => a.id - b.id);
    let waiting = 0;
    return active.map((job) => {
        if (job.status === 'running') return { job, position: 0 };
        waiting += 1;
        return { job, position: waiting };
    });
}

/** 任务卡片上的小方块。数字只来自后端（实时进度或采样账目），读不到就写「等待」。 */
export function taskBlocks(job: AuditJob): TaskBlock[] {
    if (job.kind === 'hlwy_compare') {
        const counts = job.hlwy_summary?.counts;
        const planned = counts?.planned_calls ?? 0;
        const started = counts?.started_slots ?? 0;
        const running = job.status === 'running' && planned > 0;
        return [
            {
                id: 'hlwy',
                label: '数字习惯',
                state: job.status === 'queued' ? 'waiting' : running && started < planned ? 'running' : started >= planned && planned > 0 ? 'done' : 'waiting',
                detail: planned > 0 ? `采样 ${started} / ${planned}` : '等待',
            },
        ];
    }
    const live = job.live;
    if (live?.probes?.length) {
        return live.probes.map((probe) => {
            const label = probeShortLabel(probe.id);
            switch (probe.status) {
                case 'done':
                    return { id: probe.id, label, state: 'done', detail: probe.planned === 0 ? '不适用' : `✓ ${probe.sent}/${probe.planned}` };
                case 'failed':
                    return { id: probe.id, label, state: 'failed', detail: probe.planned === 0 ? '不适用' : '没跑成' };
                case 'running':
                    return { id: probe.id, label, state: 'running', detail: `进行中 ${probe.sent}/${Math.max(probe.planned, probe.sent)}` };
                default:
                    return { id: probe.id, label, state: 'waiting', detail: '等待' };
            }
        });
    }
    const ids = (job.probes ?? '').split(',').map((id) => id.trim()).filter(Boolean);
    return ids.map((id) => ({ id, label: probeShortLabel(id), state: 'waiting' as const, detail: '等待' }));
}

/** 记录表里「有问题」：可疑或没测成。 */
export function jobHasProblem(job: AuditJob): boolean {
    if (job.kind === 'hlwy_compare') return job.hlwy_summary?.execution === 'failed';
    const counts = job.verdict_counts ?? {};
    return (
        job.single_verdict === 'medium' ||
        job.single_verdict === 'high' ||
        job.single_status === 'failed' ||
        (counts.medium ?? 0) + (counts.high ?? 0) + (counts.failed ?? 0) > 0
    );
}

/** 记录表里的「花了多少次」：多探针看后端账，对照看采样账目。 */
export function jobRequestsSpent(job: AuditJob): number | null {
    if (job.kind === 'hlwy_compare') return job.hlwy_summary?.counts?.started_slots ?? null;
    return typeof job.sent_requests === 'number' ? job.sent_requests : null;
}

/* ------------------------------------------------------------------ 鹈鹕测试 */

export const PELICAN_PROMPT = 'Generate an SVG of a pelican riding a bicycle';

/**
 * 鹈鹕图放进隔离框的 srcdoc。
 *
 * 安全靠两层：iframe 只给 `sandbox="allow-scripts"`（**不给** allow-same-origin，
 * 框里的脚本拿不到本站的 cookie、存储和接口），再加一条只允许内联资源的 CSP，
 * 框里的东西连不了外网、也提交不了表单。SVG 包一层最小页面居中铺满。
 */
export const CREATIVE_FRAME_CSP =
    "default-src 'none'; img-src data: blob:; style-src 'unsafe-inline'; script-src 'unsafe-inline'; font-src data:; media-src data: blob:";

export function creativeSrcDoc(item: Pick<CreativeItem, 'format' | 'content'>): string {
    const csp = `<meta http-equiv="Content-Security-Policy" content="${CREATIVE_FRAME_CSP}">`;
    if (item.format === 'html') {
        const content = item.content ?? '';
        // 已经有 <head> 的整页：把 CSP 塞到 head 最前面；没有就包一层。
        if (/<head[\s>]/i.test(content)) return content.replace(/<head([^>]*)>/i, `<head$1>${csp}`);
        return `<!doctype html><html><head>${csp}<meta charset="utf-8"></head><body>${content}</body></html>`;
    }
    const content = item.content ?? '';
    // 有 viewBox 才让它铺满：preserveAspectRatio 默认等比居中，不会变形。
    // 没有 viewBox 的 SVG 强设 100% 宽高只会被拉伸，只给上限、按原尺寸居中。
    const svgSize = /<svg[^>]*\bviewBox\s*=/i.test(content)
        ? 'svg{max-width:100%;max-height:100%;width:100%;height:100%}'
        : 'svg{max-width:100%;max-height:100%}';
    return (
        `<!doctype html><html><head>${csp}<meta charset="utf-8">` +
        '<style>html,body{margin:0;height:100%;background:#fff}body{display:flex;align-items:center;justify-content:center}' +
        svgSize + '</style></head>' +
        `<body>${content}</body></html>`
    );
}

/** 卡片底部的「18 秒 · 1,920 token」。 */
export function creativeMeta(item: Pick<CreativeItem, 'duration_ms' | 'output_tokens' | 'input_tokens'>): string {
    const parts: string[] = [];
    if (item.duration_ms > 0) parts.push(`${Math.max(1, Math.round(item.duration_ms / 1000))} 秒`);
    const tokens = (item.output_tokens ?? 0) + (item.input_tokens ?? 0);
    if (tokens > 0) parts.push(`${tokens.toLocaleString('en-US')} token`);
    return parts.join(' · ');
}

/** 没出成图时卡片上那句话。失败原因优先用后端给的 item.error（真因），没给才回落通用文案。 */
export function creativeFailureText(item: Pick<CreativeItem, 'status' | 'error' | 'kind'>): string {
    switch (item.status) {
        case 'pending':
            return '排队中…';
        case 'running':
            return '正在出图…';
        case 'timeout':
            return item.error || '超时了';
        case 'invalid':
            // 后端会说清楚为什么（回复是空的 / 没返回合法的 SVG/HTML / 图太大…），照实显示。
            return item.error || (item.kind === 'html' ? '没返回能用的 HTML' : '没返回合法 SVG');
        case 'failed':
            return item.error ? `没出成：${item.error}` : '没出成';
        default:
            return '';
    }
}

/* ---------------------------- SVG 自动存 PNG ---------------------------- */

/** 这张图有没有自动存的 PNG（列表只带 png_size，字节另取）。 */
export function creativeHasPNG(item: Pick<CreativeItem, 'png_size'>): boolean {
    return (item.png_size ?? 0) > 0;
}

/** 要不要给这张图自动截 PNG：出完、是 SVG、有内容、还没有 PNG。 */
export function creativePNGCandidate(item: Pick<CreativeItem, 'status' | 'format' | 'content' | 'png_size'>): boolean {
    return item.status === 'done' && item.format === 'svg' && item.content !== '' && !creativeHasPNG(item);
}

/** 下载时用的文件名，与后端 Content-Disposition 里的名字一致。 */
export function creativePNGFileName(item: Pick<CreativeItem, 'id'>): string {
    return `creative-${item.id}.png`;
}

/**
 * 从 SVG 里读出「这张图本来的尺寸」：优先 <svg> 的 width/height 属性（px 数字），
 * 退回 viewBox 的宽高。都读不出返回 null（栅格化时退回默认尺寸）。
 * 百分比 / em / 负数 / 0 一律不算数。
 */
export function creativeSVGIntrinsicSize(content: string): { width: number; height: number } | null {
    const open = /<svg\b[^>]*>/i.exec(content);
    if (!open) return null;
    const attrs = open[0];
    const px = (name: string): number | null => {
        const m = new RegExp(`(?:^|\\s)${name}\\s*=\\s*["']([^"']*)["']`, 'i').exec(attrs);
        const raw = (m?.[1] ?? '').trim().replace(/px$/i, '');
        if (!/^\d+(\.\d+)?$/.test(raw)) return null;
        const n = Number(raw);
        return n > 0 ? n : null;
    };
    const w = px('width');
    const h = px('height');
    if (w !== null && h !== null) return { width: w, height: h };
    const vb = /(?:^|\s)viewBox\s*=\s*["']([^"']*)["']/i.exec(attrs);
    if (vb) {
        const parts = vb[1].trim().split(/[\s,]+/).map(Number);
        if (parts.length === 4 && parts.every((n) => Number.isFinite(n)) && parts[2] > 0 && parts[3] > 0) {
            return { width: parts[2], height: parts[3] };
        }
    }
    // 只有一边时按 1:1 补另一边（浏览器对缺尺寸的 SVG 也这么猜）。
    if (w !== null) return { width: w, height: w };
    if (h !== null) return { width: h, height: h };
    return null;
}

/**
 * 栅格化目标尺寸：长边夹在 [512, 2048]，短边按比例；读不出尺寸时用 1024×768。
 * 下限避免小图存出缩略图，上限避免超大 SVG 存出几十 MB 的 PNG。
 */
export function creativePNGRasterSize(intrinsic: { width: number; height: number } | null): { width: number; height: number } {
    let width = intrinsic?.width ?? 0;
    let height = intrinsic?.height ?? 0;
    if (!(width > 0) || !(height > 0)) {
        width = 1024;
        height = 768;
    }
    const long = Math.max(width, height);
    const scale = long < 512 ? 512 / long : long > 2048 ? 2048 / long : 1;
    return { width: Math.max(1, Math.round(width * scale)), height: Math.max(1, Math.round(height * scale)) };
}
