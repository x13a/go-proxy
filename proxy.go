package proxy

import (
	"errors"
	"io"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTimeout    = 1 << 3 * time.Second
	DefaultBufferSize = 1 << 9
)

func NewFileStreamProxy(
	fd int,
	dest string,
	timeout time.Duration,
) (*streamProxy, error) {
	file, err := newFile(fd)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	ln, err := net.FileListener(file)
	if err != nil {
		return nil, err
	}
	return NewStreamProxy(ln, dest, timeout), nil
}

func NewFilePacketProxy(
	fd int,
	dest string,
	timeout time.Duration,
	bufSize int,
) (*packetProxy, error) {
	file, err := newFile(fd)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	pc, err := net.FilePacketConn(file)
	if err != nil {
		return nil, err
	}
	return NewPacketProxy(pc, dest, timeout, bufSize), nil
}

func newFile(fd int) (*os.File, error) {
	if file := os.NewFile(uintptr(fd), ""); file != nil {
		return file, nil
	}
	return nil, errors.New("Invalid file descriptor: " + strconv.Itoa(fd))
}

func prepareDestination(dest string, addr net.Addr) (network, address string) {
	substr := "://"
	if idx := strings.Index(dest, substr); idx == -1 {
		address = dest
	} else {
		network = dest[:idx]
		address = dest[idx+len(substr):]
	}
	if network == "" {
		network = addr.Network()
	}
	if !strings.HasPrefix(network, "tcp") &&
		!strings.HasPrefix(network, "udp") {

		return
	}
	if host, _, err := net.SplitHostPort(address); err == nil && host == "" {
		var ip net.IP
		switch laddr := addr.(type) {
		case *net.TCPAddr:
			ip = laddr.IP
		case *net.UDPAddr:
			ip = laddr.IP
		default:
			return
		}
		if ip.To4() != nil {
			address = "127.0.0.1" + address
		} else {
			address = "[::1]" + address
		}
	}
	return
}

type Proxy interface {
	Start()
	Close() error
	WaitChan() <-chan struct{}
}

func NewStreamProxy(
	ln net.Listener,
	dest string,
	timeout time.Duration,
) *streamProxy {
	network, address := prepareDestination(dest, ln.Addr())
	if timeout < 1 {
		timeout = DefaultTimeout
	}
	return &streamProxy{
		ln:       ln,
		network:  network,
		address:  address,
		timeout:  timeout,
		waitChan: make(chan struct{}),
	}
}

type streamProxy struct {
	ln       net.Listener
	network  string
	address  string
	timeout  time.Duration
	waitChan chan struct{}
}

func (p *streamProxy) Start() {
	go p.start()
}

func (p *streamProxy) Close() error {
	return p.ln.Close()
}

func (p *streamProxy) WaitChan() <-chan struct{} {
	return p.waitChan
}

func (p *streamProxy) start() {
	defer close(p.waitChan)
	defer p.ln.Close()
	addr := p.ln.Addr()
	network := addr.Network()
	log.Printf("[%s] %s -> [%s] %s\n", network, addr, p.network, p.address)
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			log.Println(err)
			break
		}
		go p.handle(conn)
	}
	log.Printf("[%s] %s -> exit\n", network, addr)
}

func (p *streamProxy) handle(in net.Conn) {
	defer in.Close()
	out, err := net.DialTimeout(p.network, p.address, p.timeout)
	if err != nil {
		log.Println(err)
		return
	}
	defer out.Close()
	errChan := make(chan error, 2)
	proxy := func(dst, src net.Conn) {
		_, err := io.Copy(dst, src)
		errChan <- err
	}
	go proxy(out, in)
	go proxy(in, out)
	for i, n := 0, cap(errChan); i < n; i++ {
		select {
		case err = <-errChan:
			if err != nil {
				log.Println(err)
			}
		case <-p.waitChan:
			return
		}
	}
}

func makeConnMap() connMap {
	return connMap{m: make(map[string]net.Conn)}
}

type connMap struct {
	mu sync.RWMutex
	m  map[string]net.Conn
}

func (m *connMap) Load(key string) (value net.Conn, ok bool) {
	m.mu.RLock()
	value, ok = m.m[key]
	m.mu.RUnlock()
	return
}

