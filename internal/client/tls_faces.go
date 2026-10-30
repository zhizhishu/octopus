package client

import (
	"context"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// Per-CLI-family TLS ClientHello faces.
//
// Both specs below are byte-faithful transcriptions of ClientHellos captured on
// the wire from the genuine CLIs (2026-10-13, direct legs against a capture
// proxy; see _artifacts/berobox/codexshape-h2/captures/ and
// parse_clienthello.py for the decoder):
//
//   - codex:  codex CLI 0.161.0 (linux-x64 musl, rustls) — 10 cipher suites,
//     ALPN h2+http/1.1, 10 extensions, X25519MLKEM768 + X25519 key shares,
//     1466 bytes total. The API connection is the ALPN-carrying one; the CLI's
//     no-ALPN network-probe connection is NOT mimicked (oct only makes API
//     calls).
//   - claude: Claude Code 2.1.296 (linux-x64, Node/OpenSSL) — 30 cipher suites,
//     NO ALPN (Node speaks HTTP/1.1 over TLS), 10 extensions, X25519MLKEM768 +
//     X25519 key shares, 1522 bytes total.
//
// Extension order, cipher order, curve order and signature-algorithm order are
// all part of the fingerprint and are kept exactly as captured. Values are raw
// hex with names in comments so the wire bytes stay auditable against the
// capture. SNI is first in both lists (both stacks prepend server_name for
// hostname targets); uTLS omits the extension entirely for IP targets, which is
// also what the real CLIs do (the captures, taken against an IP, show no SNI).
//
// Randoms differ per connection by design (client random, session id, key
// shares) — uTLS generates fresh ones, matching how the real CLI behaves.
//
// The reference project CLIProxyAPI ships the same approach for Anthropic
// (internal/runtime/executor/helps/utls_client.go, claudeCodeTLSClientHelloSpec)
// with a macOS capture; ours is transcribed from our own linux capture of the
// current CLI version instead.

// codexRustlsClientHelloSpec reproduces the genuine codex CLI's rustls
// ClientHello for /responses traffic.
func codexRustlsClientHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1301, // TLS_AES_128_GCM_SHA256
			0x1303, // TLS_CHACHA20_POLY1305_SHA256
			0xc02c, // ECDHE-ECDSA-AES256-GCM-SHA384
			0xc02b, // ECDHE-ECDSA-AES128-GCM-SHA256
			0xcca9, // ECDHE-ECDSA-CHACHA20-POLY1305
			0xc030, // ECDHE-RSA-AES256-GCM-SHA384
			0xc02f, // ECDHE-RSA-AES128-GCM-SHA256
			0xcca8, // ECDHE-RSA-CHACHA20-POLY1305
			0x00ff, // TLS_EMPTY_RENEGOTIATION_INFO_SCSV
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0503, // ecdsa_secp384r1_sha384
				0x0403, // ecdsa_secp256r1_sha256
				0x0603, // ecdsa_secp521r1_sha512
				0x0807, // ed25519
				0x0806, // rsa_pss_rsae_sha512
				0x0805, // rsa_pss_rsae_sha384
				0x0804, // rsa_pss_rsae_sha256
				0x0601, // rsa_pkcs1_sha512
				0x0501, // rsa_pkcs1_sha384
				0x0401, // rsa_pkcs1_sha256
				0x0904, // post-quantum signature scheme (ML-DSA family)
				0x0905, // post-quantum signature scheme (ML-DSA family)
				0x0906, // post-quantum signature scheme (ML-DSA family)
			}},
			&utls.SessionTicketExtension{},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519MLKEM768, // 0x11ec
				utls.X25519,         // 0x001d
				utls.CurveP256,      // 0x0017
				utls.CurveP384,      // 0x0018
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.StatusRequestExtension{},
		},
	}
}

