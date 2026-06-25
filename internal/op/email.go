package op

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const emailCodeTTL = 10 * time.Minute

const (
	emailRateLimitPerEmail = 60 * time.Second
	emailRateLimitPerIP    = 20 * time.Second
)

type emailCodeEntry struct {
	code      string
	expiresAt time.Time
}

var (
	emailCodeMu    sync.Mutex
	emailCodeStore = make(map[string]emailCodeEntry)
)

var (
	emailRateMu      sync.Mutex
	emailRateByEmail = make(map[string]time.Time)
	emailRateByIP    = make(map[string]time.Time)
)

// storeEmailCode records a verification code for the given email with a fixed
// TTL, opportunistically pruning expired entries while the lock is held.
func storeEmailCode(email, code string) {
	emailCodeMu.Lock()
	defer emailCodeMu.Unlock()
	now := time.Now()
	for k, v := range emailCodeStore {
		if now.After(v.expiresAt) {
			delete(emailCodeStore, k)
		}
	}
	emailCodeStore[email] = emailCodeEntry{
		code:      code,
		expiresAt: now.Add(emailCodeTTL),
	}
}

// VerifyEmailCode reports whether an unexpired stored code matches the supplied
// code. It does not consume the code.
func VerifyEmailCode(email, code string) bool {
	emailCodeMu.Lock()
	defer emailCodeMu.Unlock()
	entry, ok := emailCodeStore[email]
	if !ok {
		return false
	}
	if time.Now().After(entry.expiresAt) {
		delete(emailCodeStore, email)
		return false
	}
	return entry.code == code
}

// ConsumeEmailCode removes any stored code for the given email.
func ConsumeEmailCode(email string) {
	emailCodeMu.Lock()
	defer emailCodeMu.Unlock()
	delete(emailCodeStore, email)
}

// generateEmailCode returns a random 6-digit numeric code.
func generateEmailCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1000000))
	if err != nil {
		return "000000"
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// validateEmail reports whether the address is a syntactically valid single
// email address with a domain and no embedded whitespace or control characters.
func validateEmail(email string) bool {
	if email == "" {
		return false
	}
	if strings.ContainsAny(email, "\r\n ") {
		return false
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return false
	}
	at := strings.LastIndex(addr.Address, "@")
	if at <= 0 || at == len(addr.Address)-1 {
		return false
	}
	return true
}

// emailRateLimitAllow enforces a minimum gap between code requests per email and
// per IP. It records the timestamps only when the request is allowed.
func emailRateLimitAllow(email, ip string) error {
	emailRateMu.Lock()
	defer emailRateMu.Unlock()
	now := time.Now()
	if last, ok := emailRateByEmail[email]; ok && now.Sub(last) < emailRateLimitPerEmail {
		return fmt.Errorf("please wait a moment before requesting another code")
	}
	if ip != "" {
		if last, ok := emailRateByIP[ip]; ok && now.Sub(last) < emailRateLimitPerIP {
			return fmt.Errorf("please wait a moment before requesting another code")
		}
	}
	emailRateByEmail[email] = now
	if ip != "" {
		emailRateByIP[ip] = now
	}
	return nil
}

type smtpConfig struct {
	host     string
	port     int
	user     string
	password string
	from     string
	fromName string
	ssl      bool
}

// loadSMTPConfig reads the SMTP settings from the settings cache.
func loadSMTPConfig() (smtpConfig, error) {
	host, _ := SettingGetString(model.SettingKeyEmailSMTPHost)
	host = strings.TrimSpace(host)
	port, err := SettingGetInt(model.SettingKeyEmailSMTPPort)
	if err != nil || port < 1 || port > 65535 {
		return smtpConfig{}, fmt.Errorf("email service is not configured")
	}
	user, _ := SettingGetString(model.SettingKeyEmailSMTPUser)
	user = strings.TrimSpace(user)
	password, _ := SettingGetString(model.SettingKeyEmailSMTPPassword)
	from, _ := SettingGetString(model.SettingKeyEmailSMTPFrom)
	from = strings.TrimSpace(from)
	fromName, _ := SettingGetString(model.SettingKeyEmailSMTPFromName)
	fromName = strings.TrimSpace(fromName)
	ssl, _ := SettingGetBool(model.SettingKeyEmailSMTPSSL)

	if host == "" {
		return smtpConfig{}, fmt.Errorf("email service is not configured")
	}
	if from == "" && user == "" {
		return smtpConfig{}, fmt.Errorf("email service is not configured")
	}
	return smtpConfig{
		host:     host,
		port:     port,
		user:     user,
		password: password,
		from:     from,
		fromName: fromName,
		ssl:      ssl,
	}, nil
}

// emailProvider returns the configured email transport, defaulting to "smtp"
// when the setting is empty, unreadable, or anything other than "http".
func emailProvider() string {
	provider, err := SettingGetString(model.SettingKeyEmailProvider)
	if err != nil {
		return "smtp"
	}
	if strings.ToLower(strings.TrimSpace(provider)) == "http" {
		return "http"
	}
	return "smtp"
}

type httpEmailConfig struct {
	baseURL   string
	from      string
	adminAuth string
	siteAuth  string
}

// loadHTTPEmailConfig reads the HTTP email provider settings from the cache.
// baseURL and from are required; adminAuth/siteAuth may be empty for services
// without authentication.
func loadHTTPEmailConfig() (httpEmailConfig, error) {
	baseURL, _ := SettingGetString(model.SettingKeyEmailHTTPBaseURL)
	baseURL = strings.TrimSpace(baseURL)
	from, _ := SettingGetString(model.SettingKeyEmailHTTPFrom)
	from = strings.TrimSpace(from)
	adminAuth, _ := SettingGetString(model.SettingKeyEmailHTTPAdminAuth)
	siteAuth, _ := SettingGetString(model.SettingKeyEmailHTTPSiteAuth)

	if baseURL == "" || from == "" {
		return httpEmailConfig{}, fmt.Errorf("email service is not configured")
	}
	return httpEmailConfig{
		baseURL:   baseURL,
		from:      from,
		adminAuth: adminAuth,
		siteAuth:  siteAuth,
	}, nil
}

// buildHTTPEmailPayload renders the JSON body sent to the HTTP email provider.
func buildHTTPEmailPayload(from, to, subject, html string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"from":    from,
		"to_mail": to,
		"subject": subject,
		"content": html,
		"is_html": true,
	})
}

