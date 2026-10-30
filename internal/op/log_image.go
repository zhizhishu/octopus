package op

import (
	"context"
	"sort"
	"time"

	"github.com/zhizhishu/octopus/internal/db"
	"github.com/zhizhishu/octopus/internal/logimage"
	"github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/utils/log"
	"gorm.io/gorm"
)

// 「真实出站请求体」另存图片的引用与清理。
//
// 文件按内容哈希去重，一张图可能被多条日志共用，所以清理分两步：
//
//  1. 日志行被删（保留期 / 容量 / 手动清空）时，顺手删掉 relay_log_images 里
//     这些 log_id 的引用行；
//  2. 清扫：目录里一条引用都没有、且超过宽限期的文件删掉；目录总量超过上限时
//     先删最旧的无引用文件，再删最旧的有引用文件（连同它的引用行）。
//
// 宽限期是给「图已写盘、日志还在内存待落库」留的：那一刻文件确实还没有引用行。
// 同一张图被新请求复用时 logimage.Enqueue 会刷新 mtime，所以不会被误删。

const (
	// RelayLogImageOrphanGrace 是无引用文件的最短保留时间。
	RelayLogImageOrphanGrace = time.Hour
	// relayLogImageClearGrace 用于手动清空：用户要的是立刻腾出空间，
	// 只给正在落库的那几秒留一点余地。
	relayLogImageClearGrace = time.Minute
	relayLogImageQueryChunk = 500
)

func relayLogImageTableReady(tx *gorm.DB) bool {
	return tx != nil && tx.Migrator().HasTable(&model.RelayLogImage{})
}

// relayLogImageRefsInsert 在一批日志落库后写引用行。失败只记日志：最坏情况是
// 这几张图 1 小时后被当成孤儿删掉，日志本身不受影响。
func relayLogImageRefsInsert(tx *gorm.DB, batch []model.RelayLog) {
	var rows []model.RelayLogImage
	for _, l := range batch {
		if len(l.UpstreamImageHashes) == 0 {
			continue
		}
		seen := make(map[string]struct{}, len(l.UpstreamImageHashes))
		for _, h := range l.UpstreamImageHashes {
			if !logimage.ValidHash(h) {
				continue
			}
			if _, dup := seen[h]; dup {
				continue
			}
			seen[h] = struct{}{}
			rows = append(rows, model.RelayLogImage{LogID: l.ID, Hash: h, Time: l.Time})
		}
	}
	if len(rows) == 0 {
		return
	}
	if err := tx.CreateInBatches(rows, 200).Error; err != nil {
		log.Warnf("relay log image refs insert failed (%d rows): %v", len(rows), err)
	}
}

// relayLogImageRefsDeleteByLogIDs 删掉这批日志的图片引用。失败不拦主表删除：
// 清扫开头会把指向已不存在日志的引用行一并清掉，不会留下永久孤儿。
func relayLogImageRefsDeleteByLogIDs(tx *gorm.DB, ids []int64) {
	if len(ids) == 0 || !relayLogImageTableReady(tx) {
		return
	}
	if err := tx.Where("log_id IN ?", ids).Delete(&model.RelayLogImage{}).Error; err != nil {
		log.Warnf("relay log image refs delete failed (%d logs): %v", len(ids), err)
	}
}

// RelayLogImageSweep 清扫图片目录：删无引用且超过 minAge 的文件，再按总容量上限收缩。
// 返回删掉的文件数。
func RelayLogImageSweep(ctx context.Context, minAge time.Duration) (int, error) {
	conn := db.GetDB().WithContext(ctx)
	if !relayLogImageTableReady(conn) {
		// 引用表都没有就无从判断谁还在用，一张都不删。
		return 0, nil
	}
	// 指向已不存在日志的引用行（某条删除路径没顺手删到）先清掉，否则它们会让
	// 文件永远「有引用」。
	if err := conn.Where("NOT EXISTS (SELECT 1 FROM relay_logs WHERE relay_logs.id = relay_log_images.log_id)").
		Delete(&model.RelayLogImage{}).Error; err != nil {
		return 0, err
	}

	files, err := logimage.List()
	if err != nil || len(files) == 0 {
		return 0, err
	}
	referenced, err := relayLogImageReferenced(conn, files)
	if err != nil {
		return 0, err
	}

	now := time.Now()
	removed := 0
	var keep []logimage.File
	var total int64
	for _, f := range files {
		if _, ok := referenced[f.Hash]; !ok && now.Sub(f.ModTime) >= minAge {
			if err := logimage.Remove(f.Hash); err == nil {
				removed++
				continue
			}
		}
		keep = append(keep, f)
		total += f.Size
	}

	maxBytes := logimage.MaxTotalBytes()
	if total <= maxBytes {
		return removed, nil
	}
	// 超容量时的删除顺序（各档内按 mtime 从旧到新）：
	//  0. 无引用、且已过 relayLogImageClearGrace 的旧文件；
	//  1. 有引用的文件（日志还在，点开会显示「原图已清理」）；
	//  2. 无引用但刚写盘不到一分钟的——它们多半属于还在内存里、马上就落库的日志，
	//     先删掉会让刚发生的请求一张图都看不到，所以放到最后。
	tier := func(f logimage.File) int {
		if _, ok := referenced[f.Hash]; ok {
			return 1
		}
		if now.Sub(f.ModTime) < relayLogImageClearGrace {
			return 2
		}
		return 0
	}
	sort.SliceStable(keep, func(i, j int) bool {
		ti, tj := tier(keep[i]), tier(keep[j])
		if ti != tj {
			return ti < tj
		}
		return keep[i].ModTime.Before(keep[j].ModTime)
	})
	for _, f := range keep {
		if total <= maxBytes {
			break
		}
		if err := logimage.Remove(f.Hash); err != nil {
			continue
		}
		total -= f.Size
		removed++
		if _, ok := referenced[f.Hash]; ok {
			// 日志行留着，占位符点开会显示「原图已清理」。
			if err := conn.Where("hash = ?", f.Hash).Delete(&model.RelayLogImage{}).Error; err != nil {
				log.Warnf("relay log image ref delete for evicted %s failed: %v", f.Hash[:12], err)
			}
		}
	}
	return removed, nil
}

func relayLogImageReferenced(conn *gorm.DB, files []logimage.File) (map[string]struct{}, error) {
	out := make(map[string]struct{})
	hashes := make([]string, 0, len(files))
	for _, f := range files {
		hashes = append(hashes, f.Hash)
	}
	for start := 0; start < len(hashes); start += relayLogImageQueryChunk {
		end := min(start+relayLogImageQueryChunk, len(hashes))
		var got []string
		if err := conn.Model(&model.RelayLogImage{}).
			Where("hash IN ?", hashes[start:end]).
			Distinct("hash").Pluck("hash", &got).Error; err != nil {
			return nil, err
		}
		for _, h := range got {
			out[h] = struct{}{}
		}
	}
	return out, nil
}

func relayLogImageSweepLogged(ctx context.Context, minAge time.Duration) {
	if n, err := RelayLogImageSweep(ctx, minAge); err != nil {
		log.Warnf("relay log image sweep failed: %v", err)
	} else if n > 0 {
		log.Debugf("relay log image sweep removed %d files", n)
	}
}
