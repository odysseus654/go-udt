package udt

import (
	"context"
	"fmt"
	"log"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/odysseus654/go-udt/udt/packet"
)

// packetWrapper is used to explicitly designate the destination of a packet,
// to assist with sending it to its destination
type packetWrapper struct {
	pkt  packet.Packet
	dest *net.UDPAddr
}

/*
A multiplexer multiplexes multiple UDT sockets over a single PacketConn.
*/
type multiplexer struct {
	network       string
	laddr         *net.UDPAddr   // the local address handled by this multiplexer
	conn          net.PacketConn // the UDPConn from which we read/write
	sockets       sync.Map       // the udtSockets handled by this multiplexer, by sockId
	rvSockets     []*udtSocket   // the list of any sockets currently in rendezvous mode
	listenSock    *listener      // the server socket listening to incoming connections, if there is one
	servSockMutex sync.Mutex
	mtu           int    // the Maximum Transmission Unit of packets sent from this address
	nextSid       uint32 // the SockID for the next socket created
	//sendQ        *udtSocketQueue // priority queue of udtSockets awaiting a send (actually includes ones with no packets waiting too)
	pktOut chan packetWrapper // packets queued for immediate sending
	//in chan packetHolder // packets inbound from the PacketConn
	//out             chan packet       // packets outbound to the PacketConn
	//writeBufferPool *bpool.BufferPool // leaky buffer pool for writing to conn
	//readBytePool *bpool.BytePool // leaky byte pool for reading from conn
	shutdown chan struct{}
}

/*
multiplexerFor gets or creates a multiplexer for the given local address.  If a
new multiplexer is created, the given init function is run to obtain an
io.ReadWriter.
*/
func multiplexerFor(ctx context.Context, network string, laddr string) (*multiplexer, error) {
	key := fmt.Sprintf("%s:%s", network, laddr)
	if ifM, ok := multiplexers.Load(key); ok {
		m := ifM.(*multiplexer)
		if m.isLive() { // checking this in case we have a race condition with multiplexer destruction
			return m, nil
		}
	}

	// No multiplexer, need to create connection

	// try to avoid fragmentation (and hopefully be notified if we exceed path MTU)
	config := net.ListenConfig{}
	config.Control = func(network, address string, c syscall.RawConn) error {
		return c.Control(func(fd uintptr) {
			var err error
			os := runtime.GOOS
			switch os {
			case "windows":
				//err = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 14 /* IP_DONTFRAGMENT for winsock2 */, 1)
				err = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 71 /* IP_MTU_DISCOVER for winsock2 */, 2 /* IP_PMTUDISC_DO */)
			case "linux", "android":
				err = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 10 /* IP_MTU_DISCOVER */, 2 /* IP_PMTUDISC_DO */)
			default:
				err = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 67 /* IP_DONTFRAG */, 1)
			}
			if err != nil {
				log.Printf("error on setSockOpt: %s", err.Error())
			}
		})
	}

	//conn, err := net.ListenUDP(network, laddr)
	conn, err := config.ListenPacket(ctx, network, laddr)
	if err != nil {
		return nil, err
	}

	addr := conn.LocalAddr().(*net.UDPAddr)

	m := newMultiplexer(network, addr, conn)
	multiplexers.Store(key, m)
	return m, nil
}

func newMultiplexer(network string, laddr *net.UDPAddr, conn net.PacketConn) (m *multiplexer) {
	mtu, _ := discoverMTU(laddr.IP)
	m = &multiplexer{
		network: network,
		laddr:   laddr,
		conn:    conn,
		mtu:     mtu,
		nextSid: randUint32(),
		//sendQ:        newUdtSocketQueue(),
		pktOut: make(chan packetWrapper, 100), // todo: figure out how to size this
		//in:           make(chan packetHolder, 100),  // todo: make this tunable
		//out:             make(chan packet, 100),                         // todo: make this tunable
		//writeBufferPool: bpool.NewBufferPool(25600), // todo: make this tunable
		//readBytePool:    bpool.NewBytePool(25600, mtu), // todo: make this tunable
		shutdown: make(chan struct{}, 1),
	}

	go m.goReceiveTick()
	go m.goRead()
	go m.goWrite()

	return
}

func (m *multiplexer) key() string {
	return fmt.Sprintf("%s:%s", m.network, m.laddr.String())
}

func (m *multiplexer) listenUDT(l *listener) bool {
	m.servSockMutex.Lock()
	defer m.servSockMutex.Unlock()
	if m.listenSock != nil {
		return false
	}
	m.listenSock = l
	return true
}

func (m *multiplexer) unlistenUDT(l *listener) bool {
	m.servSockMutex.Lock()
	if m.listenSock != l {
		m.servSockMutex.Unlock()
		return false
	}
	m.listenSock = nil
	m.servSockMutex.Unlock()
	m.checkLive()
	return true
}