// httpEmailClient is a proxy-bypassing HTTP client for the email provider. It
// mirrors client.GetHTTPClientSystemProxy(false) (proxy disabled) but is built
// locally to avoid an import cycle (internal/client imports internal/op).
var (
	httpEmailClientOnce sync.Once
	httpEmailClient     *http.Client
)

func getHTTPEmailClient() *http.Client {
	httpEmailClientOnce.Do(func() {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			httpEmailClient = &http.Client{}
			return
		}
		cloned := transport.Clone()
		cloned.Proxy = nil
		httpEmailClient = &http.Client{Transport: cloned}
	})
	return httpEmailClient
}

// sendMailHTTP delivers an HTML message via the HTTP email provider's
// POST {base}/admin/send_mail endpoint. It returns a generic error on failure
// and never includes auth header values or the full response body, to avoid
// leaking credentials.
func sendMailHTTP(cfg httpEmailConfig, to, subject, htmlBody string) error {
	payload, err := buildHTTPEmailPayload(cfg.from, to, subject, htmlBody)
	if err != nil {
		return fmt.Errorf("email http provider rejected the message")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := strings.TrimRight(cfg.baseURL, "/") + "/admin/send_mail"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("email http provider rejected the message")
	}
	req.Header.Set("content-type", "application/json")
	if cfg.adminAuth != "" {
		req.Header.Set("x-admin-auth", cfg.adminAuth)
	}
	if cfg.siteAuth != "" {
		req.Header.Set("x-custom-auth", cfg.siteAuth)
	}

	resp, err := getHTTPEmailClient().Do(req)
	if err != nil {
		log.Errorf("email http provider request failed: %v", err)
		return fmt.Errorf("email http provider rejected the message")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !strings.Contains(string(body), "sent") {
		log.Errorf("email http provider rejected the message: status %d", resp.StatusCode)
		return fmt.Errorf("email http provider rejected the message")
	}
	return nil
}

func encodeMIMEHeader(value string) string {
	return "=?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

// sendMail builds an RFC5322 HTML message and delivers it over SMTP. When the
// port is 465 or ssl is requested it uses implicit TLS, otherwise it relies on
// smtp.SendMail which negotiates STARTTLS when offered.
func sendMail(cfg smtpConfig, to, subject, htmlBody string) error {
	from := cfg.from
	if from == "" {
		from = cfg.user
	}
	fromName := cfg.fromName
	if fromName == "" {
		fromName = "Octopus"
	}

	msgIDHost := cfg.host
	if msgIDHost == "" {
		msgIDHost = "octopus.local"
	}

	var b strings.Builder
	b.WriteString("From: " + encodeMIMEHeader(fromName) + " <" + from + ">\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeMIMEHeader(subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: <" + generateEmailCode() + strconv.FormatInt(time.Now().UnixNano(), 10) + "@" + msgIDHost + ">\r\n")
	b.WriteString("\r\n")
	b.WriteString(htmlBody)
	msg := []byte(b.String())

	addr := net.JoinHostPort(cfg.host, strconv.Itoa(cfg.port))

	var auth smtp.Auth
	if cfg.user != "" {
		auth = smtp.PlainAuth("", cfg.user, cfg.password, cfg.host)
	}

	if cfg.port == 465 || cfg.ssl {
		return sendMailImplicitTLS(cfg, addr, auth, from, to, msg)
	}
	return smtp.SendMail(addr, auth, from, []string{to}, msg)
}

func sendMailImplicitTLS(cfg smtpConfig, addr string, auth smtp.Auth, from, to string, msg []byte) error {
	conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: cfg.host})
	if err != nil {
		return err
	}
	client, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer client.Quit()

	if auth != nil {
		if err := client.Auth(auth); err != nil {
			return err
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	if err := client.Rcpt(to); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

// EmailVerificationEnabled reports whether registration email verification is on.
func EmailVerificationEnabled() bool {
	enabled, err := SettingGetBool(model.SettingKeyEmailVerificationEnabled)
	if err != nil {
		return false
	}
	return enabled
}

// UserEmailTaken reports whether any user already owns the given email address.
func UserEmailTaken(email string) (bool, error) {
	var count int64
	if err := db.GetDB().Model(&model.User{}).Where("email = ?", email).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// SendEmailVerificationCode generates, stores, and emails a verification code to
// the supplied address, enforcing the enabled toggle, validation, uniqueness,
// and rate limits.
func SendEmailVerificationCode(email, ip string) error {
	if !EmailVerificationEnabled() {
		return fmt.Errorf("email verification is not enabled")
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if !validateEmail(email) {
		return fmt.Errorf("invalid email address")
	}
	if taken, _ := UserEmailTaken(email); taken {
		return fmt.Errorf("email already registered")
	}
	if err := emailRateLimitAllow(email, ip); err != nil {
		return err
	}

	// Validate the chosen provider's configuration BEFORE storing a code so we
	// never persist a code for a misconfigured server.
	provider := emailProvider()
	var smtpCfg smtpConfig
	var httpCfg httpEmailConfig
	if provider == "http" {
		var err error
		httpCfg, err = loadHTTPEmailConfig()
		if err != nil {
			return err
		}
	} else {
		var err error
		smtpCfg, err = loadSMTPConfig()
		if err != nil {
			return err
		}
	}

	code := generateEmailCode()
	storeEmailCode(email, code)

	fromName := "Octopus"
	if provider != "http" {
		if name := strings.TrimSpace(smtpCfg.fromName); name != "" {
			fromName = name
		}
	}
	subject := fromName + " 邮箱验证码"
	body := fmt.Sprintf(
		"<div style=\"font-family:Arial,sans-serif;font-size:14px;color:#333\">"+
			"<p>您的邮箱验证码是：</p>"+
			"<p style=\"font-size:28px;font-weight:bold;letter-spacing:4px\">%s</p>"+
			"<p>验证码 10 分钟内有效，请勿泄露给他人。</p>"+
			"</div>",
		code,
	)

	var sendErr error
	if provider == "http" {
		sendErr = sendMailHTTP(httpCfg, email, subject, body)
	} else {
		sendErr = sendMail(smtpCfg, email, subject, body)
	}
	if sendErr != nil {
		log.Errorf("failed to send verification email: %v", sendErr)
		return fmt.Errorf("failed to send verification email")
	}
	return nil
}
