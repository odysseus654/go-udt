package udt

import (
	"errors"
	"log"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/odysseus654/go-udt/udt/packet"
)

type sockState int

const (
	sockStateInit       sockState = iota // object is being constructed
	sockStateRendezvous                  // attempting to create a rendezvous connection
	sockStateConnecting                  // attempting to create a connection
	sockStateConnected                   // connection is established
	sockStateClosed                      // connection has been closed (by either end)
	sockStateRefused                     // connection rejected by remote host
	sockStateCorrupted                   // peer behaved in an improper manner
	sockStateTimeout                     // connection failed due to peer timeout
)

type recvPktEvent struct {
	pkt packet.Packet
	now time.Time
}

type sendMessage struct {
	content []byte
	tim     time.Time     // time message is submitted
	ttl     time.Duration // message dropped if it can't be sent in this timeframe
}

type shutdownMessage struct {
	sockState    sockState
	permitLinger bool
	err          error
}

/*
udtSocket encapsulates a UDT socket between a local and remote address pair, as
defined by the UDT specification.  udtSocket implements the net.Conn interface
so that it can be used anywhere that a stream-oriented network connection
(like TCP) would be used.
*/
type udtSocket struct {
	// this data not changed after the socket is initialized and/or handshaked
	m           *multiplexer    // the multiplexer that handles this socket
	raddr       *net.UDPAddr    // the remote address
	created     time.Time       // the time that this socket was created
	Config      *Config         // configuration parameters for this socket
	udtVer      int             // UDT protcol version (normally 4.  Will we be supporting others?)
	isDatagram  bool            // if true then we're sending and receiving datagrams, otherwise we're a streaming socket
	isServer    bool            // if true then we are behaving like a server, otherwise client (or rendezvous). Only useful during handshake
	sockID      uint32          // our sockID
	farSockID   uint32          // the peer's sockID
	initPktSeq  packet.PacketID // initial packet sequence to start the connection with
	connectWait *sync.WaitGroup // released when connection is complete (or failed)

	sockState           sockState    // socket state - used mostly during handshakes
	mtu                 atomicUint32 // the negotiated maximum packet size
	maxFlowWinSize      uint         // receiver: maximum unacknowledged packet count
	currPartialRead     []byte       // stream connections: currently reading message (for partial reads). Owned by client caller (Read)
	readDeadline        *time.Timer  // if set, then calls to Read() will return "timeout" after this time
	readDeadlinePassed  bool         // if set, then calls to Read() will return "timeout"
	writeDeadline       *time.Timer  // if set, then calls to Write() will return "timeout" after this time
	writeDeadlinePassed bool         // if set, then calls to Write() will return "timeout"

	rttProt sync.RWMutex // lock must be held before referencing rtt/rttVar
	rtt     uint         // receiver: estimated roundtrip time. (in microseconds)
	rttVar  uint         // receiver: roundtrip variance. (in microseconds)

	receiveRateProt sync.RWMutex // lock must be held before referencing deliveryRate/bandwidth
	deliveryRate    uint         // delivery rate reported from peer (packets/sec)
	bandwidth       uint         // bandwidth reported from peer (packets/sec)

	// channels
	messageIn     chan []byte          // inbound messages. Sender is goReceiveEvent->ingestData, Receiver is client caller (Read)
	messageOut    chan sendMessage     // outbound messages. Sender is client caller (Write), Receiver is goSendEvent. Closed when socket is closed
	recvEvent     chan recvPktEvent    // receiver: ingest the specified packet. Sender is readPacket, receiver is goReceiveEvent
	sendEvent     chan recvPktEvent    // sender: ingest the specified packet. Sender is readPacket, receiver is goSendEvent
	sendPacket    chan packet.Packet   // packets to send out on the wire (once goManageConnection is running)
	shutdownEvent chan shutdownMessage // channel signals the connection to be shutdown
	sockShutdown  chan struct{}        // closed when socket is shutdown
	sockClosed    chan struct{}        // closed when socket is closed

	// timers
	connTimeout <-chan time.Time // connecting: fires when connection attempt times out
	connRetry   <-chan time.Time // connecting: fires when connection attempt to be retried
	lingerTimer <-chan time.Time // after disconnection, fires once our linger timer runs out

	send *udtSocketSend // reference to sending side of this socket
	recv *udtSocketRecv // reference to receiving side of this socket
	cong *udtSocketCc   // reference to contestion control

	// performance metrics
	//PktSent      uint64        // number of sent data packets, including retransmissions
	//PktRecv      uint64        // number of received packets
	//PktSndLoss   uint          // number of lost packets (sender side)
	//PktRcvLoss   uint          // number of lost packets (receiver side)
	//PktRetrans   uint          // number of retransmitted packets
	//PktSentACK   uint          // number of sent ACK packets
	//PktRecvACK   uint          // number of received ACK packets
	//PktSentNAK   uint          // number of sent NAK packets
	//PktRecvNAK   uint          // number of received NAK packets
	//MbpsSendRate float64       // sending rate in Mb/s
	//MbpsRecvRate float64       // receiving rate in Mb/s
	//SndDuration  time.Duration // busy sending time (i.e., idle time exclusive)

	// instant measurements
	//PktSndPeriod        time.Duration // packet sending period
	//PktFlowWindow       uint          // flow window size, in number of packets
	//PktCongestionWindow uint          // congestion window size, in number of packets
	//PktFlightSize       uint          // number of packets on flight
	//MsRTT               time.Duration // RTT
	//MbpsBandwidth       float64       // estimated bandwidth, in Mb/s
	//ByteAvailSndBuf     uint          // available UDT sender buffer size
	//ByteAvailRcvBuf     uint          // available UDT receiver buffer size
}

