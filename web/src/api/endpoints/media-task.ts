import { useQuery } from '@tanstack/react-query';
import { apiClient } from '../client';
import { useAuthStore } from './user';

/**
 * 「图片/视频任务」合并列表（fix-ui1127a）。字段与 video-task.ts 逐字对齐
 * （后端 mediaTaskListItem 对齐 videoTaskListItem），多一个 kind 维度区分类型。
 * 词表不新造：图片任务的 processing/completed/failed 与视频的 not_start…failure
 * 都是上游/后端原文，unknown 表示「本地不存状态」（midjourney）。
 */
export type MediaTaskKind = 'image' | 'video';

export interface MediaTaskItem {
    kind: MediaTaskKind;
    task_id: string;
    model: string;
    status: string;
    progress: string;
    fail_reason: string;
    result_url: string;
    submit_time: number;
    start_time: number;
    finish_time: number;
    created_at: number;
}

export interface MediaTaskListPage {
    items: MediaTaskItem[];
    total: number;
    page: number;
    page_size: number;
}

function useMediaTaskCacheScope(): string | null {
    const userId = useAuthStore((state) => state.user?.id);
    if (userId === undefined || userId === null) return null;
    return `user:${userId}`;
}

const MEDIA_TASK_SCOPE_SLOT = 2;

function mediaTaskListQueryKey(scope: string | null, page: number, pageSize: number, kind: MediaTaskKind | '', status: string, model: string, startTime: number, endTime: number) {
    return ['media-tasks', 'list', scope, page, pageSize, kind, status, model, startTime, endTime] as const;
}

/** 终态判定（合并词表）：success/failure（视频）+ completed/failed（图片）。unknown 不是终态。 */
export function isMediaTaskTerminal(status: string): boolean {
    return status === 'success' || status === 'failure' || status === 'completed' || status === 'failed';
}

export function useMediaTaskList(options: {
    page?: number;
    pageSize?: number;
    kind?: MediaTaskKind | '';
    status?: string;
    model?: string;
    startTime?: number;
    endTime?: number;
    enabled?: boolean;
} = {}) {
    const { page = 1, pageSize = 20, kind = '', status = '', model = '', startTime = 0, endTime = 0, enabled = true } = options;
    const scope = useMediaTaskCacheScope();

    return useQuery({
        queryKey: mediaTaskListQueryKey(scope, page, pageSize, kind, status, model, startTime, endTime),
        queryFn: async () => {
            const params = new URLSearchParams();
            params.set('page', String(page));
            params.set('page_size', String(pageSize));
            if (kind) params.set('type', kind);
            if (status) params.set('status', status);
            if (model) params.set('model', model);
            if (startTime) params.set('start_time', String(startTime));
            if (endTime) params.set('end_time', String(endTime));
            return apiClient.get<MediaTaskListPage>(`/api/v1/media-task/list?${params.toString()}`);
        },
        enabled: enabled && scope !== null,
        placeholderData: (previousData, previousQuery) => {
            if (!previousQuery) return undefined;
            if (previousQuery.queryKey[MEDIA_TASK_SCOPE_SLOT] !== scope) return undefined;
            return previousData;
        },
        refetchInterval: (query) => {
            const items = query.state.data?.items ?? [];
            return items.some((item) => !isMediaTaskTerminal(item.status)) ? 5000 : false;
        },
    });
}
