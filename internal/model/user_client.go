package model

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
)

// UserClientFingerprint 是「这个用户 / 这把 Key 用什么客户端来调用」的指纹账：
// 每个 (用户, API Key, 指纹) 一行，记首次 / 最近出现时间、请求数和这个指纹的全部特征。
//
// 和 user_access_seen 的分工：那张表按 (IP, UA) 记来源、算「异常切换」提示；这张表按
// 客户端指纹（头名顺序 + UA + 稳定的身份头 + 反代转交的 JA3/JA4）记「到底是什么客户端」，
// 同一个指纹换 IP 不新开行（只更新最近 IP）。两张表都只来自入站请求，纯观测，不参与
// 路由 / 计费，出站请求一个字节都不碰。
//
// 不存秘密：Authorization / x-api-key / Cookie 等只记头名（在 HeaderNames 里），值不存；
// Notable 只放白名单里的非秘密头的值（见 telemetry.notableHeader）。
//
// 体量有上限：按 SeenKey 去重；Notable / HeaderNames 截断；90 天没再出现的删掉；
// 每个 (用户, Key) 最多留 UserClientPerKeyMax 个指纹（多出来的删最旧的）。
type UserClientFingerprint struct {
	ID            int64  `json:"id" gorm:"primaryKey"`
	SeenKey       string `json:"-" gorm:"size:40;uniqueIndex:idx_user_client_fp_key"`
	UserID        int    `json:"user_id" gorm:"index:idx_user_client_fp_user_last,priority:1;not null;default:0"`
	APIKeyID      int    `json:"api_key_id" gorm:"index:idx_user_client_fp_apikey;not null;default:0"`
	FPHash        string `json:"fp_hash" gorm:"size:32"`
	ClientName    string `json:"client_name" gorm:"size:64"`
	ClientVersion string `json:"client_version" gorm:"size:64"`
	UserAgent     string `json:"user_agent" gorm:"size:512"`
	HeaderNames   string `json:"header_names" gorm:"type:text"`
	HeaderOrder   bool   `json:"header_order" gorm:"not null;default:false"`
	Notable       string `json:"notable" gorm:"type:text"`
	HTTPVersion   string `json:"http_version" gorm:"size:16"`
	JA3           string `json:"ja3" gorm:"size:64"`
	JA4           string `json:"ja4" gorm:"size:96"`
	JA4H          string `json:"ja4h" gorm:"size:96"`
	ViaProxy      bool   `json:"via_proxy" gorm:"not null;default:false"`
	Country       string `json:"country" gorm:"size:8"`
	LastIP        string `json:"last_ip" gorm:"size:128"`
	LastPath      string `json:"last_path" gorm:"size:128"`
	FirstSeen     int64  `json:"first_seen" gorm:"not null;default:0"`
	LastSeen      int64  `json:"last_seen" gorm:"index:idx_user_client_fp_user_last,priority:2;index:idx_user_client_fp_last;not null;default:0"`
	Requests      int64  `json:"requests" gorm:"not null;default:0"`
}

func (UserClientFingerprint) TableName() string { return "user_client_fingerprints" }

// UserClientPerKeyMax：每个 (用户, Key) 最多留几个指纹。
const UserClientPerKeyMax = 100

// UserClientSeenKey 是 UserClientFingerprint.SeenKey 的算法。
func UserClientSeenKey(userID, apiKeyID int, fpHash string) string {
	sum := sha1.Sum([]byte(strconv.Itoa(userID) + "|" + strconv.Itoa(apiKeyID) + "|" + fpHash))
	return hex.EncodeToString(sum[:])
}

// UserClientKeyInfo 是抽屉里「按 Key 看」的一个选项。
type UserClientKeyInfo struct {
	APIKeyID     int    `json:"api_key_id"`
	APIKeyName   string `json:"api_key_name,omitempty"`
	Fingerprints int    `json:"fingerprints"`
	Requests     int64  `json:"requests"`
	LastSeen     int64  `json:"last_seen"`
}

// UserClientDetail 是 GET /api/v1/user/telemetry 的结果。
//
// TLSTerminated：本服务自己有没有终结 TLS。octopus 只监听明文 HTTP（TLS 在前面的反代 /
// Cloudflare 上终结），所以恒为 false——JA3/JA4 只能由可信反代用头转交。
type UserClientDetail struct {
	UserID        int                     `json:"user_id"`
	APIKeyID      int                     `json:"api_key_id,omitempty"`
	KeepDays      int                     `json:"keep_days"`
	PerKeyMax     int                     `json:"per_key_max"`
	TLSTerminated bool                    `json:"tls_terminated"`
	Keys          []UserClientKeyInfo     `json:"keys"`
	Fingerprints  []UserClientFingerprint `json:"fingerprints"`
}