/*******************************************************************************
 Implementation of net.Conn interface
*******************************************************************************/

// Grab the next data packet
func (s *udtSocket) fetchReadPacket(blocking bool) ([]byte, error) {
	var result []byte
	if blocking {
		for {
			if s.readDeadlinePassed {
				return nil, syscall.ETIMEDOUT
			}
			var deadline <-chan time.Time
			if s.readDeadline != nil {
				deadline = s.readDeadline.C
			}
			select {
			case result = <-s.messageIn:
				return result, nil
			case _, ok := <-deadline:
				if !ok {
					continue
				}
				s.readDeadlinePassed = true
				return nil, syscall.ETIMEDOUT
			}
		}
	}

	select {
	case result = <-s.messageIn:
		// ok we have a message
	default:
		// ok we've read some stuff and there's nothing immediately available
		return nil, nil
	}
	return result, nil
}

func (s *udtSocket) connectionError() error {
	switch s.sockState {
	case sockStateRefused:
		return errors.New("Connection refused by remote host")
	case sockStateCorrupted:
		return errors.New("Connection closed due to protocol error")
	case sockStateClosed:
		return errors.New("Connection closed")
	case sockStateTimeout:
		return errors.New("Connection timed out")
	}
	return nil
}

// TODO: int sendmsg(const char* data, int len, int msttl, bool inorder)