// Adapted from https://github.com/hlandau/degoutils/blob/master/net/mtu.go
const absMaxDatagramSize = 2147483646 // 2**31-2
func discoverMTU(ourIP net.IP) (int, error) {

	ifaces, err := net.Interfaces()
	if err != nil {
		return 65535, err
	}

	var filtered []net.Interface
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			log.Printf("cannot retrieve iface addresses for %s: %s", iface.Name, err.Error())
			continue
		}
		for _, a := range addrs {
			var ipnet *net.IPNet
			switch v := a.(type) {
			case *net.IPAddr:
				ipnet = &net.IPNet{v.IP, v.IP.DefaultMask()}
			case *net.IPNet:
				ipnet = v
			}
			if ipnet == nil {
				log.Printf("cannot retrieve IPNet from address %s on interface %s", a.String(), iface.Name)
				continue
			}
			if ipnet.Contains(ourIP) {
				filtered = append(filtered, iface)
			}
		}
	}
	if len(filtered) == 0 {
		log.Printf("cannot identify interface(s) associated with %s, doing blind search", ourIP.String())
		filtered = ifaces
	}

	var mtu int = 65535
	for _, iface := range filtered {
		if iface.Flags&(net.FlagUp|net.FlagLoopback) == net.FlagUp && iface.MTU > mtu {
			mtu = iface.MTU
		}
	}
	if mtu > absMaxDatagramSize {
		mtu = absMaxDatagramSize
	}
	return mtu, nil
}

func (m *multiplexer) newSocket(peer *net.UDPAddr, isServer bool) (s *udtSocket, err error) {
	sid := atomic.AddUint32(&m.nextSid, ^uint32(0))

	s, err = newSocket(m, sid, isServer, peer)
	if err != nil {
		return nil, err
	}

	m.sockets.Store(sid, s)
	return s, nil
}

func (m *multiplexer) closeSocket(sockID uint32) bool {
	if s, ok := m.sockets.Load(sockID); !ok {
		return false
	}
	m.sockets.Delete(sockID)
	m.checkLive()
	return true
}

func (m *multiplexer) checkLive() bool {
	if m.conn == nil { // have we already been destructed ?
		return false
	}
	if m.isLive() { // are we currently in use?
		return true
	}

	// deregister this multiplexer
	key := m.key()
	multiplexers.Delete(key)
	if m.isLive() { // checking this in case we have a race condition with multiplexer destruction
		multiplexers.Store(key, m)
		return true
	}

	// tear everything down
	m.conn.Close()
	m.conn = nil
	close(m.pktOut)
	m.pktOut = nil
	close(m.shutdown)
	m.shutdown = nil
	return false
}

func (m *multiplexer) isLive() bool {
	if m.conn == nil {
		return false
	}
	m.servSockMutex.Lock()
	defer m.servSockMutex.Unlock()

	if m.listenSock != nil {
		return true
	}
	if m.rvSockets != nil {
		if len(m.rvSockets) > 0 {
			return true
		}
	}

	isEmpty := true
	m.sockets.Range(func(key, val interface{}) bool {
		isEmpty = false
		return false
	})
	return !isEmpty
}

/*
read runs in a goroutine and reads packets from conn using a buffer from the
readBufferPool, or a new buffer.
*/
func (m *multiplexer) goRead() {
	buf := make([]byte, m.mtu)
	for {
		numBytes, from, err := m.conn.ReadFrom(buf)
		if err != nil {
			log.Printf("Unable to read into buffer: %s", err)
			continue
		}

		p, err := packet.ReadPacketFrom(buf[0:numBytes])
		if err != nil {
			log.Printf("Unable to read packet: %s", err)
			continue
		}

		// attempt to route the packet
		sockID := p.SocketID()
		if sockID == 0 {
			var hsPacket *packet.HandshakePacket
			var ok bool
			if hsPacket, ok = p.(*packet.HandshakePacket); !ok {
				log.Printf("Received non-handshake packet with destination socket = 0")
				continue
			}

			m.servSockMutex.Lock()
			if m.rvSockets != nil {
				foundMatch := false
				for _, sock := range m.rvSockets {
					if sock.readHandshake(m, hsPacket, from.(*net.UDPAddr)) {
						foundMatch = true
						break
					}
				}
				if foundMatch {
					m.servSockMutex.Unlock()
					continue
				}
			}
			if m.listenSock != nil {
				m.listenSock.readHandshake(m, hsPacket, from.(*net.UDPAddr))
				m.servSockMutex.Unlock()
			}
		}
		if ifDestSock, ok := m.sockets.Load(sockID); ok {
			ifDestSock.(*udtSocket).readPacket(m, p, from.(*net.UDPAddr))
		}
	}
}

/*
write runs in a goroutine and writes packets to conn using a buffer from the
writeBufferPool, or a new buffer.
*/
func (m *multiplexer) goWrite() {
	buf := make([]byte, m.mtu)
	pktOut := m.pktOut
	for {
		select {
		case pw, ok := <-pktOut:
			if !ok {
				return
			}
			plen, err := pw.pkt.WriteTo(buf)
			if err != nil {
				// TODO: handle write error
				log.Fatalf("Unable to buffer out: %s", err.Error())
				continue
			}

			log.Printf("Writing to %s", pw.pkt.SocketID())
			if _, err = m.conn.WriteTo(buf[0:plen], pw.dest); err != nil {
				// TODO: handle write error
				log.Fatalf("Unable to write out: %s", err.Error())
			}
		}
	}
}

func (m *multiplexer) sendPacket(destAddr *net.UDPAddr, destSockID uint32, ts uint32, p packet.Packet) error {
	p.SetHeader(destSockID, ts)
	m.pktOut <- packetWrapper{pkt: p, dest: destAddr}
	return nil
}

// goReceiveTick runs in a goroutine and handles any receiving socket alarms
func (m *multiplexer) goReceiveTick() {
	ticker := time.NewTicker(10 * time.Millisecond) // SYN = 0.01s
	shutdown := m.shutdown
	for {
		select {
		case _, ok := <-shutdown:
			ticker.Stop()
			return
		case tm := <-ticker.C:
			m.sockets.Range(func(key, val interface{}) bool {
				val.(*udtSocket).onReceiveTick(m, tm)
				return true
			})
		}
	}
}
