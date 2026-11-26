import type {
    PromptRule,
    PromptRuleMatch,
    PromptRulePayload,
    PromptRulePreviewResult,
    PromptRuleScope,
    PromptRuleSegment,
} from '@/api/endpoints/prompt-rule';
import { matchesPromptSearch } from './prompt-rows';

// 提示词规则页的纯数据整理：列表筛选、叠加顺序与上移/下移、一句话总结、草稿、
// 保存前校验。命中判定与合并出来的文本一律用后端 /preview 的结果，这里不再自己算一遍
// （模型名的 1M 别名清洗等规范化只在后端做，前端自算必然对不上）。

/** 具体度层级，与后端 model.PromptRule.Tier 逐条对应：数字越大越具体，注入时排在越外侧。 */
export const PROMPT_RULE_TIER = { global: 0, plan: 1, model: 2, channel: 3 } as const;

/** 新建规则的草稿在草稿表里的 key；数字 key 都是已保存规则的 id。 */
export const NEW_RULE_KEY = '$new';

/** 规则编辑草稿：id 缺省表示还没保存过（保存走 create）。 */
export type PromptRuleDraft = Omit<PromptRule, 'id' | 'created_at' | 'updated_at'> & { id?: number };
export type PromptRuleDraftState = { value: PromptRuleDraft; baseline: PromptRuleDraft };
export type PromptRuleDraftMap = Record<string, PromptRuleDraftState>;

export function promptRuleTier(rule: PromptRule): number {
    // 与 Go Tier() 逐条对应：渠道受限最具体，其次模型，其次方案，全开 = 全局。
    if (rule.channel_mode && rule.channel_mode !== 'all') return PROMPT_RULE_TIER.channel;
    if (rule.model_mode === 'selected') return PROMPT_RULE_TIER.model;
    if (rule.plan_mode === 'selected') return PROMPT_RULE_TIER.plan;
    return PROMPT_RULE_TIER.global;
}

/** 方案作用域：all 全中；selected 只看 plan_ids，没选方案的 key（plan_id=0）由 include_no_plan 兜底。 */
export function promptRulePlanAllows(rule: PromptRule, planId: number): boolean {
    if (rule.plan_mode === 'all') return true;
    if (rule.plan_mode !== 'selected') return false;
    if (planId === 0) return rule.include_no_plan;
    return rule.plan_ids.includes(planId);
}

/** 渠道作用域：all / include(只在这些) / exclude(除了这些)。 */
export function promptRuleChannelAllows(rule: PromptRule, channelId: number): boolean {
    if (rule.channel_mode === 'all') return true;
    if (rule.channel_mode === 'include') return rule.channel_ids.includes(channelId);
    if (rule.channel_mode === 'exclude') return !rule.channel_ids.includes(channelId);
    return false;
}

/**
 * 模型作用域：pattern 精确名或尾 * 前缀，不分大小写。
 * 只用于列表筛选（「按模型找规则」），不做命中判定：后端匹配前还会做 1M 别名清洗
 * （CleanOneMillionCapabilityModelName），这里没有那份规范化，所以预览/命中一律走 /preview。
 */
export function promptRuleModelAllows(rule: PromptRule, requestModel: string): boolean {
    if (rule.model_mode === 'all') return true;
    if (rule.model_mode !== 'selected') return false;
    const request = requestModel.trim().toLowerCase();
    for (const pattern of rule.models) {
        const p = pattern.trim().toLowerCase();
        if (!p) continue;
        if (p.endsWith('*')) {
            if (request.startsWith(p.slice(0, -1))) return true;
            continue;
        }
        if (request === p) return true;
    }
    return false;
}

// ── 列表顺序 / 上移下移 ──

/**
 * 列表与真实叠加共用的顺序：层级（全局 → 方案 → 模型 → 渠道）正序，同级按 sort、再按 id。
 * 层级越靠后的规则在最终文本里越靠外，所以列表从上到下就是「先加谁」。
 */