// Read reads data from the connection.
// Read can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetReadDeadline.
// (required for net.Conn implementation)
func (s *udtSocket) Read(p []byte) (n int, err error) {
	connErr := s.connectionError()
	if s.isDatagram {
		// for datagram sockets, block until we have a message to return and then return it
		// if the buffer isn't big enough, return a truncated message (discarding the rest) and return an error
		msg, rerr := s.fetchReadPacket(connErr == nil)
		if rerr != nil {
			err = rerr
			return
		}
		if msg == nil && connErr != nil {
			err = connErr
			return
		}
		n = copy(p, msg)
		if n < len(msg) {
			err = errors.New("Message truncated")
		}
	} else {
		// for streaming sockets, block until we have at least something to return, then
		// fill up the passed buffer as far as we can without blocking again
		idx := 0
		l := len(p)
		n = 0
		for idx < l {
			if s.currPartialRead == nil {
				// Grab the next data packet
				currPartialRead, rerr := s.fetchReadPacket(n == 0 && connErr == nil)
				s.currPartialRead = currPartialRead
				if rerr != nil {
					err = rerr
					return
				}
				if s.currPartialRead == nil {
					if n != 0 {
						return
					}
					if connErr != nil {
						err = connErr
						return
					}
				}
			}
			thisN := copy(p[idx:], s.currPartialRead)
			n = n + thisN
			idx = idx + thisN
			if n >= len(s.currPartialRead) {
				// we've exhausted the current data packet, reset to nil
				s.currPartialRead = nil
			} else {
				s.currPartialRead = s.currPartialRead[n:]
			}
		}
	}
	return
}

// Write writes data to the connection.
// Write can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
// (required for net.Conn implementation)
func (s *udtSocket) Write(p []byte) (n int, err error) {
	// at the moment whatever we have right now we'll shove it into a channel and return
	// on the other side:
	//  for datagram sockets: this is a distinct message to be broken into as few packets as possible
	//  for streaming sockets: collect as much as can fit into a packet and send them out
	switch s.sockState {
	case sockStateRefused:
		err = errors.New("Connection refused by remote host")
		return
	case sockStateCorrupted:
		err = errors.New("Connection closed due to protocol error")
		return
	case sockStateClosed:
		err = errors.New("Connection closed")
		return
	}

	n = len(p)

	for {
		if s.writeDeadlinePassed {
			err = syscall.ETIMEDOUT
			return
		}
		var deadline <-chan time.Time
		if s.writeDeadline != nil {
			deadline = s.writeDeadline.C
		}
		select {
		case s.messageOut <- sendMessage{content: p, tim: time.Now()}:
			// send successful
			return
		case _, ok := <-deadline:
			if !ok {
				continue
			}
			s.writeDeadlinePassed = true
			err = syscall.ETIMEDOUT
			return
		}
	}
}

// Close closes the connection.
// Any blocked Read or Write operations will be unblocked.
// Write operations will be permitted to send (initial packets)
// Read operations will return an error
// (required for net.Conn implementation)
func (s *udtSocket) Close() error {
	if !s.isOpen() {
		return nil // already closed
	}

	close(s.messageOut)
	_, _ = <-s.shutdownEvent
	return nil
}

func (s *udtSocket) isOpen() bool {
	switch s.sockState {
	case sockStateClosed, sockStateRefused, sockStateCorrupted, sockStateTimeout:
		return false
	default:
		return true
	}
}

// LocalAddr returns the local network address.
// (required for net.Conn implementation)
func (s *udtSocket) LocalAddr() net.Addr {
	return s.m.laddr
}

// RemoteAddr returns the remote network address.
// (required for net.Conn implementation)
func (s *udtSocket) RemoteAddr() net.Addr {
	return s.raddr
}

// SetDeadline sets the read and write deadlines associated
// with the connection. It is equivalent to calling both
// SetReadDeadline and SetWriteDeadline.
//
// A deadline is an absolute time after which I/O operations
// fail with a timeout (see type Error) instead of
// blocking. The deadline applies to all future and pending
// I/O, not just the immediately following call to Read or
// Write. After a deadline has been exceeded, the connection
// can be refreshed by setting a deadline in the future.
//
// An idle timeout can be implemented by repeatedly extending
// the deadline after successful Read or Write calls.
//
// A zero value for t means I/O operations will not time out.
//
// Note that if a TCP connection has keep-alive turned on,
// which is the default unless overridden by Dialer.KeepAlive
// or ListenConfig.KeepAlive, then a keep-alive failure may
// also return a timeout error. On Unix systems a keep-alive
// failure on I/O can be detected using
// errors.Is(err, syscall.ETIMEDOUT).
// (required for net.Conn implementation)
func (s *udtSocket) SetDeadline(t time.Time) error {
	s.setDeadline(t, &s.readDeadline, &s.readDeadlinePassed)
	s.setDeadline(t, &s.writeDeadline, &s.writeDeadlinePassed)
	return nil
}

