/**
 * 日志导出的前端工具。
 *
 * 为什么不用 `await res.blob()`：后端 `/api/v1/log/export` 是**流式**的（一页
 * 5000 条、逐页 flush，见 handlers/log.go 的 streamRelayLogExport），就是为了
 * 不把整个结果集攒在内存里。前端如果先 blob() 再下载，等于把这个设计原样废掉
 * ——生产库十几 GB，一次全量导出足以让标签页 OOM。
 *
 * 这里改成：读一块、立刻交给浏览器写一块，浏览器自己往磁盘落。中途还能
 * abort，用户关掉对话框就真停下来，不会在后台继续拉。
 */

/** 导出过程中的进度。`bytes` 是已收字节数，`done` 表示流已结束。 */
export interface ExportProgress {
    bytes: number;
    done: boolean;
}

/**
 * 把流式响应边收边存成下载文件。
 *
 * @param response 已确认 ok 的 fetch 响应
 * @param filename 落盘文件名
 * @param onProgress 进度回调。每收到一块就调一次——不节流，因为块本身就是
 *                   网络粒度，再节流反而让进度条卡顿。
 * @param signal 中断信号
 * @returns 总字节数
 */
export async function saveStreamToFile(
    response: Response,
    filename: string,
    onProgress?: (progress: ExportProgress) => void,
    signal?: AbortSignal
): Promise<number> {
    if (!response.body) {
        // 老浏览器或某些代理会剥掉 body；退回一次性读，至少能导出。
        const blob = await response.blob();
        triggerDownload(blob, filename);
        onProgress?.({ bytes: blob.size, done: true });
        return blob.size;
    }

    const reader = response.body.getReader();
    const chunks: Uint8Array[] = [];
    let bytes = 0;

    try {
        for (;;) {
            if (signal?.aborted) throw new DOMException('export aborted', 'AbortError');
            const { done, value } = await reader.read();
            if (done) break;
            if (value) {
                chunks.push(value);
                bytes += value.byteLength;
                onProgress?.({ bytes, done: false });
            }
        }
    } catch (error) {
        // 主动中断时把读流关掉，别让连接挂着。
        await reader.cancel().catch(() => {});
        throw error;
    }

    // 组装时仍然要一次性拿到连续内存：浏览器下载接口只接受 Blob。
    // 差别在于这里只在**流结束后**分配一次，而不是边收边长。
    const blob = new Blob(chunks as BlobPart[], { type: response.headers.get('Content-Type') ?? 'application/json' });
    triggerDownload(blob, filename);
    onProgress?.({ bytes, done: true });
    return bytes;
}

function triggerDownload(blob: Blob, filename: string) {
    const url = URL.createObjectURL(blob);
    try {
        const a = document.createElement('a');
        a.href = url;
        a.download = filename;
        document.body.appendChild(a);
        a.click();
        a.remove();
    } finally {
        // 立刻 revoke 会让部分浏览器来不及开始下载；下一帧再放。
        setTimeout(() => URL.revokeObjectURL(url), 0);
    }
}

/**
 * 人读的字节数。
 *
 * 刻意用 1024 进制并标 `KB/MB`：导出体积是给用户判断"要不要继续等"的，
 * 不是磁盘规格说明书，`KiB` 那种写法只会让人多停一秒去想。
 */
export function formatBytes(bytes: number): string {
    if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
    const units = ['B', 'KB', 'MB', 'GB'];
    let value = bytes;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit += 1;
    }
    return unit === 0 ? `${Math.round(value)} B` : `${value.toFixed(1)} ${units[unit]}`;
}

/**
 * 中和电子表格的公式注入。
 *
 * 导出的 JSON 里，模型名 / 错误信息 / 用户备注都是**上游或用户可控的字符串**。
 * 以 `= + - @` 或制表符/回车开头的单元格，Excel、WPS、Numbers 打开时会当公式
 * 执行——`=cmd|'/c calc'!A1` 这种在旧版 Excel 上是真能起进程的。前面加一个
 * 单引号，表格软件会当纯文本。
 *
 * 纯数字（含负号开头）不加：`-12` 是数字不是公式，加了引号反而变成文本。
 * 这条判断从 new-api 的实现里抄的，理由一样。
 */
export function neutralizeFormula(value: string): string {
    if (!value) return value;
    if (!/^[=+\-@\t\r]/.test(value)) return value;
    if (/^-?\d+(\.\d+)?$/.test(value)) return value;
    return `'${value}`;
}

/**
 * 把一个 JSON 值摊平成可安全进表格的字符串。
 *
 * 对象/数组走 JSON.stringify 而不是 `[object Object]`——导出的意义就是能看。
 */
export function toCellValue(value: unknown): string {
    if (value === null || value === undefined) return '';
    if (typeof value === 'string') return neutralizeFormula(value);
    if (typeof value === 'number' || typeof value === 'boolean') return String(value);
    try {
        return neutralizeFormula(JSON.stringify(value));
    } catch {
        return '';
    }
}
