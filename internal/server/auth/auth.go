package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"math/big"
	"os"
	"strings"
	"time"

	"github.com/bestruirui/octopus/internal/conf"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/op"
	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	UserID   int            `json:"user_id"`
	Username string         `json:"username"`
	Role     model.UserRole `json:"role"`
	jwt.RegisteredClaims
}

func GenerateJWTToken(user model.User, expiresMin int) (string, string, error) {
	now := time.Now()
	claims := &Claims{
		UserID:   user.ID,
		Username: user.Username,
		Role:     user.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			Issuer:    conf.APP_NAME,
			Subject:   fmt.Sprintf("%d", user.ID),
		},
	}
	if expiresMin == 0 {
		claims.ExpiresAt = jwt.NewNumericDate(now.Add(15 * time.Minute))
	} else if expiresMin > 0 {
		claims.ExpiresAt = jwt.NewNumericDate(now.Add(time.Duration(expiresMin) * time.Minute))
	} else if expiresMin == -1 {
		claims.ExpiresAt = jwt.NewNumericDate(now.Add(30 * 24 * time.Hour))
	}
	secret := userTokenSecret(user)
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		return "", "", err
	}
	return token, claims.ExpiresAt.Format(time.RFC3339), nil
}

func VerifyJWTToken(token string) (model.User, bool) {
	var unverified Claims
	_, _, err := jwt.NewParser().ParseUnverified(token, &unverified)
	if err != nil || unverified.UserID <= 0 {
		return model.User{}, false
	}

	user, err := op.UserGet(unverified.UserID)
	if err != nil || !user.IsActive() {
		return model.User{}, false
	}

	claims := &Claims{}
	jwtToken, err := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (interface{}, error) {
		return []byte(userTokenSecret(user)), nil
	})
	if err != nil || !jwtToken.Valid {
		return model.User{}, false
	}
	if claims.UserID != user.ID {
		return model.User{}, false
	}
	return user, true
}

func userTokenSecret(user model.User) string {
	return user.Username + user.Password
}

// adminAccessTokenMinLen refuses a configured token shorter than this so a stray or
// weak value cannot silently become an admin bypass.
const adminAccessTokenMinLen = 24

// VerifyAdminAccessToken validates a long-lived admin access token — an alternative
// to a login JWT for automation/CLI. On a match it returns the real default admin
// user so downstream handlers see a genuine user ID (not a synthetic one). The token
// comes from SettingKeyAdminToken, falling back to the <APP>_ADMIN_TOKEN env var so a
// fresh public-repo deployment can inject it without hardcoding. Security invariants:
//   - empty configured token = feature DISABLED, never a backdoor;
//   - a token shorter than adminAccessTokenMinLen is refused;
//   - constant-time comparison avoids leaking the token via timing.
func VerifyAdminAccessToken(token string) (model.User, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return model.User{}, false
	}
	configured, err := op.SettingGetString(model.SettingKeyAdminToken)
	if err != nil {
		configured = ""
	}
	configured = strings.TrimSpace(configured)
	if configured == "" {
		configured = strings.TrimSpace(os.Getenv(strings.ToUpper(conf.APP_NAME) + "_ADMIN_TOKEN"))
	}
	if len(configured) < adminAccessTokenMinLen {
		return model.User{}, false
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(configured)) != 1 {
		return model.User{}, false
	}
	admin, err := op.UserDefaultAdmin(context.Background())
	if err != nil {
		return model.User{}, false
	}
	return admin, true
}

func GenerateAPIKey() string {
	const keyChars = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	b := make([]byte, 48)
	maxI := big.NewInt(int64(len(keyChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, maxI)
		if err != nil {
			return ""
		}
		b[i] = keyChars[n.Int64()]
	}
	return "sk-" + conf.APP_NAME + "-" + string(b)
}