func (s *udtSocket) setDeadline(dl time.Time, timer **time.Timer, timerPassed *bool) {
	if *timer == nil {
		if !dl.IsZero() {
			*timer = time.NewTimer(dl.Sub(time.Now()))
		}
	} else {
		now := time.Now()
		if !dl.IsZero() && dl.Before(now) {
			*timerPassed = true
		}
		oldTime := *timer
		if dl.IsZero() {
			*timer = nil
		}
		oldTime.Stop()
		_, _ = <-oldTime.C
		if !dl.IsZero() && dl.After(now) {
			*timerPassed = false
			oldTime.Reset(dl.Sub(time.Now()))
		}
	}
}

// SetReadDeadline sets the deadline for future Read calls
// and any currently-blocked Read call.
// A zero value for t means Read will not time out.
// (required for net.Conn implementation)
func (s *udtSocket) SetReadDeadline(t time.Time) error {
	s.setDeadline(t, &s.readDeadline, &s.readDeadlinePassed)
	return nil
}

// SetWriteDeadline sets the deadline for future Write calls
// and any currently-blocked Write call.
// Even if write times out, it may return n > 0, indicating that
// some of the data was successfully written.
// A zero value for t means Write will not time out.
// (required for net.Conn implementation)
func (s *udtSocket) SetWriteDeadline(t time.Time) error {
	s.setDeadline(t, &s.writeDeadline, &s.writeDeadlinePassed)
	return nil
}

/*******************************************************************************
 Private functions
*******************************************************************************/

// newSocket creates a new UDT socket, which will be configured afterwards as either an incoming our outgoing socket
func newSocket(m *multiplexer, config *Config, sockID uint32, isServer bool, isDatagram bool, raddr *net.UDPAddr) (s *udtSocket) {
	now := time.Now()

	mtu := m.mtu
	if config.MaxPacketSize > 0 && config.MaxPacketSize < mtu {
		mtu = config.MaxPacketSize
	}

	maxFlowWinSize := config.MaxFlowWinSize
	if maxFlowWinSize == 0 {
		maxFlowWinSize = DefaultConfig().MaxFlowWinSize
	}
	if maxFlowWinSize < 32 {
		maxFlowWinSize = 32
	}

	s = &udtSocket{
		m:              m,
		Config:         config,
		raddr:          raddr,
		created:        now,
		sockState:      sockStateInit,
		udtVer:         4,
		isServer:       isServer,
		mtu:            atomicUint32{val: uint32(mtu)},
		maxFlowWinSize: maxFlowWinSize,
		isDatagram:     isDatagram,
		sockID:         sockID,
		initPktSeq:     packet.PacketID{Seq: randUint32()},
		messageIn:      make(chan []byte, 256),
		messageOut:     make(chan sendMessage, 256),
		recvEvent:      make(chan recvPktEvent, 256),
		sendEvent:      make(chan recvPktEvent, 256),
		sockClosed:     make(chan struct{}, 1),
		sockShutdown:   make(chan struct{}, 1),
		deliveryRate:   16,
		bandwidth:      1,
		sendPacket:     make(chan packet.Packet, 256),
		shutdownEvent:  make(chan shutdownMessage, 5),
	}
	s.cong = newUdtSocketCc(s)

	return
}

func (s *udtSocket) launchProcessors() {
	s.send = newUdtSocketSend(s)
	s.recv = newUdtSocketRecv(s)
	s.cong.init(s.initPktSeq)
}

func (s *udtSocket) startConnect() error {

	connectWait := &sync.WaitGroup{}
	s.connectWait = connectWait
	connectWait.Add(1)

	s.sockState = sockStateConnecting

	s.connTimeout = time.After(3 * time.Second)
	s.connRetry = time.After(250 * time.Millisecond)
	go s.goManageConnection()

	s.sendHandshake(0, packet.HsRequest)

	connectWait.Wait()
	return s.connectionError()
}

