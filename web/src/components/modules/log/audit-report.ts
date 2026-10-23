/**
 * `model_audit_report` 这一列的解析与类型。
 *
 * 单独成模块（而不是留在 AuditReportPanel.tsx 里）的唯一理由：**它要被测试直接加载**。
 * 这一列是 Go 侧 `behavior.ViewReport` 序列化出来的 JSON，前端只是照着读；
 * 两侧字段名一旦对不上，前端不会报错——JSON.parse 照样成功，只是每个字段都是
 * undefined，面板静默变成一片空白。那种失联没有任何编译期或运行期信号，
 * 只有一条对着真实 Go struct 的测试能拦住。
 */

export interface AuditReportPlain {
    level?: string;
    headline?: string;
    meaning?: string;
    caveat?: string;
    next_step?: string;
}

export interface AuditReportFinding {
    probe?: string;
    severity?: string;
    score?: number;
    title?: string;
    evidence?: Record<string, unknown>;
    recommendation?: string;
}

/** 后端 ProbeError 的真实形态是 `{probe, error}` —— 不是 probe_id/message。 */
export interface AuditReportProbeError {
    probe?: string;
    error?: string;
}

export interface AuditReport {
    requested_model?: string;
    requested_families?: string[];
    score?: number;
    verdict?: string;
    findings?: AuditReportFinding[];
    results?: unknown[];
    errors?: AuditReportProbeError[];
    disclaimer?: string;
    plain?: AuditReportPlain;
    family?: string;
}

/**
 * 把这一列解析成报告；没被检测过或数据坏了都返回 null。
 *
 * 为什么要吞掉解析异常而不是抛出：这一列是历史数据，库里可能存着旧版本写下的、
 * 或人工改坏的 JSON。为了一条读不懂的历史记录，让整个日志详情弹窗白屏——
 * 那等于用一条陈年脏数据惩罚用户查当前问题。
 * 返回 null = "这行没有可展示的检测明细"，其余详情照常显示。
 */
export function parseAuditReport(raw?: string | null): AuditReport | null {
    const text = raw?.trim();
    if (!text) return null;
    try {
        const parsed: unknown = JSON.parse(text);
        // 数组和 null 都是合法 JSON 但不是报告；放过去会在渲染时炸。
        if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) return null;
        return parsed as AuditReport;
    } catch {
        return null;
    }
}
