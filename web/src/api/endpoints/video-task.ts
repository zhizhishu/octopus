import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';
import { useAuthStore } from './user';

/**
 * 视频任务状态。
 *
 * 取值与后端 `model.VideoTaskStatus` 逐字对齐（也就是上游火山 Ark / 豆包视频生成的
 * status 原文），不在中间层翻译一套自己的词表——两套词表的漂移是排查时最容易骗人的东西。
 * `unknown` 刻意**不是**终态：上游返回了本层不认识的状态时最安全的假设是「还在变」。
 */
export type VideoTaskStatus =
    | 'not_start'
    | 'submitted'
    | 'queued'
    | 'in_progress'
    | 'success'
    | 'failure'
    | 'unknown';

/**
 * 一条视频任务。
 *
 * 字段名与后端 `internal/server/handlers/video_task.go` 的 videoTaskListItem 一一对应，
 * 语义照 new-api 的 `dto.TaskDto`（task_id / status / progress / fail_reason /
 * result_url / submit_time / finish_time），从别的系统迁过来不用重新学一套命名。
 * 后端刻意不返回渠道/密钥引用/归属/计费字段，所以这里也没有。
 */
export interface VideoTaskItem {
    task_id: string;
    model: string;
    status: VideoTaskStatus;
    progress: string;
    fail_reason: string;
    result_url: string;
    /** unix 秒；0 = 上游还没给这个时间点。 */
    submit_time: number;
    start_time: number;
    finish_time: number;
    created_at: number;
}

export interface VideoTaskListPage {
    items: VideoTaskItem[];
    total: number;
    page: number;
    page_size: number;
}

/** 状态 → 中文标签。unknown 不并进「失败」，它只是「还没收敛」。 */
export const VIDEO_TASK_STATUS_LABELS: Record<VideoTaskStatus, string> = {
    not_start: '未提交',
    submitted: '已提交',
    queued: '排队中',
    in_progress: '生成中',
    success: '成功',
    failure: '失败',
    unknown: '状态未知',
};

/** 终态判定：只有 success / failure 是终态，unknown 不算（与后端 IsVideoTaskTerminal 一致）。 */
export function isVideoTaskTerminal(status: VideoTaskStatus): boolean {
    return status === 'success' || status === 'failure';
}

/**
 * 缓存归属维度：这份列表是「谁」的。
 *
 * 后端按会话身份过滤（前端连别人的 id 都传不了），但缓存是浏览器本地的、全站共用
 * 一个 QueryClient（provider/query.tsx，staleTime 30s）。键里不带归属的话，A 登出、
 * B 在同一浏览器登录后，30 秒内会直接命中 A 的缓存——里面含结果视频地址；过期后
 * 也会先拿 A 的旧数据占位。服务端过滤再正确，也挡不住浏览器这一层的复用。
 *
 * 没有身份（未登录 / 还没拿到 user）时返回 null：此时不发请求。
 */
function useVideoTaskCacheScope(): string | null {
    const userId = useAuthStore((state) => state.user?.id);
    if (userId === undefined || userId === null) return null;
    return `user:${userId}`;
}

/** queryKey 里归属维度占的槽位，见 videoTaskListQueryKey。 */
const VIDEO_TASK_SCOPE_SLOT = 2;

/**
 * ['video-tasks', 'list', 归属, page, pageSize, status]
 *
 * 归属排在第三位而不是塞在末尾：它是「这是谁的缓存」的身份维度，不是筛选条件。
 */
function videoTaskListQueryKey(scope: string | null, page: number, pageSize: number, status: VideoTaskStatus | '') {
    return ['video-tasks', 'list', scope, page, pageSize, status] as const;
}

/**
 * 当前用户自己的视频任务列表（后端按会话身份过滤，前端传不了别人的）。
 *
 * 只有还有任务没进终态时才轮询 5 秒：结果该自己冒出来，但翻历史（全是终态）时
 * 不必每 5 秒扫一次库。
 */
export function useVideoTaskList(options: {
    page?: number;
    pageSize?: number;
    status?: VideoTaskStatus | '';
    enabled?: boolean;
} = {}) {
    const { page = 1, pageSize = 20, status = '', enabled = true } = options;
    const scope = useVideoTaskCacheScope();

    return useQuery({
        queryKey: videoTaskListQueryKey(scope, page, pageSize, status),
        queryFn: async () => {
            const params = new URLSearchParams();
            params.set('page', String(page));
            params.set('page_size', String(pageSize));
            if (status) params.set('status', status);
            return apiClient.get<VideoTaskListPage>(`/api/v1/video-task/list?${params.toString()}`);
        },
        // 没有身份就不发请求：这条列表注定是错的（更可能是上一个用户的），
        // 发出去只会往缓存里塞一份归属不明的数据。
        enabled: enabled && scope !== null,
        // 等价于 keepPreviousData —— 翻页/换筛选时保留上一页，不闪一下空列表 ——
        // 但只在**同一个人的缓存**之间搬运。TanStack 自带的 keepPreviousData 是全局的，
        // 换人时会把上一个用户的任务和结果地址也搬过来；这里按归属槽位比一下。
        placeholderData: (previousData, previousQuery) => {
            if (!previousQuery) return undefined;
            if (previousQuery.queryKey[VIDEO_TASK_SCOPE_SLOT] !== scope) return undefined;
            return previousData;
        },
        refetchInterval: (query) => {
            const items = query.state.data?.items ?? [];
            return items.some((item) => !isVideoTaskTerminal(item.status)) ? 5000 : false;
        },
    });
}