func (s *udtSocket) startRendezvous() error {
	connectWait := &sync.WaitGroup{}
	s.connectWait = connectWait
	s.connectWait.Add(1)

	s.sockState = sockStateRendezvous

	s.connTimeout = time.After(30 * time.Second)
	s.connRetry = time.After(250 * time.Millisecond)
	go s.goManageConnection()

	s.m.startRendezvous(s)
	s.sendHandshake(0, packet.HsRendezvous)

	connectWait.Wait()
	return s.connectionError()
}

func (s *udtSocket) goManageConnection() {
	sockClosed := s.sockClosed
	sockShutdown := s.sockShutdown
	for {
		select {
		case <-s.lingerTimer: // linger timer expired, shut everything down
			s.m.closeSocket(s.sockID)
			close(s.sockClosed)
			return
		case _, _ = <-sockShutdown:
			// catching this to force re-evaluation of this select (catching the linger timer)
		case _, _ = <-sockClosed:
			return
		case p := <-s.sendPacket:
			ts := uint32(time.Now().Sub(s.created) / time.Microsecond)
			s.cong.onPktSent(p)
			log.Printf("%s (id=%d) sending %s to %s (id=%d)", s.m.laddr.String(), s.sockID, packet.PacketTypeName(p.PacketType()),
				s.raddr.String(), s.farSockID)
			s.m.sendPacket(s.raddr, s.farSockID, ts, p)
		case sd := <-s.shutdownEvent: // connection shut down
			s.shutdown(sd.sockState, sd.permitLinger, sd.err)
		case <-s.connTimeout: // connection timed out
			s.shutdown(sockStateTimeout, true, nil)
		case <-s.connRetry: // resend connection attempt
			s.connRetry = nil
			switch s.sockState {
			case sockStateConnecting:
				s.sendHandshake(0, packet.HsRequest)
				s.connRetry = time.After(250 * time.Millisecond)
			case sockStateRendezvous:
				s.sendHandshake(0, packet.HsRendezvous)
				s.connRetry = time.After(250 * time.Millisecond)
			}
		}
	}
}

func (s *udtSocket) sendHandshake(synCookie uint32, reqType packet.HandshakeReqType) {
	sockType := packet.TypeSTREAM
	if s.isDatagram {
		sockType = packet.TypeDGRAM
	}

	p := &packet.HandshakePacket{
		UdtVer:         uint32(s.udtVer),
		SockType:       sockType,
		InitPktSeq:     s.initPktSeq,
		MaxPktSize:     s.mtu.get(),              // maximum packet size (including UDP/IP headers)
		MaxFlowWinSize: uint32(s.maxFlowWinSize), // maximum flow window size
		ReqType:        reqType,
		SockID:         s.sockID,
		SynCookie:      synCookie,
		SockAddr:       s.raddr.IP,
	}

	ts := uint32(time.Now().Sub(s.created) / time.Microsecond)
	s.cong.onPktSent(p)
	log.Printf("%s (id=%d) sending handshake(%d) to %s (id=%d)", s.m.laddr.String(), s.sockID, int(reqType),
		s.raddr.String(), s.farSockID)
	s.m.sendPacket(s.raddr, s.farSockID, ts, p)
}

// checkValidHandshake checks to see if we want to accept a new connection with this handshake.
func (s *udtSocket) checkValidHandshake(m *multiplexer, p *packet.HandshakePacket, from *net.UDPAddr) bool {
	if s.udtVer != 4 {
		return false
	}
	return true
}