export function promptRuleCompareOrder(a: PromptRule, b: PromptRule): number {
    const tierA = promptRuleTier(a);
    const tierB = promptRuleTier(b);
    if (tierA !== tierB) return tierA - tierB;
    if (a.sort !== b.sort) return a.sort - b.sort;
    return a.id - b.id;
}

/** 按共用顺序排好的副本（不改入参）。 */
export function promptRuleOrdered(rules: PromptRule[]): PromptRule[] {
    return [...rules].sort(promptRuleCompareOrder);
}

/**
 * 上移/下移一条规则：只在同一层（tier）的相邻规则之间换，跨层不动（跨层由范围决定，换不动）。
 * 返回 /reorder 要的**全量** id 列表（后端要求全量精确，不许只发改动项）；换不动返回 null。
 */
export function promptRuleReorderIDs(rules: PromptRule[], id: number, direction: 'up' | 'down'): number[] | null {
    const ordered = promptRuleOrdered(rules);
    const index = ordered.findIndex((rule) => rule.id === id);
    if (index < 0) return null;
    const target = direction === 'up' ? index - 1 : index + 1;
    if (target < 0 || target >= ordered.length) return null;
    if (promptRuleTier(ordered[target]) !== promptRuleTier(ordered[index])) return null;
    const swapped = [...ordered];
    [swapped[index], swapped[target]] = [swapped[target], swapped[index]];
    return swapped.map((rule) => rule.id);
}

/** 这条规则在该方向上还能不能换（按钮禁用用）。 */
export function promptRuleCanMove(rules: PromptRule[], id: number, direction: 'up' | 'down'): boolean {
    return promptRuleReorderIDs(rules, id, direction) !== null;
}

// ── 合并预览：只做展示，不改后端给的结果 ──

/** 客户端这次没带系统提示词时，段里显示的提示；它只是一句说明，永远不会拼进正文。 */
export const PROMPT_RULE_CLIENT_EMPTY_HINT = '这次请求没带系统提示词';

export type PromptRulePreviewView = {
    matches: PromptRuleMatch[];
    segments: PromptRuleSegment[];
    text: string;
    matchedCount: number;
};

/**
 * 后端 /preview 的结果原样展示：命中顺序、分段顺序、正文都不重排、不补字。
 * 前端不再自己拼一遍，避免和后端的模型名规范化、分块顺序对不上。
 */
export function promptRulePreviewView(preview: PromptRulePreviewResult): PromptRulePreviewView {
    return {
        matches: preview.matches,
        segments: preview.segments,
        text: preview.text,
        matchedCount: preview.matches.filter((row) => row.matched).length,
    };
}

/** 客户端段没有原文时，页面上给一句提示；正文里不出现这句。 */
export function promptRuleClientSegmentEmpty(segment: PromptRuleSegment): boolean {
    return segment.position === 'client' && !segment.text.trim();
}

/** 未命中/命中的大白话原因（预览页左侧灰行用）。 */
export function promptRuleReasonText(reason: PromptRuleMatch['reason'], scope: PromptRuleScope): string {
    switch (reason) {
        case 'matched':
            return '命中，会加上';
        case 'disabled':
            return '已关闭，什么都不加';
        case 'audience':
            return scope.is_admin ? '只对普通用户生效' : '只对管理员 Key 生效';
        case 'plan_scope':
            return scope.plan_id === 0 ? '只管有方案的 Key' : '方案不在它的范围里';
        case 'channel_scope':
            return scope.channel_id === 0 ? '没有候选渠道可用' : '渠道不在它的范围（或被排除）';
        case 'model_scope':
            return '模型不在它的范围里';
        default:
            return '没有命中';
    }
}

// ── 草稿 ──

export function promptRuleFromSaved(rule: PromptRuleDraft): PromptRuleDraft {
    return {
        name: rule.name,
        enabled: rule.enabled,
        text: rule.text,
        position: rule.position,
        plan_mode: rule.plan_mode,
        plan_ids: [...rule.plan_ids],
        include_no_plan: rule.include_no_plan,
        channel_mode: rule.channel_mode,
        channel_ids: [...rule.channel_ids],
        model_mode: rule.model_mode,
        models: [...rule.models],
        audience: rule.audience,
        sort: rule.sort,
        id: rule.id,
    };
}

