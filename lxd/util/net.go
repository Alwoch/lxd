package util

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net"
	"reflect"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/canonical/lxd/shared"
	"github.com/canonical/lxd/shared/logger"
)

// InMemoryNetwork creates a fully in-memory listener and dial function.
//
// Each time the dial function is invoked a new pair of net.Conn objects will
// be created using net.Pipe: the listener's Accept method will unblock and
// return one end of the pipe and the other end will be returned by the dial
// function.
func InMemoryNetwork() (net.Listener, func() net.Conn) {
	listener := &inMemoryListener{
		conns:  make(chan net.Conn, 16),
		closed: make(chan struct{}),
	}
	dialer := func() net.Conn {
		server, client := net.Pipe()
		listener.conns <- server
		return client
	}
	return listener, dialer
}

type inMemoryListener struct {
	conns  chan net.Conn
	closed chan struct{}
}

// Accept waits for and returns the next connection to the listener.
func (l *inMemoryListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, fmt.Errorf("closed")
	}
}

// Close closes the listener.
// Any blocked Accept operations will be unblocked and return errors.
func (l *inMemoryListener) Close() error {
	close(l.closed)
	return nil
}

// Addr returns the listener's network address.
func (l *inMemoryListener) Addr() net.Addr {
	return &inMemoryAddr{}
}

type inMemoryAddr struct {
}

func (a *inMemoryAddr) Network() string {
	return "memory"
}

func (a *inMemoryAddr) String() string {
	return ""
}

// CanonicalNetworkAddress parses the given network address and returns a string of the form "host:port",
// possibly filling it with the default port if it's missing. It will also wrap a bare IPv6 address with square
// brackets if needed.
func CanonicalNetworkAddress(address string, defaultPort int) string {
	_, _, err := net.SplitHostPort(address)
	if err != nil {
		ip := net.ParseIP(address)
		if ip != nil {
			// If the input address is a bare IP address, then convert it to a proper listen address
			// using the canonical IP with default port and wrap IPv6 addresses in square brackets.
			address = net.JoinHostPort(ip.String(), fmt.Sprintf("%d", defaultPort))
		} else {
			// Otherwise assume this is either a host name or a partial address (e.g `[::]`) without
			// a port number, so append the default port.
			address = fmt.Sprintf("%s:%d", address, defaultPort)
		}
	}

	return address
}

// CanonicalNetworkAddressFromAddressAndPort returns a network address from separate address and port values.
// The address accepts values such as "[::]", "::" and "localhost".
func CanonicalNetworkAddressFromAddressAndPort(address string, port int, defaultPort int) string {
	// Because we accept just the host part of an IPv6 listen address (e.g. `[::]`) don't use net.JoinHostPort.
	// If a bare IP address is supplied then CanonicalNetworkAddress will use net.JoinHostPort if needed.
	return CanonicalNetworkAddress(fmt.Sprintf("%s:%d", address, port), defaultPort)
}

// ServerTLSConfig returns a new server-side tls.Config generated from the give
// certificate info.
func ServerTLSConfig(cert *shared.CertInfo) *tls.Config {
	config := shared.InitTLSConfig()
	config.ClientAuth = tls.RequestClientCert
	config.Certificates = []tls.Certificate{cert.KeyPair()}
	config.NextProtos = []string{"h2"} // Required by gRPC

	if cert.CA() != nil {
		pool := x509.NewCertPool()
		pool.AddCert(cert.CA())
		config.RootCAs = pool
		config.ClientCAs = pool

		logger.Infof("LXD is in CA mode, only CA-signed certificates will be allowed")
	}

	config.BuildNameToCertificate()
	return config
}

// NetworkInterfaceAddress returns the first global unicast address of any of the system network interfaces.
// Return the empty string if none is found.
func NetworkInterfaceAddress() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		if len(addrs) == 0 {
			continue
		}

		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}

			if !ipNet.IP.IsGlobalUnicast() {
				continue
			}

			return ipNet.IP.String()
		}
	}

	return ""
}

// IsAddressCovered detects if network address1 is actually covered by
// address2, in the sense that they are either the same address or address2 is
// specified using a wildcard with the same port of address1.
func IsAddressCovered(address1, address2 string) bool {
	address1 = CanonicalNetworkAddress(address1, shared.HTTPSDefaultPort)
	address2 = CanonicalNetworkAddress(address2, shared.HTTPSDefaultPort)

	if address1 == address2 {
		return true
	}

	host1, port1, err := net.SplitHostPort(address1)
	if err != nil {
		return false
	}

	host2, port2, err := net.SplitHostPort(address2)
	if err != nil {
		return false
	}

	// If the ports are different, then address1 is clearly not covered by
	// address2.
	if port2 != port1 {
		return false
	}

	// If address1 contains a host name, let's try to resolve it, in order
	// to compare the actual IPs.
	var addresses1 []net.IP
	if host1 != "" {
		ip := net.ParseIP(host1)
		if ip != nil {
			addresses1 = append(addresses1, ip)
		} else {
			ips, err := net.LookupHost(host1)
			if err == nil && len(ips) > 0 {
				for _, ipStr := range ips {
					ip := net.ParseIP(ipStr)
					if ip != nil {
						addresses1 = append(addresses1, ip)
					}
				}
			}
		}
	}

	// If address2 contains a host name, let's try to resolve it, in order
	// to compare the actual IPs.
	var addresses2 []net.IP
	if host2 != "" {
		ip := net.ParseIP(host2)
		if ip != nil {
			addresses2 = append(addresses2, ip)
		} else {
			ips, err := net.LookupHost(host2)
			if err == nil && len(ips) > 0 {
				for _, ipStr := range ips {
					ip := net.ParseIP(ipStr)
					if ip != nil {
						addresses2 = append(addresses2, ip)
					}
				}
			}
		}
	}

	for _, a1 := range addresses1 {
		for _, a2 := range addresses2 {
			if a1.Equal(a2) {
				return true
			}
		}
	}

	// If address2 is using an IPv4 wildcard for the host, then address2 is
	// only covered if it's an IPv4 address.
	if host2 == "0.0.0.0" {
		ip1 := net.ParseIP(host1)
		if ip1 != nil && ip1.To4() != nil {
			return true
		}
		return false
	}

	// If address2 is using an IPv6 wildcard for the host, then address2 is
	// always covered.
	if host2 == "::" || host2 == "" {
		return true
	}

	return false
}

