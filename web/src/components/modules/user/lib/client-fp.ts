// 用户抽屉「客户端指纹」页的纯函数（不碰界面文字）。
import type { UserClientFingerprint } from '@/api/endpoints/user';

/** 这些头的值后端从不保存（只记名字），界面上给它们一把锁。和后端 telemetry.isSecretHeader 同一套规则。 */
const SECRET_NAMES = new Set(['authorization', 'proxy-authorization', 'cookie', 'set-cookie', 'x-api-key', 'api-key', 'x-goog-api-key', 'mj-api-secret', 'sec-websocket-protocol', 'sec-websocket-key', 'x-auth-token']);
const SECRET_HINTS = ['auth', 'token', 'secret', 'key', 'password', 'passwd', 'signature', 'checksum', 'credential', 'cookie', 'session-token'];

export function isSecretHeader(name: string): boolean {
    const lower = name.trim().toLowerCase();
    return SECRET_NAMES.has(lower) || SECRET_HINTS.some((h) => lower.includes(h));
}

/** 头名列表（后端用逗号拼的）。 */
export function headerNames(fp: Pick<UserClientFingerprint, 'header_names'>): string[] {
    return (fp.header_names ?? '').split(',').map((s) => s.trim()).filter(Boolean);
}

/** 留了值的身份头，按名字排。解析失败返回空。 */
export function notableEntries(fp: Pick<UserClientFingerprint, 'notable'>): Array<[string, string]> {
    if (!fp.notable) return [];
    try {
        const obj = JSON.parse(fp.notable) as Record<string, unknown>;
        return Object.entries(obj)
            .filter(([, v]) => typeof v === 'string')
            .map(([k, v]) => [k, v as string] as [string, string])
            .sort(([a], [b]) => a.localeCompare(b));
    } catch {
        return [];
    }
}

/** 卡片标题：认得出的给名字 + 版本，认不出的给 UA 第一段；都没有时 undefined。 */
export function fpTitle(fp: Pick<UserClientFingerprint, 'client_name' | 'client_version' | 'user_agent'>): string | undefined {
    const name = fp.client_name?.trim();
    if (name) return fp.client_version ? `${name} ${fp.client_version}` : name;
    const ua = fp.user_agent?.trim();
    return ua ? ua.split(/\s/)[0] : undefined;
}

/** 有没有任何一条拿到了 TLS 指纹（没有时提示「本服务不处理 HTTPS」）。 */
export function hasTLSFingerprint(fps: Array<Pick<UserClientFingerprint, 'ja3' | 'ja4'>>): boolean {
    return fps.some((f) => !!(f.ja3 || f.ja4));
}

/** 按 Key 筛（0 = 全部）。 */
export function filterByKey<T extends Pick<UserClientFingerprint, 'api_key_id'>>(fps: T[], keyID: number): T[] {
    return keyID > 0 ? fps.filter((f) => f.api_key_id === keyID) : fps;
}

/** 短指纹号：前 8 位。 */
export function shortHash(h: string | undefined): string {
    return (h ?? '').slice(0, 8);
}