/** 新建规则默认值：全局范围（所有方案 + 所有渠道 + 所有模型）；开关先开（没文字就不会加东西）。 */
export function blankPromptRuleDraft(sort = 0): PromptRuleDraft {
    return {
        name: '',
        enabled: true,
        text: '',
        position: 'before',
        plan_mode: 'all',
        plan_ids: [],
        include_no_plan: false,
        channel_mode: 'all',
        channel_ids: [],
        model_mode: 'all',
        models: [],
        audience: 'all',
        sort,
    };
}

function sameDraftIds(a: number[], b: number[]): boolean {
    if (a.length !== b.length) return false;
    const sortedA = [...a].sort((x, y) => x - y);
    const sortedB = [...b].sort((x, y) => x - y);
    return sortedA.every((id, i) => id === sortedB[i]);
}

/** 模型 pattern 只按 trim 后逐条比；只改大小写也算改动（保存写的是原样）。 */
function sameDraftPatterns(a: string[], b: string[]): boolean {
    const norm = (list: string[]) => list.map((p) => p.trim()).filter(Boolean);
    const x = norm(a);
    const y = norm(b);
    return x.length === y.length && x.every((p, i) => p === y[i]);
}

export function promptRuleValuesEqual(a: PromptRuleDraft, b: PromptRuleDraft): boolean {
    return a.name.trim() === b.name.trim()
        && a.enabled === b.enabled
        && a.text.trim() === b.text.trim()
        && a.position === b.position
        && a.plan_mode === b.plan_mode
        && sameDraftIds(a.plan_ids, b.plan_ids)
        && a.include_no_plan === b.include_no_plan
        && a.channel_mode === b.channel_mode
        && sameDraftIds(a.channel_ids, b.channel_ids)
        && a.model_mode === b.model_mode
        && sameDraftPatterns(a.models, b.models)
        && a.audience === b.audience;
}

/** 改动保存在草稿表里（按规则 key），折叠、切筛选、切条目都不会丢。 */
export function editRuleDraft(
    map: PromptRuleDraftMap,
    key: string,
    baseline: PromptRuleDraft,
    mutate: (value: PromptRuleDraft) => PromptRuleDraft,
): PromptRuleDraftMap {
    const fromSaved = promptRuleFromSaved(baseline);
    const existing = map[key] ?? { value: fromSaved, baseline: fromSaved };
    return { ...map, [key]: { ...existing, value: mutate(existing.value) } };
}

/** 保存受理：保存动作之外又打了字的草稿保留，只把基线推进到本次提交值。 */
export function acceptRuleDraftSave(map: PromptRuleDraftMap, key: string, submitted: PromptRuleDraft): PromptRuleDraftMap {
    const original = map[key];
    const value = original && !promptRuleValuesEqual(original.value, submitted) ? original.value : submitted;
    return { ...map, [key]: { value, baseline: submitted } };
}

export function dropRuleDraft(map: PromptRuleDraftMap, key: string): PromptRuleDraftMap {
    if (!map[key]) return map;
    const next = { ...map };
    delete next[key];
    return next;
}

/** 有未保存草稿的规则 key（保存动作之外改了任何字段）。 */
export function dirtyRuleDraftKeys(map: PromptRuleDraftMap): string[] {
    return Object.keys(map).filter((key) => !promptRuleValuesEqual(map[key].value, map[key].baseline));
}

// ── 作用域标签 / 一句话总结 ──

export type PromptRuleScopeTag = { kind: 'plan' | 'channel' | 'model' | 'audience'; label: string; title: string };

function capNames(names: string[], keep: number): string {
    if (names.length <= keep) return names.join('、');
    return `${names.slice(0, keep).join('、')} +${names.length - keep}`;
}

