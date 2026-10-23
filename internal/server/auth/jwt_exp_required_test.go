package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/zhizhishu/octopus/internal/db"
	dbmodel "github.com/zhizhishu/octopus/internal/model"
	"github.com/zhizhishu/octopus/internal/op"
)

// setupAuthTestDB 建库并落一个真实用户。
// 🔴 VerifyJWTToken 的第一步是 `op.UserGet(unverified.UserID)` —— 库是空的就直接
// 返回 false。不建库的"无 exp 被拒"测试会**假绿**：它拒的是"查不到用户"，
// 与 exp 检查毫无关系。这条坑是实测踩出来的（第一版测试就是这么假绿的）。
func setupAuthTestDB(t *testing.T, username string) dbmodel.User {
	t.Helper()
	if err := db.InitDB("sqlite", filepath.Join(t.TempDir(), "auth_test.db"), false); err != nil {
		t.Fatalf("init db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := op.InitCache(); err != nil {
		t.Fatalf("init cache: %v", err)
	}
	user, err := op.UserCreate(dbmodel.UserCreateRequest{
		Username: username,
		Password: "password",
		Role:     dbmodel.UserRoleUser,
		Status:   dbmodel.UserStatusActive,
	}, context.Background())
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	// 重新读一次，确保拿到的是落库后的真实字段值（userTokenSecret 用的是
	// username+password，两者都必须与库里一致）。
	reloaded, err := op.UserGet(user.ID)
	if err != nil {
		t.Fatalf("reload user: %v", err)
	}
	return reloaded
}

// 没有 exp 的 token 必须验不过。
//
// 缺陷形态：jwt/v5 的默认校验器**只在 exp 存在时**才检查它。一个不带 exp 的 token
// 于是永久有效 —— 签发侧的钳制（clamp）拦不住它，因为这种 token 可能来自
// 旧版本签发的存量、被篡改的载荷、或任何手工构造。签发侧修好后校验侧仍需独立兜住，
// 否则"把有效期卡在 30 天"只对**新**token 有效，旧的永生 token 依旧能用。
//
// 这条走 VerifyJWTToken 真实路径（不是只测库函数）：去掉 jwt.WithExpirationRequired()
// 即变红。
func TestVerifyJWTRejectsTokenWithoutExpiry(t *testing.T) {
	user := setupAuthTestDB(t, "no-exp-user")

	// 手工构造一个**不带 ExpiresAt** 的 token，签名密钥与生产一致。
	claims := &Claims{
		UserID:   user.ID,
		Username: user.Username,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).
		SignedString([]byte(userTokenSecret(user)))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, ok := VerifyJWTToken(token); ok {
		t.Fatal("无 exp 的 token 通过了校验：它会永久有效，" +
			"签发侧的 30 天上限对这类 token 完全无效")
	}
}

// 正常签发的 token 必须照常通过（防误伤：别把 WithExpirationRequired 加过头）。
func TestVerifyJWTAcceptsFreshlyIssuedToken(t *testing.T) {
	user := setupAuthTestDB(t, "normal-user")
	token, _, err := GenerateJWTToken(user, 0)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if _, ok := VerifyJWTToken(token); !ok {
		t.Fatal("正常签发的 token 被拒了")
	}
}

// 超长有效期必须被钳到上限。
func TestGenerateJWTTokenClampsHugeTTL(t *testing.T) {
	user := dbmodel.User{ID: 44, Username: "huge-ttl", Role: dbmodel.UserRoleUser}
	// 一年（分钟）。旧实现会原样签发成一年有效。
	token, _, err := GenerateJWTToken(user, 365*24*60)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	parsed, err := jwt.ParseWithClaims(token, &Claims{}, func(*jwt.Token) (interface{}, error) {
		return []byte(userTokenSecret(user)), nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	claims, ok := parsed.Claims.(*Claims)
	if !ok || claims.ExpiresAt == nil {
		t.Fatal("token 没有 exp")
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl > maxJWTTTL+time.Minute {
		t.Fatalf("有效期 %s 超过了上限 %s：请求体可以签出长期有效的 token", ttl, maxJWTTTL)
	}
}
