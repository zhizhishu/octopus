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
	neturl "net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bestruirui/octopus/internal/db"
	"github.com/bestruirui/octopus/internal/model"
	"github.com/bestruirui/octopus/internal/utils/log"
)

const emailCodeTTL = 10 * time.Minute

// maxEmailCodeAttempts caps the number of wrong guesses against a single issued
// code before the entry is invalidated, defeating online brute force of the
// 6-digit code within its TTL.
const maxEmailCodeAttempts = 5

const (
	emailRateLimitPerEmail = 60 * time.Second
	emailRateLimitPerIP    = 20 * time.Second
)

// maxGlobalSendsPerWindow bounds the total number of verification emails sent
// across all callers within globalSendWindow, limiting email-bombing even when
// the per-IP gate is bypassed via spoofing.
const (
	maxGlobalSendsPerWindow = 60
	globalSendWindow        = 60 * time.Second
)

type emailCodeEntry struct {
	code      string
	expiresAt time.Time
	attempts  int
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

var (
	globalSendMu    sync.Mutex
	globalSendTimes []time.Time
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
		attempts:  0,
	}
}

// VerifyEmailCode reports whether an unexpired stored code matches the supplied
// code. It does not consume the code. Each wrong guess increments a per-entry
// attempt counter; once maxEmailCodeAttempts wrong guesses accumulate the entry
// is invalidated, so an attacker cannot exhaust the 6-digit space online.
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
	if entry.code == code {
		return true
	}
	entry.attempts++
	if entry.attempts >= maxEmailCodeAttempts {
		delete(emailCodeStore, email)
		return false
	}
	emailCodeStore[email] = entry
	return false
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
// per IP. It records the timestamps only when the request is allowed and
// opportunistically prunes entries older than their window so the maps cannot
// grow without bound.
func emailRateLimitAllow(email, ip string) error {
	emailRateMu.Lock()
	defer emailRateMu.Unlock()
	now := time.Now()
	for k, v := range emailRateByEmail {
		if now.Sub(v) >= emailRateLimitPerEmail {
			delete(emailRateByEmail, k)
		}
	}
	for k, v := range emailRateByIP {
		if now.Sub(v) >= emailRateLimitPerIP {
			delete(emailRateByIP, k)
		}
	}
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

// globalSendAllow enforces a global cap on the number of verification emails
// sent within globalSendWindow, regardless of source IP. It prunes timestamps
// older than the window and, when the request is admitted, records the send so
// the budget reflects messages actually about to be delivered. Callers must
// invoke it only once they have decided to send (e.g. after the taken-check
// returns not-taken) so silently-skipped emails do not consume the budget.
func globalSendAllow() error {
	globalSendMu.Lock()
	defer globalSendMu.Unlock()
	now := time.Now()
	kept := globalSendTimes[:0]
	for _, t := range globalSendTimes {
		if now.Sub(t) < globalSendWindow {
			kept = append(kept, t)
		}
	}
	globalSendTimes = kept
	if len(globalSendTimes) >= maxGlobalSendsPerWindow {
		return fmt.Errorf("please wait a moment before requesting another code")
	}
	globalSendTimes = append(globalSendTimes, now)
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

// isDisallowedEmailHostIP reports whether an IP is an SSRF-dangerous target for
// the HTTP email provider. It rejects loopback, link-local (which covers the
// 169.254.169.254 cloud metadata endpoint), unspecified, and multicast
// addresses. Ordinary private LAN ranges (10/8, 172.16/12, 192.168/16) are
// allowed on purpose so admins may self-host the provider on a LAN.
func isDisallowedEmailHostIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}

// checkEmailHTTPTarget resolves the host of the configured base URL and refuses
// the most dangerous SSRF targets. It returns a generic error that never echoes
// the resolved IP.
func checkEmailHTTPTarget(baseURL string) error {
	u, err := neturl.Parse(baseURL)
	if err != nil || u.Host == "" {
		return fmt.Errorf("email http target is not allowed")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("email http target is not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isDisallowedEmailHostIP(ip) {
			return fmt.Errorf("email http target is not allowed")
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return fmt.Errorf("email http target is not allowed")
	}
	for _, ip := range ips {
		if isDisallowedEmailHostIP(ip) {
			return fmt.Errorf("email http target is not allowed")
		}
	}
	return nil
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

// noRedirect blocks all redirects so an open-redirect cannot bounce the request
// (and its auth headers) to a different, attacker-chosen host.
func noRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func getHTTPEmailClient() *http.Client {
	httpEmailClientOnce.Do(func() {
		transport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			httpEmailClient = &http.Client{CheckRedirect: noRedirect}
			return
		}
		cloned := transport.Clone()
		cloned.Proxy = nil
		httpEmailClient = &http.Client{Transport: cloned, CheckRedirect: noRedirect}
	})
	return httpEmailClient
}

// sendMailHTTP delivers an HTML message via the HTTP email provider's
// POST {base}/admin/send_mail endpoint. It returns a generic error on failure
// and never includes auth header values or the full response body, to avoid
// leaking credentials.
func sendMailHTTP(cfg httpEmailConfig, to, subject, htmlBody string) error {
	if err := checkEmailHTTPTarget(cfg.baseURL); err != nil {
		return err
	}

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
// the supplied address, enforcing the enabled toggle, validation, and rate
// limits.
//
// To avoid an account-enumeration oracle, the endpoint behaves identically for
// any syntactically valid address whether or not an account already exists: the
// rate-limit check runs first, and an already-registered email returns nil
// WITHOUT sending an email or storing a code (the handler reports the same
// generic "sent" message either way). Only the enabled-check and validation can
// surface a distinguishable error, neither of which reveals account existence.
func SendEmailVerificationCode(email, ip string) error {
	if !EmailVerificationEnabled() {
		return fmt.Errorf("email verification is not enabled")
	}
	email = strings.TrimSpace(strings.ToLower(email))
	if !validateEmail(email) {
		return fmt.Errorf("invalid email address")
	}
	if err := emailRateLimitAllow(email, ip); err != nil {
		return err
	}
	if taken, _ := UserEmailTaken(email); taken {
		// Silently succeed: do not send or store a code, and do not consume the
		// global send budget, so a registered address is indistinguishable from
		// a fresh one to an unauthenticated caller.
		return nil
	}
	if err := globalSendAllow(); err != nil {
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
