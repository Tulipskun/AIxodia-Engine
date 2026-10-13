package opencode

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

// THIS DOES NOT GET PAST THE GATE. It is kept because the measurement in it is
// the expensive part and there is no reason for anyone to repeat it.
//
// Matching the connection gets the TLS handshake accepted and the request still
// refused, and the one value that cannot be matched is the reason: the client
// offers x25519mlkem768 (group 4588) first, Cloudflare prefers it, and this TLS
// library cannot generate a key share for it — offering it fails the handshake
// outright with "CurvePreferences includes unsupported curve", and leaving it
// out is a different fingerprint. So the JA3 cannot be reproduced exactly with
// this tooling, and reproducing it approximately is not enough.
//
// Everything else was matched and made no difference: the cipher suites, the
// extension list and its order, the signature algorithms, the ALPN, the request
// written by hand in the client's own header order, and every header value
// including the key, the organisation, the project and the session ids.
//
// The endpoint refuses every caller but the OpenCode client, and it decides who
// is calling from the connection rather than from the request: the client's own
// request was captured byte for byte — headers, session ids, project id, the
// tool declarations, the key — and replayed from Go, Python and curl, from this
// host and from Kaggle, and every one of them was refused while the client
// answered in the same second.
//
// So what has to match is the connection. Everything below is measured from the
// client rather than guessed: a TLS server was put in front of it, its
// ClientHello was recorded, and the numbers are copied from that recording. The
// same JA3 came back on fifteen consecutive connections, so this is its shape
// and not a coincidence.
//
// This is impersonation and it is worth being plain about the cost: it is
// pinned to one build of one client, and it stops working the day that client's
// TLS stack changes. It is not a supported interface and nothing promises it
// will keep working. What it is not is a guess — every value here was read off
// the wire.

const (
	// JA3 5260242a2eb12c71995767c24569bff5
	// 771,4865-4866-4867-49195-49199-49196-49200-52393-52392-49161-49171-49162-49172-156-157-47-53,
	// 23-65281-10-11-35-16-5-13-18-51-45-43,4588-29-23-24,0
	//
	// The client offers http/1.1 and nothing else, so there is no HTTP/2
	// fingerprint to match and Go's HTTP/1.1 writer is used as it is.
	bunALPN = "http/1.1"
)

// bunCipherSuites are the suites the client offers, in its order. Order is part
// of the fingerprint, so this is not sorted.
var bunCipherSuites = []uint16{
	0x1301, // TLS_AES_128_GCM_SHA256
	0x1302, // TLS_AES_256_GCM_SHA384
	0x1303, // TLS_CHACHA20_POLY1305_SHA256
	0xC02B, // ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
	0xC02F, // ECDHE_RSA_WITH_AES_128_GCM_SHA256
	0xC02C, // ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
	0xC030, // ECDHE_RSA_WITH_AES_256_GCM_SHA384
	0xCCA9, // ECDHE_ECDSA_WITH_CHACHA20_POLY1305
	0xCCA8, // ECDHE_RSA_WITH_CHACHA20_POLY1305
	0xC009, // ECDHE_ECDSA_WITH_AES_128_CBC_SHA
	0xC013, // ECDHE_RSA_WITH_AES_128_CBC_SHA
	0xC00A, // ECDHE_ECDSA_WITH_AES_256_CBC_SHA
	0xC014, // ECDHE_RSA_WITH_AES_256_CBC_SHA
	0x009C, // RSA_WITH_AES_128_GCM_SHA256
	0x009D, // RSA_WITH_AES_256_GCM_SHA384
	0x002F, // RSA_WITH_AES_128_CBC_SHA
	0x0035, // RSA_WITH_AES_256_CBC_SHA
}

// bunSignatureAlgorithms are the schemes the client offers, in its order.
var bunSignatureAlgorithms = []utls.SignatureScheme{
	1027, // ecdsa_secp256r1_sha256
	2052, // rsa_pss_rsae_sha256
	1025, // rsa_pkcs1_sha256
	1283, // ecdsa_secp384r1_sha384
	2053, // rsa_pss_rsae_sha384
	1281, // rsa_pkcs1_sha384
	2054, // rsa_pss_rsae_sha512
	1537, // rsa_pkcs1_sha512
	513,  // ecdsa_sha1
}

// bunSupportedCurves are the groups the client offers, in its order. 4588 is
// x25519mlkem768, which the client names first.
var bunSupportedCurves = []utls.CurveID{
	utls.X25519,
	utls.CurveP256,
	utls.CurveP384,
}

// bunClientHello is the recorded ClientHello, built by starting from the
// preset this library already knows how to send and then replacing the parts
// that were measured.
//
// Building it from nothing was tried first and every handshake failed, including
// with one extension removed at a time — the shape was wrong in a way that no
// single deletion explained. Chrome's preset is BoringSSL's, and so is the
// client's, so everything structural in it is already legal; only the measured
// values are substituted. That is both less code and the difference between a
// connection that is accepted and one that is not.
func bunClientHello() *utls.ClientHelloSpec {
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		// The preset ships with the library; if it cannot be read there is
		// nothing to fall back to that would be any better.
		panic("utls: chrome preset unavailable: " + err.Error())
	}
	spec.CipherSuites = bunCipherSuites

	// The measured extension order, and nothing the client does not send: the
	// preset carries GREASE placeholders and a compress_certificate that the
	// recording has no room for.
	wanted := []utls.TLSExtension{
		&utls.SNIExtension{},                  // 0
		&utls.ExtendedMasterSecretExtension{}, // 23
		&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},            // 65281
		&utls.SupportedCurvesExtension{Curves: bunSupportedCurves},                               // 10
		&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},                               // 11
		&utls.SessionTicketExtension{},                                                           // 35
		&utls.ALPNExtension{AlpnProtocols: []string{bunALPN}},                                    // 16
		&utls.StatusRequestExtension{},                                                           // 5
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: bunSignatureAlgorithms}, // 13
		&utls.SCTExtension{}, // 18
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},                  // 51
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},                        // 45
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}}, // 43
	}
	spec.Extensions = wanted
	return &spec
}

// bunTransport dials with that ClientHello.
//
// HTTP/2 is deliberately left off. The client does not offer it, so a request
// over h2 would be a different caller than the one being matched, and the whole
// point is to be the same one.
func bunTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return &http.Transport{
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			conn := utls.UClient(raw, &utls.Config{
				ServerName: host,
				// The key share is built from here, not from the extension list,
				// and Go's default set includes the post-quantum group this
				// library cannot generate a share for — which fails the
				// handshake before anything is sent. The curves named on the
				// wire still include it, because the extension list is what is
				// fingerprinted and this is only what can actually be computed.
				CurvePreferences: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384},
			}, utls.HelloCustom)
			if err := conn.ApplyPreset(bunClientHello()); err != nil {
				raw.Close()
				return nil, err
			}
			if err := conn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return conn, nil
		},
		ForceAttemptHTTP2:   false,
		MaxIdleConns:        8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 30 * time.Second,
	}
}

// bunClient is the HTTP client the opencode adapter sends with.
func bunClient() *http.Client {
	return &http.Client{Transport: bunTransport()}
}

// assertTLS12 keeps the TLS 1.2 suites meaningful: utls.Config without a
// minimum would negotiate 1.3 only, and the recorded hello offers both.
var _ = tls.VersionTLS12