// claudeCodeNodeClientHelloSpec reproduces the genuine Claude Code CLI's
// Node/OpenSSL ClientHello for /messages traffic. No ALPN: the CLI speaks
// HTTP/1.1 over TLS, so this face never negotiates h2.
func claudeCodeNodeClientHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			// 🔴 这三个注释曾按"顺序猜测"标错：0x1301 被标成 CHACHA20、
			// 0x1303 被标成 AES_128。按 RFC 8446 的真实映射是
			//   0x1301 = TLS_AES_128_GCM_SHA256
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1303, // TLS_CHACHA20_POLY1305_SHA256
			0x1301, // TLS_AES_128_GCM_SHA256
			0xc02c, // ECDHE-ECDSA-AES256-GCM-SHA384
			0xc030, // ECDHE-RSA-AES256-GCM-SHA384
			0x009f, // DHE-RSA-AES256-GCM-SHA384
			0xcca9, // ECDHE-ECDSA-CHACHA20-POLY1305
			0xcca8, // ECDHE-RSA-CHACHA20-POLY1305
			0xccaa, // ECDHE-ECDSA-CHACHA20-POLY1305 (old draft codepoint)
			0xc02b, // ECDHE-ECDSA-AES128-GCM-SHA256
			0xc02f, // ECDHE-RSA-AES128-GCM-SHA256
			0x009e, // DHE-RSA-AES128-GCM-SHA256
			0xc024, // ECDHE-ECDSA-AES256-SHA384
			0xc028, // ECDHE-RSA-AES256-SHA384
			0x006b, // DHE-RSA-AES256-SHA256
			0xc023, // ECDHE-ECDSA-AES128-SHA256
			0xc027, // ECDHE-RSA-AES128-SHA256
			0x0067, // DHE-RSA-AES128-SHA256
			0xc00a, // ECDHE-ECDSA-AES256-SHA
			0xc014, // ECDHE-RSA-AES256-SHA
			0x0039, // DHE-RSA-AES256-SHA
			0xc009, // ECDHE-ECDSA-AES128-SHA
			0xc013, // ECDHE-RSA-AES128-SHA
			0x0033, // DHE-RSA-AES128-SHA
			0x009d, // RSA-AES256-GCM-SHA384
			0x009c, // RSA-AES128-GCM-SHA256
			0x003d, // RSA-AES256-SHA256
			0x003c, // RSA-AES128-SHA256
			0x0035, // RSA-AES256-SHA
			0x002f, // RSA-AES128-SHA
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519MLKEM768,  // 0x11ec
				utls.X25519,          // 0x001d
				utls.CurveP256,       // 0x0017
				utls.CurveID(0x001e), // x448
				utls.CurveP384,       // 0x0018
				utls.CurveP521,       // 0x0019
				utls.CurveID(0x0100), // ffdhe2048
				utls.CurveID(0x0101), // ffdhe3072
			}},
			&utls.SessionTicketExtension{},
			// encrypt_then_mac (type 22, empty body): uTLS 1.8.2 has no typed
			// extension for it, so emit it generically — byte-identical on the wire.
			&utls.GenericExtension{Id: 22},
			&utls.ExtendedMasterSecretExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0905, // post-quantum signature scheme (ML-DSA family)
				0x0906, // post-quantum signature scheme (ML-DSA family)
				0x0904, // post-quantum signature scheme (ML-DSA family)
				0x0403, // ecdsa_secp256r1_sha256
				0x0503, // ecdsa_secp384r1_sha384
				0x0603, // ecdsa_secp521r1_sha512
				0x0807, // ed25519
				0x0808, // ed448
				0x081a, // ecdsa_brainpoolP256r1_sha256
				0x081b, // ecdsa_brainpoolP384r1_sha384
				0x081c, // ecdsa_brainpoolP512r1_sha512
				0x0809, // rsa_pss_pss_sha256
				0x080a, // rsa_pss_pss_sha384
				0x080b, // rsa_pss_pss_sha512
				0x0804, // rsa_pss_rsae_sha256
				0x0805, // rsa_pss_rsae_sha384
				0x0806, // rsa_pss_rsae_sha512
				0x0401, // rsa_pkcs1_sha256
				0x0501, // rsa_pkcs1_sha384
				0x0601, // rsa_pkcs1_sha512
				0x0303, // ecdsa_sha224
				0x0301, // rsa_pkcs1_sha224
				0x0302, // dsa_sha224
				0x0402, // dsa_sha256
				0x0502, // dsa_sha384
				0x0602, // dsa_sha512
			}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519MLKEM768}, {Group: utls.X25519}}},
		},
	}
}

// tlsFaceFamily labels which ClientHello face a request's upstream connection
// should wear. It follows the same path routing as the header-order tables in
// header_order.go: /responses is codex CLI traffic, /messages is Claude Code
// traffic, anything else keeps the Chrome face (non-CLI upstreams are not
// mimicked — see the shape rules).
const (
	tlsFaceCodex  = "codex"
	tlsFaceClaude = "claude"
	tlsFaceChrome = "chrome"
)

// clientHelloSpecForPath routes a request to its CLI-family ClientHello spec.
// A nil spec means "use utls.HelloChrome_Auto" (the pre-existing default face
// for non-CLI paths). The returned spec may be pinned by long-lived callers
// (the per-family h1 transport closure), so dialUTLS clones it per dial —
// see specForDial.
func clientHelloSpecForPath(path string) (spec *utls.ClientHelloSpec, family string) {
	p := strings.ToLower(path)
	switch {
	case strings.Contains(p, "/responses"):
		return codexRustlsClientHelloSpec(), tlsFaceCodex
	case strings.Contains(p, "/messages"):
		return claudeCodeNodeClientHelloSpec(), tlsFaceClaude
	default:
		return nil, tlsFaceChrome
	}
}

