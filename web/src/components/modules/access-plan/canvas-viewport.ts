// 方案无限画布的「视口要不要动」判定（纯函数，便于 node --test 直接测）。
//
// 标准 React Flow 做法：数据编辑（+1/−1、弹窗保存、增删映射、保存后的回刷）只原地
// setNodes（同一批 id），视口一律不动；只有「换了一个视图」才自动 fit：
//   - 首次挂载（没有同方案的已存视口时）
//   - 切换了方案（plan id 变化）
//   - 切换了筛选视图（全部/按渠道/按映射、选中的渠道或模型、搜索词）——这是用户在换
//     要看的东西，不是改数据；不 fit 的话筛完结果可能整块落在视口外
//   - 画布从「空」变成「有内容」（ReactFlow 重新挂载时没有可沿用的视口）
// 「显示全部」按钮与全屏切换走显式 fitView，不经过这里。

export type CanvasViewKey = {
    planId: number;
    /** 筛选视图签名：viewMode | 渠道 | 映射模型 | 搜索词 */
    view: string;
    empty: boolean;
};

export type CanvasViewportAction = 'fit' | 'restore' | 'keep';

export type CanvasViewport = { x: number; y: number; zoom: number };

export function canvasViewSignature(
    viewMode: string,
    selectedChannelId: string,
    selectedMappingModel: string,
    query: string,
): string {
    return [viewMode, selectedChannelId, selectedMappingModel, query.trim().toLowerCase()].join('|');
}

/**
 * @param prev       上一次处理过的视图键；null = 本组件首次处理（挂载）
 * @param next       当前视图键
 * @param hasSavedViewport 是否存着「同一方案」上次的视口（重挂载兜底）
 */
export function decideCanvasViewport(
    prev: CanvasViewKey | null,
    next: CanvasViewKey,
    hasSavedViewport: boolean,
): CanvasViewportAction {
    if (next.empty) return 'keep';
    if (prev === null) return hasSavedViewport ? 'restore' : 'fit';
    if (prev.planId !== next.planId) return 'fit';
    if (prev.view !== next.view) return 'fit';
    if (prev.empty) return 'fit';
    return 'keep';
}

// 视口兜底：只记住「最近一个方案」的视口。同一方案的画布若被重新挂载（离开页面再回来、
// 未来某处又引入了 remount），回到原位置；换成别的方案则不命中，照常 fit。
let savedViewport: { planId: number; viewport: CanvasViewport } | null = null;

export function rememberCanvasViewport(planId: number, viewport: CanvasViewport): void {
    if (!Number.isFinite(viewport.x) || !Number.isFinite(viewport.y) || !Number.isFinite(viewport.zoom) || viewport.zoom <= 0) return;
    savedViewport = { planId, viewport: { x: viewport.x, y: viewport.y, zoom: viewport.zoom } };
}

export function savedCanvasViewport(planId: number): CanvasViewport | null {
    return savedViewport && savedViewport.planId === planId ? savedViewport.viewport : null;
}

export function forgetCanvasViewport(): void {
    savedViewport = null;
}

/**
 * 编辑器本地 targets 何时用服务器数据重置：服务器版本号变了就以服务器为准。
 *
 * 父级原来写的是 `key=routes-${id}-${updated_at}`，本意是服务器数据一变就整块重挂载、
 * 让 useState 初值重新读服务器。但后端 AccessPlan 根本不返回 updated_at，那个 key 恒为
 * `routes-${id}-0`：重挂载从未发生，外部改动（另一位管理员 / 渠道自动同步）也从来没刷进编辑器。
 * 这里改用路由目标内容本身做版本号，在同一编辑器内原地重置，画布节点按同 id 更新、视口不动；
 * 若以后后端补上 updated_at，也一并计入。
 */
export function routeTargetsServerVersion(plan: { id: number; updated_at?: number | null; route_targets?: unknown }): string {
    return `${plan.id}:${plan.updated_at ?? 0}:${JSON.stringify(plan.route_targets ?? [])}`;
}

/**
 * 是否该把本地 targets 重置为服务器数据。保存请求进行中时先不动（保存成功回调会写入服务器结果，
 * 中途的列表回刷可能还是旧数据，立刻重置会把用户刚连点的第二下闪回去）。
 * 本地有没保存的改动（弹窗里改到一半、加了一行还没存）时也不动：列表回刷不能把人家正在改的东西冲掉，
 * 等保存或放弃之后再对齐。
 */
export function shouldResyncRouteTargets(syncedVersion: string, serverVersion: string, savePending: boolean, localDirty = false): boolean {
    return !savePending && !localDirty && syncedVersion !== serverVersion;
}