/** 卡片上的范围小标签：方案 / 渠道 / 模型 / 对谁，长列表截断并留完整 title。 */
export function promptRuleScopeTags(
    rule: PromptRule,
    planNames: Map<number, string>,
    channelNames: Map<number, string>,
): PromptRuleScopeTag[] {
    const tags: PromptRuleScopeTag[] = [];
    if (rule.plan_mode === 'all') {
        tags.push({ kind: 'plan', label: '所有方案', title: '所有 key（含没选方案的）都管' });
    } else {
        const names = rule.plan_ids.map((id) => planNames.get(id) ?? `方案 ${id}`);
        if (names.length) tags.push({ kind: 'plan', label: capNames(names, 3), title: `只管这些方案：${names.join('、')}` });
        if (rule.include_no_plan) tags.push({ kind: 'plan', label: '没选方案的 Key', title: '没绑定任何方案的 API key 也管' });
    }
    if (rule.channel_mode === 'all') {
        tags.push({ kind: 'channel', label: '所有渠道', title: '按最后真正转发到的渠道算，这里全部都管' });
    } else {
        const names = rule.channel_ids.map((id) => channelNames.get(id) ?? `#${id}`);
        const prefix = rule.channel_mode === 'include' ? '只 ' : '除 ';
        tags.push({ kind: 'channel', label: `${prefix}${capNames(names, 3)}`, title: `${rule.channel_mode === 'include' ? '只在这些渠道生效' : '这些渠道不生效（其余都生效）'}：${names.join('、')}` });
    }
    if (rule.model_mode === 'all') {
        tags.push({ kind: 'model', label: '所有模型', title: '所有请求模型都管' });
    } else {
        const patterns = rule.models.map((p) => p.trim()).filter(Boolean);
        const label = patterns.length ? capNames(patterns, 3) : '没选模型';
        tags.push({ kind: 'model', label, title: `模型名逐个匹配，尾 * 通配开头（如 claude-*）：${patterns.join('、')}` });
    }
    tags.push(
        rule.audience === 'admin_only'
            ? { kind: 'audience', label: '仅管理员 Key', title: '只对管理员 API Key 发起的请求生效' }
            : { kind: 'audience', label: '所有用户', title: '普通用户和管理员 Key 的请求都生效' },
    );
    return tags;
}

/** 编辑区的一句话：这份规则的请求会怎样被加上这段文字。 */
export function promptRuleOneLineSummary(rule: PromptRuleDraft, planNames: Map<number, string>, channelNames: Map<number, string>): string {
    const who = rule.audience === 'admin_only' ? '管理员 Key 的请求' : '所有用户的请求';
    let planPart = '不管调用哪个方案';
    if (rule.plan_mode === 'selected') {
        const names = rule.plan_ids.map((id) => planNames.get(id) ?? `方案 ${id}`);
        const planText = names.length ? `${names.join('、')} 方案` : '';
        if (planText && rule.include_no_plan) planPart = `${planText}、没选方案的 Key 的请求`;
        else if (planText) planPart = planText;
        else if (rule.include_no_plan) planPart = '没选方案的 Key 的请求';
        else planPart = '（还没选方案）';
    }
    let modelPart = '';
    if (rule.model_mode === 'selected') {
        const patterns = rule.models.map((p) => p.trim()).filter(Boolean);
        modelPart = patterns.length ? `用 ${patterns.join('、')} 模型时` : '（还没选模型）';
    }
    let channelPart = '不管走哪个渠道';
    if (rule.channel_mode === 'include') {
        const names = rule.channel_ids.map((id) => channelNames.get(id) ?? `#${id}`);
        channelPart = names.length ? `走 ${capNames(names, 3)} 渠道时` : '（还没选渠道）';
    } else if (rule.channel_mode === 'exclude') {
        const names = rule.channel_ids.map((id) => channelNames.get(id) ?? `#${id}`);
        channelPart = names.length ? `走除 ${capNames(names, 3)} 外的渠道时` : '（还没选渠道）';
    }
    const where = rule.position === 'after' ? '后面' : '前面';
    return `${who}，${planPart}，${modelPart ? `${modelPart}，` : ''}${channelPart}，都在系统提示词的${where}加这段。`;
}

