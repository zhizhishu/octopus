'use client';

/**
 * 鹈鹕 SVG → PNG 的浏览器端栅格化（fix-ui1127b-r2 B1 版）。
 *
 * 安全边界：模型输出的 SVG 是不可信内容，**主页面从头到尾不加载它**——
 * 栅格化发生在与显示框同一个威胁模型下的隔离框里（sandbox="allow-scripts"、
 * CREATIVE_FRAME_CSP 只放行内联资源，框里连 data:/blob: 之外的字节都摸不到，
 * SVG 里的外部 <image href>/外部字体在框内根本发不出请求）。PNG 字节经
 * postMessage 回传，主页面只认一次性随机 token 对得上、且 source 正是这个
 * 框的消息。
 *
 * 整页 HTML 没有现成截图库可用（仓里没有 html2canvas 之类），按约定不硬造
 * html rasterizer：HTML 的图不自动存 PNG，页面上有说明。
 *
 * 尺寸解析（creativeSVGIntrinsicSize/creativePNGRasterSize）与 srcdoc/消息
 * 校验（creativeRasterSrcDoc/creativePNGMessageParse）在 v6.ts 纯函数层可单测；
 * 本文件只剩 DOM 接线。
 */

import { creativePNGRasterSize, creativePNGMessageParse, creativePNGToken, creativeRasterSrcDoc, creativeSVGIntrinsicSize } from './v6';

/** 栅格化超时：框里脚本崩溃/CSP 拦截时不能让用户一直等，放弃这张不阻塞看图。 */
const RASTER_TIMEOUT_MS = 15000;

/**
 * 把一段 SVG 源文字栅格化成 PNG Blob。解析失败、canvas 不可用、超时都返回
 * null——调用方放弃这张，不阻塞用户看图。
 */
export function rasterizeCreativeSVG(content: string): Promise<Blob | null> {
    if (typeof document === 'undefined' || !document.body) return Promise.resolve(null);
    const size = creativePNGRasterSize(creativeSVGIntrinsicSize(content));
    const token = creativePNGToken();
    return new Promise<Blob | null>((resolve) => {
        const iframe = document.createElement('iframe');
        iframe.setAttribute('sandbox', 'allow-scripts');
        iframe.style.display = 'none';
        iframe.setAttribute('srcdoc', creativeRasterSrcDoc(content, token, size.width, size.height));
        let settled = false;
        const finish = (blob: Blob | null) => {
            if (settled) return;
            settled = true;
            window.clearTimeout(timer);
            window.removeEventListener('message', onMessage);
            iframe.remove();
            resolve(blob);
        };
        const onMessage = (event: MessageEvent) => {
            // 来源校验：srcdoc 沙箱框是 opaque origin（event.origin === "null"），
            // 且事件 source 必须就是我们挂的这个框——两条都满足才看内容。
            if (event.source !== iframe.contentWindow) return;
            if (event.origin !== 'null' && event.origin !== window.location.origin) return;
            const png = creativePNGMessageParse(event.data, token);
            if (png === undefined) return;
            finish(png ? new Blob([png], { type: 'image/png' }) : null);
        };
        const timer = window.setTimeout(() => finish(null), RASTER_TIMEOUT_MS);
        window.addEventListener('message', onMessage);
        document.body.appendChild(iframe);
    });
}