// readHandshake is received when a handshake packet is received without a destination, either as part
// of a listening response or as a rendezvous connection
func (s *udtSocket) readHandshake(m *multiplexer, p *packet.HandshakePacket, from *net.UDPAddr) bool {
	if !from.IP.Equal(s.raddr.IP) || from.Port != s.raddr.Port {
		log.Printf("huh? initted with %s but handshake with %s", s.raddr.String(), from.String())
		return false
	}

	switch s.sockState {
	case sockStateInit: // server accepting a connection from a client
		s.initPktSeq = p.InitPktSeq
		s.udtVer = int(p.UdtVer)
		s.farSockID = p.SockID
		s.isDatagram = p.SockType == packet.TypeDGRAM

		if s.mtu.get() > p.MaxPktSize {
			s.mtu.set(p.MaxPktSize)
		}
		s.launchProcessors()
		s.recv.configureHandshake(p)
		s.send.configureHandshake(p, true)
		s.sockState = sockStateConnected
		s.connTimeout = nil
		s.connRetry = nil
		go s.goManageConnection()

		s.sendHandshake(p.SynCookie, packet.HsResponse)
		return true

	case sockStateConnecting: // client attempting to connect to server
		if p.ReqType == packet.HsRefused {
			s.sockState = sockStateRefused
			return true
		}
		if p.ReqType == packet.HsRequest {
			if !s.checkValidHandshake(m, p, from) || p.InitPktSeq != s.initPktSeq || !from.IP.Equal(s.raddr.IP) || from.Port != s.raddr.Port || s.isDatagram != (p.SockType == packet.TypeDGRAM) {
				// ignore, not a valid handshake request
				return true
			}
			// handshake isn't done yet, send it back with the cookie we received
			s.sendHandshake(p.SynCookie, packet.HsResponse)
			return true
		}
		if p.ReqType != packet.HsResponse {
			// unexpected packet type, ignore
			return true
		}
		if !s.checkValidHandshake(m, p, from) || p.InitPktSeq != s.initPktSeq || !from.IP.Equal(s.raddr.IP) || from.Port != s.raddr.Port || s.isDatagram != (p.SockType == packet.TypeDGRAM) {
			// ignore, not a valid handshake request
			return true
		}
		s.farSockID = p.SockID

		if s.mtu.get() > p.MaxPktSize {
			s.mtu.set(p.MaxPktSize)
		}
		s.launchProcessors()
		s.recv.configureHandshake(p)
		s.send.configureHandshake(p, true)
		s.connRetry = nil
		s.sockState = sockStateConnected
		s.connTimeout = nil
		if s.connectWait != nil {
			s.connectWait.Done()
			s.connectWait = nil
		}
		return true

	case sockStateRendezvous: // client attempting to rendezvous with another client
		if p.ReqType == packet.HsRefused {
			s.sockState = sockStateRefused
			return true
		}
		if p.ReqType != packet.HsRendezvous || s.farSockID == 0 {
			return true // not a request packet, ignore
		}
		if !s.checkValidHandshake(m, p, from) || !from.IP.Equal(s.raddr.IP) || from.Port != s.raddr.Port || s.isDatagram != (p.SockType == packet.TypeDGRAM) {
			// not a compatible handshake, ignore
			return true
		}
		/* not quite sure how to negotiate this, assuming split-brain for now
		if p.InitPktSeq != s.initPktSeq {
			s.sockState = sockStateCorrupted
			return true
		}
		*/
		s.farSockID = p.SockID
		s.m.endRendezvous(s)

		if s.mtu.get() > p.MaxPktSize {
			s.mtu.set(p.MaxPktSize)
		}
		s.launchProcessors()
		s.recv.configureHandshake(p)
		s.send.configureHandshake(p, false)
		s.connRetry = nil
		s.sockState = sockStateConnected
		s.connTimeout = nil
		if s.connectWait != nil {
			s.connectWait.Done()
			s.connectWait = nil
		}

		// send the final rendezvous packet
		s.sendHandshake(p.SynCookie, packet.HsResponse)
		return true

	case sockStateConnected: // server repeating a handshake to a client
		if s.isServer && p.ReqType == packet.HsRequest {
			// client didn't receive our response handshake, resend it
			s.sendHandshake(p.SynCookie, packet.HsResponse)
		} else if !s.isServer && p.ReqType == packet.HsResponse {
			// this is a rendezvous connection (re)send our response
			s.sendHandshake(p.SynCookie, packet.HsResponse2)
		}
		return true
	}

	return false
}

