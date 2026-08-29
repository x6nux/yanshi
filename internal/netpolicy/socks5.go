package netpolicy

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKS5 support exists because the proxy environment variables do not reach
// every client that respects a proxy. curl, git, go and npm read
// http_proxy/https_proxy; ssh -o ProxyCommand, many Go programs, and anything
// configured through ALL_PROXY speak SOCKS instead. A managed proxy that only
// understands HTTP leaves those on the unfiltered path while still being
// described as "the governed egress channel", which is the shape ADR-0014
// exists to stop.
//
// It shares the HTTP listener rather than binding a second port: the first
// byte of a SOCKS5 greeting is 0x05, which is not a valid first byte of any
// HTTP request line, so one peek separates the two protocols with no extra
// configuration surface and one URL to publish.

// socks5 protocol constants. Only the subset this proxy implements is named;
// an unnamed value on the wire is answered with a failure reply rather than
// guessed at.
const (
	socks5Version = 0x05
	socks5NoAuth  = 0x00
	socks5Connect = 0x01

	socks5AddrIPv4   = 0x01
	socks5AddrDomain = 0x03
	socks5AddrIPv6   = 0x04

	socks5ReplyOK           = 0x00
	socks5ReplyRuleset      = 0x02 // "connection not allowed by ruleset"
	socks5ReplyHostUnreach  = 0x04
	socks5ReplyCmdNotSupp   = 0x07
	socks5ReplyAddrNotSupp  = 0x08
	socks5HandshakeDeadline = 30 * time.Second
)

// serveSOCKS5 handles one SOCKS5 client from the greeting through to the
// spliced tunnel. The connection is closed on every return path.
//
// Authentication is "no auth" on purpose: the listener is bound to loopback
// and the credential that would be checked here would have to be handed to the
// child in an environment variable, which puts it exactly where every other
// child-readable secret is. The access control that matters is the policy
// check below, not a password the caller was given for free.
func (p *Proxy) serveSOCKS5(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(socks5HandshakeDeadline))
	if err := socks5Greet(conn); err != nil {
		return
	}
	host, port, err := socks5ReadRequest(conn)
	if err != nil {
		return
	}
	target := net.JoinHostPort(host, strconv.Itoa(int(port)))

	// SOCKS5 CONNECT carries no method and no URL — it is a byte tunnel from
	// the first packet. The method dimension therefore cannot apply and is not
	// pretended to: CheckRequest with an empty method returns the host verdict.
	d := p.authorize(p.baseCtx(), "socks5", host, "")
	if !d.Allowed {
		p.audit("socks5", host, d)
		_ = socks5Reply(conn, socks5ReplyRuleset, nil)
		return
	}
	p.audit("socks5", host, d)

	upstream, err := p.dialer.DialContext(p.baseCtx(), "tcp", target)
	if err != nil {
		_ = socks5Reply(conn, socks5ReplyHostUnreach, nil)
		return
	}
	defer upstream.Close()
	if err := socks5Reply(conn, socks5ReplyOK, upstream.LocalAddr()); err != nil {
		return
	}
	// The handshake deadline must go before the splice or a long-lived tunnel
	// dies 30 seconds in.
	_ = conn.SetDeadline(time.Time{})
	splice(conn, upstream)
}

// socks5Greet reads the method-selection greeting and answers "no
// authentication required". A client that does not offer 0x00 is refused with
// 0xFF, which is the protocol's own "no acceptable methods".
func socks5Greet(conn net.Conn) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return err
	}
	if header[0] != socks5Version {
		return fmt.Errorf("netpolicy: socks version %d not supported", header[0])
	}
	methods := make([]byte, int(header[1]))
	if _, err := io.ReadFull(conn, methods); err != nil {
		return err
	}
	for _, m := range methods {
		if m == socks5NoAuth {
			_, err := conn.Write([]byte{socks5Version, socks5NoAuth})
			return err
		}
	}
	_, _ = conn.Write([]byte{socks5Version, 0xFF})
	return fmt.Errorf("netpolicy: socks client offered no supported auth method")
}

// socks5ReadRequest parses the CONNECT request and returns its target. BIND
// and UDP ASSOCIATE are refused: this proxy has no way to police a listening
// socket or a datagram stream, and answering "ok" to a command it does not
// implement would hand the child a channel nothing watches.
func socks5ReadRequest(conn net.Conn) (host string, port uint16, err error) {
	header := make([]byte, 4)
	if _, err = io.ReadFull(conn, header); err != nil {
		return "", 0, err
	}
	if header[0] != socks5Version {
		return "", 0, fmt.Errorf("netpolicy: socks version %d not supported", header[0])
	}
	if header[1] != socks5Connect {
		_ = socks5Reply(conn, socks5ReplyCmdNotSupp, nil)
		return "", 0, fmt.Errorf("netpolicy: socks command %d not supported", header[1])
	}
	switch header[3] {
	case socks5AddrIPv4:
		buf := make([]byte, 4)
		if _, err = io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		host = net.IP(buf).String()
	case socks5AddrIPv6:
		buf := make([]byte, 16)
		if _, err = io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		host = net.IP(buf).String()
	case socks5AddrDomain:
		size := make([]byte, 1)
		if _, err = io.ReadFull(conn, size); err != nil {
			return "", 0, err
		}
		buf := make([]byte, int(size[0]))
		if _, err = io.ReadFull(conn, buf); err != nil {
			return "", 0, err
		}
		host = string(buf)
	default:
		_ = socks5Reply(conn, socks5ReplyAddrNotSupp, nil)
		return "", 0, fmt.Errorf("netpolicy: socks address type %d not supported", header[3])
	}
	portBuf := make([]byte, 2)
	if _, err = io.ReadFull(conn, portBuf); err != nil {
		return "", 0, err
	}
	return host, binary.BigEndian.Uint16(portBuf), nil
}

// socks5Reply writes a reply frame. bound is the address the proxy connected
// from and is echoed back when known; a nil bound sends 0.0.0.0:0, which is
// what the RFC allows for a reply that carries no useful binding.
func socks5Reply(conn net.Conn, code byte, bound net.Addr) error {
	out := []byte{socks5Version, code, 0x00, socks5AddrIPv4, 0, 0, 0, 0, 0, 0}
	if tcp, ok := bound.(*net.TCPAddr); ok && tcp != nil {
		if v4 := tcp.IP.To4(); v4 != nil {
			copy(out[4:8], v4)
			binary.BigEndian.PutUint16(out[8:10], uint16(tcp.Port))
		}
	}
	_, err := conn.Write(out)
	return err
}

// splice copies bytes in both directions until either side closes. Returning
// on the FIRST direction to finish (rather than waiting for both) is what the
// CONNECT path does too: the caller's defers close both ends, which unblocks
// the surviving copy.
func splice(a, b net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(a, b); done <- struct{}{} }()
	go func() { _, _ = io.Copy(b, a); done <- struct{}{} }()
	<-done
}
