package client

import (
	"context"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// Per-CLI-family TLS ClientHello faces.
//
// Both specs below are byte-faithful transcriptions of ClientHellos captured on
// the wire from the genuine CLIs on their direct, authenticated API legs:
//
//   - codex:  codex CLI (linux-x64 musl, rustls) — 10 cipher suites, ALPN
//     h2+http/1.1, 10 extensions, X25519MLKEM768 + X25519 key shares. The API
//     connection is the ALPN-carrying one; the CLI's no-ALPN network-probe
//     connection is NOT mimicked (oct only makes API calls).
//   - claude: Claude Code 2.1.301 (linux-x64, Node/OpenSSL) — 17 cipher suites,
//     ALPN http/1.1, 12 extensions, X25519MLKEM768 + X25519 key shares. This is
//     the captured *business* (/v1/messages) ClientHello, not the no-ALPN
//     network-probe leg.
//
// Cipher order, curve order and signature-algorithm order are part of the
// fingerprint and are kept exactly as captured; values are raw hex with names in
// comments so the wire bytes stay auditable. The claude face also keeps its
// captured extension order fixed. The codex face instead draws a fresh u16
// order seed per connection and emits its extensions in rustls's seed-derived
// order (see rustls_order.go) — rustls does NOT use a Chrome-style Fisher–Yates
// shuffle, so the permutation space must match rustls exactly. SNI participates
// in the sort like any other extension (hostname targets); uTLS omits the
// extension entirely for IP targets, which is also what the real CLIs do (the
// captures, taken against an IP, show no SNI).
//
// Randoms differ per connection by design (client random, session id, key
// shares, and — on codex — extension order) — uTLS generates fresh ones,
// matching how the real CLI behaves.
//
// The reference project CLIProxyAPI ships the same approach for Anthropic
// (internal/runtime/executor/helps/utls_client.go, claudeCodeTLSClientHelloSpec);
// ours is transcribed from our own capture of the current CLI version instead.

// codexRustlsClientHelloSpec reproduces the genuine codex CLI's rustls
// ClientHello for /responses traffic. The extension order below is the captured
// baseline only: dialUTLS reorders it per connection into rustls's seed-derived
// order (see rustls_order.go) to match the real CLI, whose extension order
// varies per connection.
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
// Node/OpenSSL ClientHello for /messages traffic, transcribed from a captured
// business (2.1.301, /v1/messages) handshake: 17 cipher suites, ALPN http/1.1,
// the extension order below, and supported versions TLS 1.3/1.2. The face still
// rides the HTTP/1.1 path — its ALPN never negotiates h2.
func claudeCodeNodeClientHelloSpec() *utls.ClientHelloSpec {
	return &utls.ClientHelloSpec{
		CipherSuites: []uint16{
			0x1301, // TLS_AES_128_GCM_SHA256
			0x1302, // TLS_AES_256_GCM_SHA384
			0x1303, // TLS_CHACHA20_POLY1305_SHA256
			0xc02b, // ECDHE-ECDSA-AES128-GCM-SHA256
			0xc02f, // ECDHE-RSA-AES128-GCM-SHA256
			0xc02c, // ECDHE-ECDSA-AES256-GCM-SHA384
			0xc030, // ECDHE-RSA-AES256-GCM-SHA384
			0xcca9, // ECDHE-ECDSA-CHACHA20-POLY1305
			0xcca8, // ECDHE-RSA-CHACHA20-POLY1305
			0xc009, // ECDHE-ECDSA-AES128-SHA
			0xc013, // ECDHE-RSA-AES128-SHA
			0xc00a, // ECDHE-ECDSA-AES256-SHA
			0xc014, // ECDHE-RSA-AES256-SHA
			0x009c, // RSA-AES128-GCM-SHA256
			0x009d, // RSA-AES256-GCM-SHA384
			0x002f, // RSA-AES128-SHA
			0x0035, // RSA-AES256-SHA
		},
		CompressionMethods: []uint8{0},
		Extensions: []utls.TLSExtension{
			// Extension wire order (IP target):
			// 23,65281,10,11,35,16,5,13,18,51,45,43. SNI (0) is prepended for
			// hostname targets; uTLS omits it entirely for IP targets, exactly
			// like the captured CLI handshake (an IP leg).
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{}, // 23
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient}, // 65281
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.X25519MLKEM768, // 0x11ec
				utls.X25519,         // 0x001d
				utls.CurveP256,      // 0x0017
				utls.CurveP384,      // 0x0018
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}}, // 11
			&utls.SessionTicketExtension{},                             // 35
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},   // 16
			&utls.StatusRequestExtension{},                             // 5
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, // ecdsa_secp256r1_sha256
				0x0804, // rsa_pss_rsae_sha256
				0x0401, // rsa_pkcs1_sha256
				0x0503, // ecdsa_secp384r1_sha384
				0x0805, // rsa_pss_rsae_sha384
				0x0501, // rsa_pkcs1_sha384
				0x0806, // rsa_pss_rsae_sha512
				0x0601, // rsa_pkcs1_sha512
				0x0201, // rsa_pkcs1_sha1
			}}, // 13
			&utls.SCTExtension{}, // 18
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.X25519MLKEM768}, {Group: utls.X25519},
			}}, // 51
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},                        // 45
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}}, // 43
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
// keeps per-connection ServerName fills off the shared spec — and the copied
// extension slice lets dialUTLS reshuffle the codex face per dial without
// touching the base spec. Values (groups, lengths, cipher/curve/sig order) come
// from the same literals; only the codex extension order is per-dial.
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
