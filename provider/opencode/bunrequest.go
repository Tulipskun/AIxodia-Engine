package opencode

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// The endpoint's answer does not depend only on the TLS shape. Matching that got
// the connection accepted and the request refused, which leaves the request
// itself — and not its contents, which were already copied byte for byte, but
// the order they are written in. Go's own writer sorts header names; the client
// writes them in the order its own code builds them, and a header order is as
// much a fingerprint as a cipher list.
//
// So the request is written by hand, in the captured order, over the connection
// the previous step negotiated.

// header is one header as the client sends it: name and value in the order they
// appear. Order is the whole point, so this is a slice and not a map.
type header struct {
	name  string
	value string
}

// bunHeaderOrder is the order the client writes its headers in, read off its own
// request. Anything not named here is written after these, which is what happens
// to a header the client would not have sent either.
var bunHeaderOrder = []string{
	"accept",
	"accept-encoding",
	"authorization",
	"connection",
	"content-length",
	"content-type",
	"host",
	"user-agent",
	"b3",
	"traceparent",
	"x-opencode-client",
	"x-opencode-org-id",
	"x-opencode-project",
	"x-opencode-session",
	"x-opencode-session-id",
	"x-session-affinity",
	"x-session-id",
}

// ordered writes the headers the way the client does.
func ordered(headers http.Header, host string, contentLength int) []header {
	values := map[string]string{}
	for name, list := range headers {
		values[strings.ToLower(name)] = strings.Join(list, ", ")
	}
	values["host"] = host
	if contentLength >= 0 {
		values["content-length"] = strconv.Itoa(contentLength)
	}

	written := map[string]bool{}
	out := make([]header, 0, len(values))
	for _, name := range bunHeaderOrder {
		if value, ok := values[name]; ok {
			out = append(out, header{name: name, value: value})
			written[name] = true
		}
	}
	// Anything the caller added that the client has no equivalent for keeps its
	// place at the end rather than being dropped.
	rest := make([]string, 0, len(values))
	for name := range values {
		if !written[name] {
			rest = append(rest, name)
		}
	}
	sortStrings(rest)
	for _, name := range rest {
		out = append(out, header{name: name, value: values[name]})
	}
	return out
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

// bunRequest performs one HTTP/1.1 request over a connection already negotiated
// with the recorded ClientHello, writing the headers in the recorded order.
func bunRequest(ctx context.Context, rawURL string, method string, headers http.Header, body []byte, timeout time.Duration) (*http.Response, []byte, error) {
	target, err := parseTarget(rawURL)
	if err != nil {
		return nil, nil, err
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", target.addr)
	if err != nil {
		return nil, nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	tlsConn := utls.UClient(conn, &utls.Config{
		ServerName:       target.host,
		CurvePreferences: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384},
	}, utls.HelloCustom)
	if err := tlsConn.ApplyPreset(bunClientHello()); err != nil {
		return nil, nil, err
	}
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		return nil, nil, err
	}

	length := -1
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		length = len(body)
	}
	var request bytes.Buffer
	request.WriteString(method + " " + target.path + " HTTP/1.1\r\n")
	for _, h := range ordered(headers, target.hostHeader, length) {
		request.WriteString(h.name + ": " + h.value + "\r\n")
	}
	request.WriteString("\r\n")
	request.Write(body)

	if _, err := tlsConn.Write(request.Bytes()); err != nil {
		return nil, nil, err
	}

	reader := bufio.NewReader(tlsConn)
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		return nil, nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return response, nil, err
	}
	return response, payload, nil
}

type target struct {
	addr       string
	host       string
	hostHeader string
	path       string
}

func parseTarget(rawURL string) (target, error) {
	rest := rawURL
	scheme := "https://"
	if strings.HasPrefix(rest, scheme) {
		rest = rest[len(scheme):]
	} else if strings.HasPrefix(rest, "http://") {
		return target{}, fmt.Errorf("only https is supported")
	}
	slash := strings.Index(rest, "/")
	host := rest
	path := "/"
	if slash >= 0 {
		host = rest[:slash]
		path = rest[slash:]
	}
	if host == "" {
		return target{}, fmt.Errorf("no host in %q", rawURL)
	}
	return target{addr: host + ":443", host: host, hostHeader: host, path: path}, nil
}