/** 模型 pattern 之间是否可能命中同一个模型（编辑区的「同时生效」交集用）。 */
function draftModelOverlap(a: PromptRuleDraft, b: PromptRuleDraft): boolean {
    if (a.model_mode === 'all' || b.model_mode === 'all') return true;
    if (a.model_mode !== 'selected' || b.model_mode !== 'selected') return false;
    const exact = (rule: PromptRuleDraft) => rule.models.map((p) => p.trim().toLowerCase()).filter(Boolean);
    const wild = (rule: PromptRuleDraft) => rule.models.map((p) => p.trim().toLowerCase()).filter((p) => p.endsWith('*') && p.length > 1).map((p) => p.slice(0, -1));
    for (const pa of exact(a)) {
        for (const pb of exact(b)) {
            if (pa === pb) return true;
            if (wild(a).some((prefix) => pb.startsWith(prefix))) return true;
            if (wild(b).some((prefix) => pa.startsWith(prefix))) return true;
        }
    }
    for (const wa of wild(a)) {
        for (const wb of wild(b)) {
            if (wa.startsWith(wb) || wb.startsWith(wa)) return true;
        }
    }
    return false;
}

function draftPlanOverlap(a: PromptRuleDraft, b: PromptRuleDraft): boolean {
    if (a.plan_mode === 'all' || b.plan_mode === 'all') return true;
    if (a.plan_mode !== 'selected' || b.plan_mode !== 'selected') return false;
    if (a.plan_ids.some((id) => b.plan_ids.includes(id))) return true;
    return a.include_no_plan || b.include_no_plan;
}

function draftChannelOverlap(a: PromptRuleDraft, b: PromptRuleDraft): boolean {
    if (a.channel_mode === 'all' || b.channel_mode === 'all') return true;
    if (a.channel_mode === 'include' && b.channel_mode === 'include') {
        return a.channel_ids.some((id) => b.channel_ids.includes(id));
    }
    // 有 exclude 的组合：多数情况存在「不在两份名单里」的渠道；include+exclude 交集可能为空。
    if (a.channel_mode === 'include' && b.channel_mode === 'exclude') {
        return a.channel_ids.some((id) => !b.channel_ids.includes(id));
    }
    if (a.channel_mode === 'exclude' && b.channel_mode === 'include') {
        return b.channel_ids.some((id) => !a.channel_ids.includes(id));
    }
    return true;
}

/** 和哪些规则会同时生效（存在某个请求两边都命中），按 方案/渠道/模型 范围求交集。 */
export function coApplicableRules(current: PromptRuleDraft, rules: PromptRule[]): PromptRule[] {
    return rules
        .filter((rule) => rule.id !== current.id && rule.enabled && rule.text.trim())
        .filter((rule) => {
            const other = promptRuleFromSaved(rule);
            return draftPlanOverlap(current, other) && draftChannelOverlap(current, other) && draftModelOverlap(current, other);
        });
}

// ── 保存阻断 ──

/** 内容上限：与后端 prompt_rules.text（262144 字节）一致，按字节算而不是字数。 */
export const PROMPT_RULE_TEXT_MAX_BYTES = 262144;

/**
 * 模型 pattern 的问题：与后端同类校验一致——只允许一个星且在末尾（裸 * 除外），
 * 去空、转小写后不许重复（精确名与尾星各算一套，Claude-* 和 claude-* 算重复）。
 */
export function promptRulePatternProblem(patterns: string[]): string | null {
    const exact = new Set<string>();
    const wild = new Set<string>();
    for (const raw of patterns) {
        const pattern = raw.trim().toLowerCase();
        if (!pattern) continue;
        const stars = pattern.split('*').length - 1;
        if (stars > 1 || (stars === 1 && !pattern.endsWith('*'))) {
            return `模型「${raw.trim()}」只能有一个星，而且要放在最后（比如 claude-*）`;
        }
        const isWild = pattern.endsWith('*');
        const key = isWild ? pattern.slice(0, -1) : pattern;
        const bucket = isWild ? wild : exact;
        if (bucket.has(key)) return `模型「${raw.trim()}」写重复了`;
        bucket.add(key);
    }
    return null;
}