// tlsFaceOverrideKeyType keys the per-request TLS face override in the request
// context. The relay sets it when a Chat channel's cloak shape explicitly
// dresses the outbound as claude/codex, so the ClientHello face follows the
// header identity instead of the URL path.
type tlsFaceOverrideKeyType struct{}

var tlsFaceOverrideKey tlsFaceOverrideKeyType

// WithTLSFace pins the CLI-family ClientHello face for a request regardless of
// its URL path. Used by the relay when a Chat channel's cloak shape explicitly
// dresses the outbound as claude/codex; an empty family clears the override.
func WithTLSFace(ctx context.Context, family string) context.Context {
	return context.WithValue(ctx, tlsFaceOverrideKey, family)
}

func tlsFaceFromContext(ctx context.Context) string {
	v, _ := ctx.Value(tlsFaceOverrideKey).(string)
	return v
}

// tlsFamilyForDial resolves the CLI face one outbound dial must wear.
//
// This is the single source of truth shared by the uTLS ClientHello path and the
// plaintext HTTP/1.1 header-order path. An explicit override on ctx — the relay's
// channelTLSFace, stored by WithTLSFace — wins over the URL path; otherwise the
// historical path rule applies (/responses codex, /messages claude, anything else
// chrome). Callers that need a per-connection pool key use the returned family:
// transports are cached process-wide, so the face cannot be closed over from the
// first request that happened to dial.
func tlsFamilyForDial(ctx context.Context, path string) (spec *utls.ClientHelloSpec, family string) {
	spec, family = clientHelloSpecForPath(path)
	if ctx == nil {
		return spec, family
	}
	if override := tlsFaceFromContext(ctx); override != "" {
		spec, family = clientHelloSpecForFamily(override)
	}
	return spec, family
}

// TLSFaceFromContext exposes the per-request face to packages that must choose a
// transport before the request is built (the helper picks the HTTP client, and a
// face-carrying channel needs the uTLS transport regardless of its proxy).
func TLSFaceFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return tlsFaceFromContext(ctx)
}

// clientHelloSpecForFamily maps an explicit face family to its ClientHello spec.
// An unknown/empty family (or "chrome") keeps the pre-existing Chrome default, so
// an unknown override value can never select an undefined face.
func clientHelloSpecForFamily(family string) (spec *utls.ClientHelloSpec, fam string) {
	switch family {
	case tlsFaceCodex:
		return codexRustlsClientHelloSpec(), tlsFaceCodex
	case tlsFaceClaude:
		return claudeCodeNodeClientHelloSpec(), tlsFaceClaude
	default:
		return nil, tlsFaceChrome
	}
}

// specForDial returns a per-dial copy of a face spec. utls's ApplyPreset
// copies the extension slice by pointer and then fills KeyShareExtension.Data
// in place (and ApplyPreset/dialUTLS write SNIExtension.ServerName the same
// way), so a spec reused across connections would carry the first
// connection's key-share bytes forever — later connections send that stale
// public key with no matching private key and the TLS 1.3 handshake aborts
// with internal_error. Clearing Data makes utls generate fresh key material
// per connection, exactly like the genuine CLIs; the copied SNI extension
// keeps per-connection ServerName fills off the shared spec. The wire shape
// is unchanged (groups, lengths and field order all come from the same
// literals).
func specForDial(spec *utls.ClientHelloSpec) *utls.ClientHelloSpec {
	if spec == nil {
		return nil
	}
	exts := make([]utls.TLSExtension, len(spec.Extensions))
	copy(exts, spec.Extensions)
	for i, ext := range exts {
		switch e := ext.(type) {
		case *utls.KeyShareExtension:
			shares := make([]utls.KeyShare, len(e.KeyShares))
			for j, ks := range e.KeyShares {
				shares[j] = utls.KeyShare{Group: ks.Group}
			}
			exts[i] = &utls.KeyShareExtension{KeyShares: shares}
		case *utls.SNIExtension:
			cp := *e
			exts[i] = &cp
		}
	}
	suites := make([]uint16, len(spec.CipherSuites))
	copy(suites, spec.CipherSuites)
	methods := make([]uint8, len(spec.CompressionMethods))
	copy(methods, spec.CompressionMethods)
	return &utls.ClientHelloSpec{
		CipherSuites:       suites,
		CompressionMethods: methods,
		Extensions:         exts,
		TLSVersMin:         spec.TLSVersMin,
		TLSVersMax:         spec.TLSVersMax,
	}
}
