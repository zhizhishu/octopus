'use client';

/**
 * 鹈鹕 SVG → PNG 的浏览器端栅格化。
 *
 * 为什么不在隔离框里「截屏」：鹈鹕图的 iframe 只有 allow-scripts（**没有**
 * allow-same-origin，框里的像素父页面读不到，这是故意的安全边界，不能为截图
 * 放开）。所以「渲染完成截一张」的实现是：父页面拿**同一段 SVG 源文字**，在
 * canvas 上按它本来的尺寸重新栅格化——和隔离框里渲染出来的是同一张图。
 *
 * 整页 HTML 没有现成截图库可用（仓里没有 html2canvas 之类），按约定不硬造
 * html rasterizer：HTML 的图不自动存 PNG，页面上有说明。
 *
 * 这里全是 DOM 依赖代码，不放 v6.ts——那边是纯函数层，node --test 加载不了
 * canvas；尺寸解析（creativeSVGIntrinsicSize/creativePNGRasterSize）在 v6.ts
 * 里可以单测。
 */

import { creativePNGRasterSize, creativeSVGIntrinsicSize } from './v6';

/**
 * 把一段 SVG 源文字栅格化成 PNG Blob。
 *
 * 「渲染完成」的信号就是 Image 的 onload（SVG 解码完成）；解析失败、canvas
 * 不可用、SVG 引了外部资源把 canvas 污染（toBlob 抛 SecurityError）都返回
 * null——调用方放弃这张，不阻塞用户看图。
 */
export async function rasterizeCreativeSVG(content: string): Promise<Blob | null> {
    const size = creativePNGRasterSize(creativeSVGIntrinsicSize(content));
    const url = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(content)}`;
    const img = await new Promise<HTMLImageElement | null>((resolve) => {
        const image = new Image();
        image.onload = () => resolve(image);
        image.onerror = () => resolve(null);
        image.src = url;
    });
    if (!img) return null;
    let canvas: HTMLCanvasElement;
    try {
        canvas = document.createElement('canvas');
        canvas.width = size.width;
        canvas.height = size.height;
        const ctx = canvas.getContext('2d');
        if (!ctx) return null;
        // SVG 默认透明底；隔离框显示时就是白底，PNG 也存白底，跟页面看到的一致。
        ctx.fillStyle = '#ffffff';
        ctx.fillRect(0, 0, canvas.width, canvas.height);
        ctx.drawImage(img, 0, 0, canvas.width, canvas.height);
        return await new Promise<Blob | null>((resolve) => canvas.toBlob((blob) => resolve(blob), 'image/png'));
    } catch {
        return null;
    }
}