func (m *connMap) LoadOrStore(
	key string,
	value net.Conn,
) (actual net.Conn, loaded bool) {
	m.mu.Lock()
	actual, loaded = m.m[key]
	if !loaded {
		m.m[key] = value
		actual = value
	}
	m.mu.Unlock()
	return
}

func (m *connMap) Delete(key string) {
	m.mu.Lock()
	delete(m.m, key)
	m.mu.Unlock()
}

func NewPacketProxy(
	pc net.PacketConn,
	dest string,
	timeout time.Duration,
	bufSize int,
) *packetProxy {
	network, address := prepareDestination(dest, pc.LocalAddr())
	if timeout < 1 {
		timeout = DefaultTimeout
	}
	if bufSize < 1 {
		bufSize = DefaultBufferSize
	}
	return &packetProxy{
		pc:       pc,
		storage:  makeConnMap(),
		network:  network,
		address:  address,
		timeout:  timeout,
		bufSize:  bufSize,
		waitChan: make(chan struct{}),
	}
}

type packetProxy struct {
	pc       net.PacketConn
	storage  connMap
	network  string
	address  string
	timeout  time.Duration
	bufSize  int
	waitChan chan struct{}
}

func (p *packetProxy) Start() {
	go p.start()
}

func (p *packetProxy) Close() error {
	return p.pc.Close()
}

func (p *packetProxy) WaitChan() <-chan struct{} {
	return p.waitChan
}

func (p *packetProxy) start() {
	defer close(p.waitChan)
	defer p.pc.Close()
	addr := p.pc.LocalAddr()
	network := addr.Network()
	log.Printf("[%s] %s -> [%s] %s\n", network, addr, p.network, p.address)
	for {
		buf := make([]byte, p.bufSize)
		n, addr, err := p.pc.ReadFrom(buf)
		if n > 0 {
			go p.handle(buf[:n], addr)
		}
		if err != nil {
			log.Println(err)
			break
		}
	}
	log.Printf("[%s] %s -> exit\n", network, addr)
}

func (p *packetProxy) nextDeadline() time.Time {
	return time.Now().Add(p.timeout)
}

func (p *packetProxy) handle(data []byte, addr net.Addr) {
	var err error
	addrstr := addr.String()
	out, ok := p.storage.Load(addrstr)
	if !ok {
		out, err = p.dial()
		if err != nil {
			log.Println(err)
			return
		}
		if out1, loaded := p.storage.LoadOrStore(addrstr, out); loaded {
			out.Close()
			out = out1
		} else {
			go p.proxy(out, addr)
		}
	}
	if _, err = out.Write(data); err == nil {
		err = out.SetReadDeadline(p.nextDeadline())
	}
	if err != nil && !isErrNetClosing(err) {
		log.Println(err)
	}
}

func (p *packetProxy) dial() (net.Conn, error) {
	out, err := net.DialTimeout(p.network, p.address, p.timeout)
	if err != nil {
		return nil, err
	}
	if err = out.SetReadDeadline(p.nextDeadline()); err != nil {
		out.Close()
		return nil, err
	}
	return out, nil
}

func (p *packetProxy) proxy(out net.Conn, addr net.Addr) {
	waitChan := make(chan struct{})
	defer func() {
		p.storage.Delete(addr.String())
		out.Close()
		close(waitChan)
	}()
	go func() {
		select {
		case <-p.waitChan:
			out.Close()
		case <-waitChan:
		}
	}()
	buf := make([]byte, p.bufSize)
	for {
		n, err := out.Read(buf)
		if n > 0 {
			if _, err1 := p.pc.WriteTo(buf[:n], addr); err == nil {
				err = err1
			}
		}
		if err != nil {
			if !isTimeout(err) {
				log.Println(err)
			}
			break
		}
		if err = out.SetReadDeadline(p.nextDeadline()); err != nil {
			log.Println(err)
		}
	}
}

func isTimeout(err error) bool {
	netErr, ok := err.(net.Error)
	return ok && netErr.Timeout()
}

func isErrNetClosing(err error) bool {
	// https://github.com/golang/go/issues/4373
	// https://golang.org/src/internal/poll/fd.go?h=ErrNetClosing
	return strings.HasSuffix(err.Error(), "use of closed network connection")
}