// IsWildCardAddress returns whether the given address is a wildcard.
func IsWildCardAddress(address string) bool {
	address = CanonicalNetworkAddress(address, shared.HTTPSDefaultPort)

	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}

	if host == "0.0.0.0" || host == "::" || host == "" {
		return true
	}

	return false
}

// SysctlGet retrieves the value of a sysctl file in /proc/sys.
func SysctlGet(path string) (string, error) {
	// Read the current content
	content, err := ioutil.ReadFile(fmt.Sprintf("/proc/sys/%s", path))
	if err != nil {
		return "", err
	}

	return string(content), nil
}

// SysctlSet writes a value to a sysctl file in /proc/sys.
// Requires an even number of arguments as key/value pairs. E.g. SysctlSet("path1", "value1", "path2", "value2")
func SysctlSet(parts ...string) error {
	partsLen := len(parts)
	if partsLen%2 != 0 {
		return fmt.Errorf("Requires even number of arguments")
	}

	for i := 0; i < partsLen; i = i + 2 {
		path := parts[i]
		newValue := parts[i+1]

		// Get current value.
		currentValue, err := SysctlGet(path)
		if err == nil && currentValue == newValue {
			// Nothing to update.
			return nil
		}

		err = ioutil.WriteFile(fmt.Sprintf("/proc/sys/%s", path), []byte(newValue), 0)
		if err != nil {
			return err
		}
	}

	return nil
}

// ExtractTCPConn tries to extract the underlying net.TCPConn from a tls.Conn.
func ExtractTCPConn(conn net.Conn) (*net.TCPConn, error) {
	// Go doesn't currently expose the underlying TCP connection of a TLS connection, but we need it in order
	// to set timeout properties on the connection. We use some reflect/unsafe magic to extract the private
	// remote.conn field, which is indeed the underlying TCP connection.
	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return nil, fmt.Errorf("Connection is not a tls.Conn")
	}

	field := reflect.ValueOf(tlsConn).Elem().FieldByName("conn")
	field = reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	c := field.Interface()

	tcpConn, ok := c.(*net.TCPConn)
	if !ok {
		return nil, fmt.Errorf("Connection is not a net.TCPConn")
	}

	return tcpConn, nil
}

// SetTCPTimeouts sets TCP_USER_TIMEOUT and TCP keep alive timeouts on a connection.
func SetTCPTimeouts(conn *net.TCPConn) error {
	// Set TCP_USER_TIMEOUT option to limit the maximum amount of time in ms that transmitted data may remain
	// unacknowledged before TCP will forcefully close the corresponding connection and return ETIMEDOUT to the
	// application. This combined with the TCP keepalive options on the socket will ensure that should the
	// remote side of the connection disappear abruptly that LXD will detect this and close the socket quickly.
	// Decreasing the user timeouts allows applications to "fail fast" if so desired. Otherwise it may take
	// up to 20 minutes with the current system defaults in a normal WAN environment if there are packets in
	// the send queue that will prevent the keepalive timer from working as the retransmission timers kick in.
	// See https://git.kernel.org/pub/scm/linux/kernel/git/torvalds/linux.git/commit/?id=dca43c75e7e545694a9dd6288553f55c53e2a3a3
	err := SetTCPUserTimeout(conn, time.Second*30)
	if err != nil {
		return err
	}

	err = conn.SetKeepAlive(true)
	if err != nil {
		return err
	}

	err = conn.SetKeepAlivePeriod(3 * time.Second)
	if err != nil {
		return err
	}

	return nil
}

// SetTCPUserTimeout sets the TCP user timeout on a connection's socket
func SetTCPUserTimeout(conn *net.TCPConn, timeout time.Duration) error {
	rawConn, err := conn.SyscallConn()
	if err != nil {
		return fmt.Errorf("Error getting raw connection: %w", err)
	}

	err = rawConn.Control(func(fd uintptr) {
		err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, unix.TCP_USER_TIMEOUT, int(timeout/time.Millisecond))
	})
	if err != nil {
		return fmt.Errorf("Error setting option on socket: %w", err)
	}

	return nil
}
