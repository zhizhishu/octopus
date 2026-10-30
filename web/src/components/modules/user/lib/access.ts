// 用户页「IP / 客户端」的纯函数（不碰界面文字，文字在组件里按语言拼）。
import type { UserAccessAlert, UserAccessSummary } from '@/api/endpoints/user';
import { humanizeClient } from '../../log/humanize';

/** 客户端显示名：认得出的给名字 + 版本（Claude CLI 2.0.1），认不出的给 UA 原文（去掉网址）。没带 UA 时返回 undefined。 */
export function clientLabel(ua: string | undefined): string | undefined {
    const raw = ua?.trim();
    if (!raw) return undefined;
    const name = humanizeClient(raw);
    if (!name) return undefined;
    // 认不出时 humanizeClient 返回的就是原文（里面本来就有版本），不再拼。
    if (name.includes('/') || name === raw) return name;
    // 浏览器 UA 的 Mozilla/5.0 不是版本号。
    if (/^mozilla\//i.test(raw)) return name;
    const version = /^[^\s/]+\/v?(\d[\w.+-]*)/.exec(raw)?.[1];
    return version ? `${name} ${version}` : name;
}

export type AccessBadgeKind = 'manyIPs' | 'newIP' | 'newClient' | 'ipAndClient';

/** 一个用户的几条提示合成一个徽章：IP 过多优先；IP 和客户端同时换了合成一个。没有提示时 null。 */
export function accessBadge(alerts: UserAccessAlert[] | undefined): AccessBadgeKind | null {
    if (!alerts || alerts.length === 0) return null;
    const kinds = new Set(alerts.map((a) => a.kind));
    const ip = kinds.has('many_ips') ? 'manyIPs' : kinds.has('new_ip') ? 'newIP' : null;
    const client = kinds.has('new_client');
    if (ip === 'newIP' && client) return 'ipAndClient';
    if (ip) return ip;
    return client ? 'newClient' : null;
}

export interface LastAccess {
    ip: string;
    userAgent?: string;
    keyName?: string;
    at: number;
}

/**
 * 列表里「最近调用」显示哪一次：来源表（带客户端）和用户表上的 last_relay_ip/at（老数据、只有 IP）谁新用谁。
 * 来源表把失败的请求也算进去，所以它常常更新一点；老库升级后来源表还没数据时退回 last_relay_*。
 */
export function lastAccess(
    user: { last_relay_at?: number; last_relay_ip?: string },
    summary: Pick<UserAccessSummary, 'last'> | undefined
): LastAccess | null {
    const s = summary?.last;
    const legacyAt = user.last_relay_at ?? 0;
    if (s && s.at > 0 && s.at >= legacyAt) {
        return { ip: s.ip, userAgent: s.user_agent || undefined, keyName: s.api_key_name || undefined, at: s.at };
    }
    if (legacyAt > 0) {
        // 同一个 IP 时客户端还是可以借来源表的（老的 last_relay 只是没记 UA）。
        const ua = s && s.ip === user.last_relay_ip ? s.user_agent || undefined : undefined;
        return { ip: user.last_relay_ip || '', userAgent: ua, at: legacyAt };
    }
    return null;
}

/** 窗口长度拆成「n 小时」或「n 分钟」，给文案用。 */
export function windowParts(sec: number): { unit: 'hours' | 'minutes'; n: number } {
    const s = Math.max(60, Math.round(sec || 3600));
    if (s % 3600 === 0) return { unit: 'hours', n: s / 3600 };
    return { unit: 'minutes', n: Math.round(s / 60) };
}

/** 在判定窗口内才第一次出现的（抽屉里给它标「新」）。 */
export function isNewInWindow(firstSeen: number, nowSec: number, windowSec: number): boolean {
    return firstSeen > 0 && nowSec - firstSeen <= windowSec;
}

/** 列表太长时只列前几个，剩下的说「等 n 个」。 */
export function shortList(values: string[], max = 3): { shown: string[]; rest: number } {
    const clean = values.filter((v) => v !== undefined && v !== null);
    return { shown: clean.slice(0, max), rest: Math.max(0, clean.length - max) };
}