func (s *udtSocket) shutdown(sockState sockState, permitLinger bool, err error) {
	if !s.isOpen() {
		return // already closed
	}
	if err != nil {
		log.Printf("socket shutdown (type=%d), due to error: %s", int(sockState), err.Error())
	} else {
		log.Printf("socket shutdown (type=%d)", int(sockState))
	}
	if s.sockState == sockStateRendezvous {
		s.m.endRendezvous(s)
	}
	if s.connectWait != nil {
		s.connectWait.Done()
		s.connectWait = nil
	}
	s.sockState = sockState
	s.cong.close()

	if permitLinger {
		linger := s.Config.LingerTime
		if linger == 0 {
			linger = DefaultConfig().LingerTime
		}
		s.lingerTimer = time.After(linger)
	}

	s.connTimeout = nil
	s.connRetry = nil
	if permitLinger {
		close(s.sockShutdown)
	} else {
		s.m.closeSocket(s.sockID)
		close(s.sockClosed)
	}
	s.messageIn <- nil
}

func absdiff(a uint, b uint) uint {
	if a < b {
		return b - a
	}
	return a - b
}

func (s *udtSocket) applyRTT(rtt uint) {
	s.rttProt.Lock()
	s.rttVar = (s.rttVar*3 + absdiff(s.rtt, rtt)) >> 2
	s.rtt = (s.rtt*7 + rtt) >> 3
	s.rttProt.Unlock()
}

func (s *udtSocket) getRTT() (rtt, rttVar uint) {
	s.rttProt.RLock()
	rtt = s.rtt
	rttVar = s.rttVar
	s.rttProt.RUnlock()
	return
}

// Update Estimated Bandwidth and packet delivery rate
func (s *udtSocket) applyReceiveRates(deliveryRate uint, bandwidth uint) {
	s.receiveRateProt.Lock()
	if deliveryRate > 0 {
		s.deliveryRate = (s.deliveryRate*7 + deliveryRate) >> 3
	}
	if bandwidth > 0 {
		s.bandwidth = (s.bandwidth*7 + bandwidth) >> 3
	}
	s.receiveRateProt.Unlock()
}

func (s *udtSocket) getRcvSpeeds() (deliveryRate uint, bandwidth uint) {
	s.receiveRateProt.RLock()
	deliveryRate = s.deliveryRate
	bandwidth = s.bandwidth
	s.receiveRateProt.RUnlock()
	return
}

// called by the multiplexer read loop when a packet is received for this socket.
// Minimal processing is permitted but try not to stall the caller
func (s *udtSocket) readPacket(m *multiplexer, p packet.Packet, from *net.UDPAddr) {
	now := time.Now()
	if s.sockState == sockStateClosed {
		return
	}
	if !from.IP.Equal(s.raddr.IP) || from.Port != s.raddr.Port {
		log.Printf("Socket connected to %s received a packet from %s? Discarded", s.raddr.String(), from.String())
		return
	}

	s.recvEvent <- recvPktEvent{pkt: p, now: now}

	switch sp := p.(type) {
	case *packet.HandshakePacket: // sent by both peers
		s.readHandshake(m, sp, from)
	case *packet.ShutdownPacket: // sent by either peer
		s.shutdownEvent <- shutdownMessage{sockState: sockStateClosed, permitLinger: true}
	case *packet.AckPacket, *packet.LightAckPacket, *packet.NakPacket: // receiver -> sender
		s.sendEvent <- recvPktEvent{pkt: p, now: now}
	case *packet.UserDefControlPacket:
		s.cong.onCustomMsg(*sp)
	}
}