/** 保存按钮的阻断原因（后端同样会拒，这里提前拦成好懂的提示）；null = 可以保存。 */
export function promptRuleSaveProblem(rule: PromptRuleDraft): string | null {
    if (!rule.name.trim()) return '规则要有名字';
    if (rule.plan_mode === 'selected' && rule.plan_ids.length === 0 && !rule.include_no_plan) {
        return '方案范围还没选：至少勾一个方案，或勾上「没选方案的 Key」';
    }
    if (rule.channel_mode !== 'all' && rule.channel_ids.length === 0) {
        return rule.channel_mode === 'include' ? '「只在这些」还没勾渠道' : '「除了这些」还没勾渠道';
    }
    if (rule.model_mode === 'selected' && !rule.models.some((p) => p.trim())) return '指定模型还没填名字';
    const patternProblem = promptRulePatternProblem(rule.models);
    if (patternProblem) return patternProblem;
    if (new TextEncoder().encode(rule.text).length > PROMPT_RULE_TEXT_MAX_BYTES) {
        return `内容太长了，删掉一些再保存（最多 ${PROMPT_RULE_TEXT_MAX_BYTES} 字节）`;
    }
    return null;
}

/** 列表搜索：名字、内容、模型 pattern 挨词匹配（空格分词，不分大小写）。 */
export function matchesRuleSearch(query: string, rule: PromptRule): boolean {
    return matchesPromptSearch(query, rule.name, rule.text, rule.models.join(' '));
}

/**
 * 草稿 → create/update 的 body：名字与文案 trim；模型 pattern 去空、按小写去重（保留第一个的写法）。
 * sort 用调用方传的服务端最新值：草稿里的 sort 可能已经被别人调顺序调旧了。
 */
export function promptRuleDraftPayload(rule: PromptRuleDraft, sortOverride?: number): PromptRulePayload {
    const seenPatterns = new Set<string>();
    const models: string[] = [];
    for (const raw of rule.models) {
        const pattern = raw.trim();
        const key = pattern.toLowerCase();
        if (!pattern || seenPatterns.has(key)) continue;
        seenPatterns.add(key);
        models.push(pattern);
    }
    return {
        id: rule.id,
        name: rule.name.trim(),
        enabled: rule.enabled,
        text: rule.text.trim(),
        position: rule.position,
        plan_mode: rule.plan_mode,
        plan_ids: [...rule.plan_ids],
        include_no_plan: rule.include_no_plan,
        channel_mode: rule.channel_mode,
        channel_ids: [...rule.channel_ids],
        model_mode: rule.model_mode,
        models,
        audience: rule.audience,
        sort: sortOverride ?? rule.sort ?? 0,
    };
}

/** 模型 pattern 输入框：逗号/换行分隔，去掉空项与重复项。 */
export function parseModelPatterns(input: string): string[] {
    return input.split(/[\n,，]/).map((p) => p.trim()).filter((p, i, list) => p && list.indexOf(p) === i);
}

/**
 * 模型输入框原文与已保存值对不上时该怎么办：
 * 去空/去重后的值一样就返回 null（保留用户正在敲的尾逗号、尾换行，不能吃掉），
 * 真变了就返回外部的新文本让输入框跟上。
 */
export function promptRuleModelsTextSync(currentText: string, signature: string): string | null {
    if (parseModelPatterns(currentText).join('\n') === signature) return null;
    return signature;
}

/** 规则名字上限（按码点算，与后端 validatePromptRule 的 191 rune 一致）。 */
export const PROMPT_RULE_NAME_MAX = 191;

/** " 副本"占的位置（空格 + 两个汉字）。 */
const PROMPT_RULE_COPY_SUFFIX = ' 副本';

/**
 * 复制一条时的名字：按 Unicode 码点截到 191-3 个再拼 " 副本"。
 * 不用 slice(0, 188)，那会按 UTF-16 把 emoji 这类字劈成两半。
 */
export function promptRuleCopyName(name: string): string {
    const base = name.trim();
    const budget = PROMPT_RULE_NAME_MAX - [...PROMPT_RULE_COPY_SUFFIX].length;
    const chars = [...base];
    const head = chars.length > budget ? chars.slice(0, budget).join('') : base;
    return `${head}${PROMPT_RULE_COPY_SUFFIX}`;
}
