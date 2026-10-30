package model

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
)

// UserAccessSeen 是「这个用户从哪里、用什么客户端来过」的聚合账：
// 每个 (用户, API Key, 入站 IP, 入站 User-Agent) 组合一行，记首次 / 最近出现时间和请求数。
//
// 为什么单独一张表而不是每次去 relay_logs 里 GROUP BY：
//   - relay_logs 可以被关掉（relay_log_keep_enabled=false）、被按天数 / 容量清掉，
//     「这个 IP 以前见过没有」需要比日志活得更久的记忆；
//   - 用户页一打开就要给所有人算「最近一次」和「异常切换」，扫日志表代价随请求量线性涨，
//     这张表的行数只随「组合数」涨。
//
// 数据来源只有入站请求：IP 是 middleware.ResolveClientIP 解析出的真实客户端 IP（与
// relay_logs.request_ip 同一个值，同一套可信反代规则），UA 是调用端原样的 User-Agent
// （与 relay_logs.request_user_agent 同一个值，512 rune 截断）。
// 纯观测：不参与路由 / 计费 / 出站指纹，出站请求头一个字节都不碰。
//
// SeenKey = sha1(user_id|api_key_id|ip|user_agent) 的十六进制，作唯一键：把 512 长的 UA
// 塞进联合唯一索引在 MySQL 上会撞索引长度上限，哈希列在三种库上都一样。
type UserAccessSeen struct {
	ID        int64  `json:"id" gorm:"primaryKey"`
	SeenKey   string `json:"-" gorm:"size:40;uniqueIndex:idx_user_access_seen_key"`
	UserID    int    `json:"user_id" gorm:"index:idx_user_access_seen_user_last,priority:1;not null;default:0"`
	APIKeyID  int    `json:"api_key_id" gorm:"not null;default:0"`
	IP        string `json:"ip" gorm:"size:128"`
	UserAgent string `json:"user_agent" gorm:"size:512"`
	FirstSeen int64  `json:"first_seen" gorm:"not null;default:0"`
	LastSeen  int64  `json:"last_seen" gorm:"index:idx_user_access_seen_user_last,priority:2;index:idx_user_access_seen_last;not null;default:0"`
	Requests  int64  `json:"requests" gorm:"not null;default:0"`
}

func (UserAccessSeen) TableName() string { return "user_access_seen" }

// UserAccessSeenKey 是 UserAccessSeen.SeenKey 的算法；记录、合并、回填都走这一个。
func UserAccessSeenKey(userID, apiKeyID int, ip, ua string) string {
	sum := sha1.Sum([]byte(strconv.Itoa(userID) + "|" + strconv.Itoa(apiKeyID) + "|" + ip + "|" + ua))
	return hex.EncodeToString(sum[:])
}

// 异常类型。判定规则见 op.userAccessAlerts。
const (
	UserAccessAlertManyIPs   = "many_ips"   // 窗口内用了太多个不同的 IP
	UserAccessAlertNewIP     = "new_ip"     // 冒出一个从没见过的 IP，同时旧 IP 也还在用
	UserAccessAlertNewClient = "new_client" // 冒出一个从没见过的客户端，同时旧客户端也还在用
)

// UserAccessAlert 是一条「异常切换」提示。Value 是新冒出来的那个 IP / 客户端 UA；
// Others 是同一窗口里同时在用的其它 IP / 客户端 UA（many_ips 时 Others 是全部 IP）。
type UserAccessAlert struct {
	Kind      string   `json:"kind"`
	WindowSec int64    `json:"window_sec"`
	Count     int      `json:"count"`
	Value     string   `json:"value,omitempty"`
	Others    []string `json:"others,omitempty"`
	At        int64    `json:"at"`
}

// UserAccessLast 是这个用户最近一次请求的来源。
type UserAccessLast struct {
	IP         string `json:"ip"`
	UserAgent  string `json:"user_agent"`
	APIKeyID   int    `json:"api_key_id"`
	APIKeyName string `json:"api_key_name,omitempty"`
	At         int64  `json:"at"`
}

// UserAccessSummary 是用户列表每一行要的那一点：最近一次 + 异常提示。
type UserAccessSummary struct {
	UserID int               `json:"user_id"`
	Last   UserAccessLast    `json:"last"`
	Alerts []UserAccessAlert `json:"alerts"`
}

// UserAccessKeyUse 是某个 IP / 客户端下面用过的一把 key。
type UserAccessKeyUse struct {
	APIKeyID   int    `json:"api_key_id"`
	APIKeyName string `json:"api_key_name,omitempty"`
	Requests   int64  `json:"requests"`
	LastSeen   int64  `json:"last_seen"`
}

// UserAccessGroup 是抽屉里的一行：一个 IP（或一个客户端）合计用了多少次、最近什么时候、走的哪些 key。
// 客户端按 UA 原文分组（展示时前端再把 UA 映射成名字）；Values 在 IP 分组时列出这个 IP 用过的 UA，
// 在客户端分组时列出这个客户端来过的 IP。
type UserAccessGroup struct {
	Value     string             `json:"value"`
	FirstSeen int64              `json:"first_seen"`
	LastSeen  int64              `json:"last_seen"`
	Requests  int64              `json:"requests"`
	Keys      []UserAccessKeyUse `json:"keys"`
	Values    []string           `json:"values,omitempty"`
}

// UserAccessDetail 是 GET /api/v1/user/access/detail 的结果。
type UserAccessDetail struct {
	UserID        int               `json:"user_id"`
	WindowSec     int64             `json:"window_sec"`
	ManyIPsLimit  int               `json:"many_ips_limit"`
	KeepDays      int               `json:"keep_days"`
	Last          *UserAccessLast   `json:"last,omitempty"`
	Alerts        []UserAccessAlert `json:"alerts"`
	IPs           []UserAccessGroup `json:"ips"`
	Clients       []UserAccessGroup `json:"clients"`
	TotalRequests int64             `json:"total_requests"`
}
