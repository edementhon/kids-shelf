// Package mdns lets tablets on the home network open kids-shelf by name,
// http://kids-shelf.local:8070/, instead of by the computer's address, which
// a home router may change. It is a small multicast DNS responder (RFC 6762)
// for that one name: it answers A questions with the computer's address on
// the asker's network, both the normal multicast kind (iPads, Macs, Windows,
// Linux) and the one-shot "legacy unicast" kind (section 6.7) that resolvers
// send from a random port. It never answers AAAA: the shelf listens on IPv4.
//
// Before answering, it asks for about a second whether another device on
// the network already answers to the name (section 8 probing). If one
// does, it stays silent and tablets keep using the address. The operating
// system's own responder (mDNSResponder, the Windows DNS client, Avahi)
// keeps running beside it on the same port.
//
// It runs in the supervisor, the only kids-shelf process that listens on
// the home network.
package mdns

import (
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/edementhon/kids-shelf/internal/lanaddr"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/ipv4"
)

// Name is the name kids-shelf answers to on the home network.
const Name = "kids-shelf.local"

const (
	mdnsPort = 5353
	// hostTTL is how long askers may keep the answer (RFC 6762 section 10
	// recommends 120 s for address records); legacyTTL caps the one-shot
	// answers (section 6.7).
	hostTTL   = 120
	legacyTTL = 10
	// The top bit of a question's class asks for a unicast reply (QU); the
	// top bit of an answer's class tells caches to drop older answers for
	// the name (cache flush).
	classTopBit = 0x8000

	probeInterval = 250 * time.Millisecond
	refreshEvery  = time.Minute
	// minMulticastGap rate-limits multicast answers on one network
	// (section 6: at most one per second).
	minMulticastGap = time.Second
	// The deaf-socket check (health): look every checkEvery; after
	// quietAfter without a packet ask itself; replace a deaf socket at most
	// once per minReopenGap.
	checkEvery   = 10 * time.Second
	quietAfter   = 25 * time.Second
	minReopenGap = time.Minute
)

var groupAddr = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: mdnsPort}

// ErrNameTaken reports that another device on the home network already
// answers to Name.
var ErrNameTaken = errors.New("another device on the home network already answers to " + Name)

// errNoNetwork reports that this computer has no home-network address.
var errNoNetwork = errors.New("this computer is not on a home network")

// Responder answers to Name until Close.
type Responder struct {
	name dnsmessage.Name
	logf func(format string, args ...any)

	// sockMu guards the socket, which openSocket may replace while the
	// responder runs, and serializes writes (the multicast interface is a
	// socket option, set before each multicast write).
	sockMu sync.Mutex
	conn   *net.UDPConn
	pc     *ipv4.PacketConn
	joined map[int]bool // interface index -> group joined on this socket
	// lastHeard is when the socket last received any packet (UnixNano).
	lastHeard atomic.Int64

	// probing is set while the start-up probe runs: questions are not
	// answered yet, and answers from other devices are checked for a
	// conflict.
	probing  atomic.Bool
	conflict atomic.Bool

	mu       sync.Mutex
	links    []lanaddr.Addr // the home networks answered on, best first
	lastSent map[sendKey]time.Time

	// send is how answers leave: dst nil means multicast on link's
	// network. Tests replace it.
	send func(link lanaddr.Addr, msg []byte, dst *net.UDPAddr)

	done chan struct{}
	wg   sync.WaitGroup
}

// Start probes for Name and, when no other device answers to it, starts
// answering. A nil Responder comes with an error that says why tablets
// cannot use the name (no home network, the port could not be opened, or
// the name is taken); the shelf still works by address then. logf gets
// the rare events worth a log line.
func Start(logf func(format string, args ...any)) (*Responder, error) {
	links := multicastLinks(lanaddr.All())
	if len(links) == 0 {
		return nil, errNoNetwork
	}
	r := newResponder(logf)
	r.send = r.sendSocket
	if err := r.openSocket(links); err != nil {
		return nil, err
	}
	if err := r.probe(); err != nil {
		r.shutdown(false)
		return nil, err
	}
	r.announce(r.currentLinks())
	r.wg.Add(1)
	go r.maintainLoop()
	return r, nil
}

// openSocket opens the shared UDP 5353 socket, joins the group on links
// and starts reading it, replacing (and closing) the previous socket.
// ListenMulticastUDP binds 0.0.0.0:5353 with the address-reuse options
// each OS needs to share the port with its own responder, and joins the
// group on the best network; setLinks joins the others.
func (r *Responder) openSocket(links []lanaddr.Addr) error {
	conn, err := net.ListenMulticastUDP("udp4", &links[0].Iface, groupAddr)
	if err != nil {
		return err
	}
	pc := ipv4.NewPacketConn(conn)
	// Loopback on, so this computer's own resolver hears the answers and
	// the responder hears itself (the deaf-socket check in maintainLoop);
	// Go turns it off by default.
	if err := pc.SetMulticastLoopback(true); err != nil {
		r.logf("mdns: multicast loopback: %v", err)
	}
	if err := pc.SetMulticastTTL(255); err != nil {
		r.logf("mdns: multicast TTL: %v", err)
	}
	r.sockMu.Lock()
	old := r.conn
	r.conn, r.pc = conn, pc
	r.joined = map[int]bool{links[0].Iface.Index: true}
	r.sockMu.Unlock()
	if old != nil {
		old.Close() // its readLoop ends on net.ErrClosed
	}
	r.lastHeard.Store(time.Now().UnixNano())
	r.setLinks(links)
	r.wg.Add(1)
	go r.readLoop(conn)
	return nil
}

func newResponder(logf func(format string, args ...any)) *Responder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Responder{
		name:     dnsmessage.MustNewName(Name + "."),
		logf:     logf,
		joined:   map[int]bool{},
		lastSent: map[sendKey]time.Time{},
		done:     make(chan struct{}),
	}
}

// Close says goodbye (a zero-TTL answer, so caches forget the address at
// once) and stops answering.
func (r *Responder) Close() error {
	if r == nil {
		return nil
	}
	r.shutdown(true)
	return nil
}

func (r *Responder) shutdown(goodbye bool) {
	select {
	case <-r.done:
		return
	default:
	}
	if goodbye {
		for _, l := range r.currentLinks() {
			if msg, err := r.answer(0, nil, l.IP, 0, true, false); err == nil {
				r.send(l, msg, nil)
			}
		}
	}
	close(r.done)
	r.sockMu.Lock()
	conn := r.conn
	r.sockMu.Unlock()
	if conn != nil {
		conn.Close()
	}
	r.wg.Wait()
}

// multicastLinks keeps the addresses whose interface can carry multicast.
func multicastLinks(all []lanaddr.Addr) []lanaddr.Addr {
	var out []lanaddr.Addr
	for _, a := range all {
		if a.Iface.Flags&net.FlagMulticast != 0 {
			out = append(out, a)
		}
	}
	return out
}

func (r *Responder) setLinks(links []lanaddr.Addr) {
	r.sockMu.Lock()
	for _, l := range links {
		if r.pc == nil || r.joined[l.Iface.Index] {
			continue
		}
		if err := r.pc.JoinGroup(&l.Iface, groupAddr); err != nil {
			r.logf("mdns: listening on %s: %v", l.Iface.Name, err)
			continue
		}
		r.joined[l.Iface.Index] = true
	}
	r.sockMu.Unlock()
	r.mu.Lock()
	r.links = links
	r.mu.Unlock()
}

func (r *Responder) currentLinks() []lanaddr.Addr {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]lanaddr.Addr(nil), r.links...)
}

// probe asks three times, a quarter second apart, whether anyone answers
// to the name, offering this computer's addresses (section 8.1).
func (r *Responder) probe() error {
	r.probing.Store(true)
	defer r.probing.Store(false)
	links := r.currentLinks()
	msg, err := r.probeMessage(links)
	if err != nil {
		return err
	}
	for range 3 {
		for _, l := range links {
			r.send(l, msg, nil)
		}
		time.Sleep(probeInterval)
		if r.conflict.Load() {
			return ErrNameTaken
		}
	}
	time.Sleep(probeInterval)
	if r.conflict.Load() {
		return ErrNameTaken
	}
	return nil
}

// announce tells each network the address, twice a second apart
// (section 8.3), so caches holding an old address update at once.
func (r *Responder) announce(links []lanaddr.Addr) {
	for _, l := range links {
		if msg, err := r.answer(0, nil, l.IP, hostTTL, true, true); err == nil {
			r.send(l, msg, nil)
		}
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		select {
		case <-r.done:
			return
		case <-time.After(time.Second):
		}
		for _, l := range links {
			if msg, err := r.answer(0, nil, l.IP, hostTTL, true, true); err == nil {
				r.send(l, msg, nil)
			}
		}
	}()
}

// maintainLoop follows network changes every refreshEvery (a laptop that
// moves to another Wi-Fi, or gets a new address from the router,
// announces the new one) and checks every checkEvery that the socket still
// hears anything. A socket opened before macOS's "find devices on local
// networks" Allow stays deaf until it is reopened (macOS 15 VM,
// 2026-10-29), so a deaf socket is replaced (see health).
func (r *Responder) maintainLoop() {
	defer r.wg.Done()
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	var h health
	lastRefresh := time.Now()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
		}
		now := time.Now()
		if now.Sub(lastRefresh) >= refreshEvery {
			lastRefresh = now
			r.refreshLinks()
		}
		links := r.currentLinks()
		if len(links) == 0 {
			continue
		}
		switch h.next(now, time.Unix(0, r.lastHeard.Load())) {
		case actAsk:
			r.askSelf(links)
		case actReopen:
			if err := r.openSocket(links); err != nil {
				r.logf("mdns: reopening the socket: %v", err)
				continue
			}
			r.logf("mdns: the socket heard nothing, not even itself; opened a new one")
			r.announce(links)
		}
	}
}

// refreshLinks re-reads the home networks and announces new addresses.
func (r *Responder) refreshLinks() {
	now := multicastLinks(lanaddr.All())
	changed := changedLinks(r.currentLinks(), now)
	r.setLinks(now)
	if len(changed) > 0 {
		r.announce(changed)
	}
}

type action int

const (
	actNone action = iota
	actAsk
	actReopen
)

// health decides when the socket is deaf. Multicast loopback is on, so the
// responder hears its own packets; on a home network it also hears other
// devices every few seconds. After quietAfter without a packet it asks
// itself a question (askSelf); a socket that does not hear even that by
// the next check is deaf and gets replaced, at most once per
// minReopenGap.
type health struct {
	askedAt    time.Time // when it asked itself; zero when not waiting
	reopenedAt time.Time
}

func (h *health) next(now, heard time.Time) action {
	if now.Sub(heard) < quietAfter || (!h.askedAt.IsZero() && heard.After(h.askedAt)) {
		h.askedAt = time.Time{}
		return actNone
	}
	if h.askedAt.IsZero() {
		h.askedAt = now
		return actAsk
	}
	if now.Sub(h.reopenedAt) < minReopenGap {
		return actNone
	}
	h.askedAt = time.Time{}
	h.reopenedAt = now
	return actReopen
}

// askSelf sends, on each network, a question for the name that already
// lists this computer's answer: the responder (and anyone else) stays
// silent (known-answer suppression), but the packet itself reaches the
// socket if it can hear.
func (r *Responder) askSelf(links []lanaddr.Addr) {
	for _, l := range links {
		if msg, err := r.selfQuestion(l.IP); err == nil {
			r.send(l, msg, nil)
		}
	}
}

// changedLinks returns the links in now that are new or carry a new
// address.
func changedLinks(before, now []lanaddr.Addr) []lanaddr.Addr {
	var out []lanaddr.Addr
	for _, n := range now {
		seen := false
		for _, b := range before {
			if b.Iface.Index == n.Iface.Index && b.IP.Equal(n.IP) {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, n)
		}
	}
	return out
}

func (r *Responder) readLoop(conn *net.UDPConn) {
	defer r.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return // shut down, or replaced by openSocket
			}
			// Windows reports an ICMP "port unreachable" for an earlier
			// unicast answer as a read error; keep reading.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		r.lastHeard.Store(time.Now().UnixNano())
		r.handle(buf[:n], src)
	}
}

// handle answers one packet, or checks it for a conflict while probing.
func (r *Responder) handle(pkt []byte, src *net.UDPAddr) {
	var p dnsmessage.Parser
	h, err := p.Start(pkt)
	if err != nil {
		return
	}
	if h.Response {
		if r.probing.Load() {
			r.checkConflict(&p)
		}
		return
	}
	if h.OpCode != 0 || r.probing.Load() {
		return
	}
	var asked []dnsmessage.Question
	positive, negative, unicast := false, false, false
	for {
		q, err := p.Question()
		if err == dnsmessage.ErrSectionDone {
			break
		}
		if err != nil {
			return
		}
		if !r.isOurName(q.Name) {
			continue
		}
		class := q.Class &^ classTopBit
		if class != dnsmessage.ClassINET && class != dnsmessage.ClassANY {
			continue
		}
		switch q.Type {
		case dnsmessage.TypeA, dnsmessage.TypeALL:
			positive = true
		default:
			// The name has no record of this type (AAAA above all). Say
			// so at once (section 6.1): otherwise a lookup that asks for
			// both IPv4 and IPv6 waits for the IPv6 question to time out
			// (5 s on macOS, VM 2026-10-29).
			negative = true
		}
		asked = append(asked, q)
		if q.Class&classTopBit != 0 {
			unicast = true
		}
	}
	if !positive && !negative {
		return
	}
	link, ok := r.linkFor(src.IP)
	if !ok {
		return
	}
	if src.Port != mdnsPort {
		// A one-shot resolver: answer it directly, with its query id and
		// questions, a short TTL and no cache-flush bit (section 6.7). A
		// question about another type only gets an answer with no
		// records, which plain DNS clients read as "no such address".
		var ip net.IP
		if positive {
			ip = link.IP
		}
		if msg, err := r.answer(h.ID, asked, ip, legacyTTL, false, false); err == nil {
			r.send(link, msg, src)
		}
		return
	}
	if positive && r.alreadyKnown(&p, link.IP) {
		// The asker listed our address as known (section 7.1).
		positive = false
		if !negative {
			return
		}
	}
	var ip net.IP
	if positive {
		ip = link.IP
	}
	msg, err := r.answer(0, nil, ip, hostTTL, true, true)
	if err != nil {
		return
	}
	if unicast {
		r.send(link, msg, src)
	}
	if r.multicastDue(link, ip == nil) {
		r.send(link, msg, nil)
	}
}

func (r *Responder) isOurName(n dnsmessage.Name) bool {
	return strings.EqualFold(n.String(), r.name.String())
}

// checkConflict marks a conflict when another device answers to the name
// with an address that is not one of this computer's.
func (r *Responder) checkConflict(p *dnsmessage.Parser) {
	if err := p.SkipAllQuestions(); err != nil {
		return
	}
	answers, err := p.AllAnswers()
	if err != nil {
		return
	}
	for _, a := range answers {
		if !r.isOurName(a.Header.Name) {
			continue
		}
		body, ok := a.Body.(*dnsmessage.AResource)
		if !ok {
			continue
		}
		if !r.isOwnAddress(net.IP(body.A[:])) {
			r.conflict.Store(true)
			return
		}
	}
}

func (r *Responder) isOwnAddress(ip net.IP) bool {
	for _, l := range r.currentLinks() {
		if l.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// linkFor picks the network the asker is on: the one whose prefix holds
// its address, else the one it is (this computer asking itself), else the
// best one.
func (r *Responder) linkFor(src net.IP) (lanaddr.Addr, bool) {
	links := r.currentLinks()
	if len(links) == 0 {
		return lanaddr.Addr{}, false
	}
	for _, l := range links {
		if l.Net != nil && l.Net.Contains(src) {
			return l, true
		}
	}
	for _, l := range links {
		if l.IP.Equal(src) {
			return l, true
		}
	}
	return links[0], true
}

// alreadyKnown reports whether the question lists our answer with at least
// half its TTL left (known-answer suppression, section 7.1).
func (r *Responder) alreadyKnown(p *dnsmessage.Parser, ip net.IP) bool {
	answers, err := p.AllAnswers()
	if err != nil {
		return false
	}
	for _, a := range answers {
		body, ok := a.Body.(*dnsmessage.AResource)
		if ok && r.isOurName(a.Header.Name) && net.IP(body.A[:]).Equal(ip) && a.Header.TTL >= hostTTL/2 {
			return true
		}
	}
	return false
}

// multicastDue applies the one-per-second limit for multicast answers on
// one network, separately for address answers and "no other type"
// answers: a lookup asks for IPv4 and IPv6 a moment apart, and holding back
// either answer would make it wait.
func (r *Responder) multicastDue(link lanaddr.Addr, negative bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	k := sendKey{index: link.Iface.Index, negative: negative}
	if last, ok := r.lastSent[k]; ok && now.Sub(last) < minMulticastGap {
		return false
	}
	r.lastSent[k] = now
	return true
}

// sendKey is what the multicast rate limit counts per: a network and the
// kind of answer.
type sendKey struct {
	index    int
	negative bool
}

// typeNSEC is the DNS record type that lists which types a name has.
const typeNSEC = dnsmessage.Type(47)

// answer builds a response for the name. ip, when set, is its address (an
// A record in the answer section). nsec adds the record saying the name
// has no other type (section 6.1): in the additional section beside an
// address, or as the answer itself without one. A response with neither
// holds no records (a one-shot "no such address"). A non-nil questions
// list is repeated (one-shot answers need it).
func (r *Responder) answer(id uint16, questions []dnsmessage.Question, ip net.IP, ttl uint32, cacheFlush, nsec bool) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 160), dnsmessage.Header{ID: id, Response: true, Authoritative: true})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	for _, q := range questions {
		if err := b.Question(q); err != nil {
			return nil, err
		}
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	class := dnsmessage.ClassINET
	if cacheFlush {
		class |= classTopBit
	}
	hdr := dnsmessage.ResourceHeader{Name: r.name, Class: class, TTL: ttl}
	nsecRR := dnsmessage.UnknownResource{Type: typeNSEC, Data: r.nsecData()}
	if ip != nil {
		var a [4]byte
		copy(a[:], ip.To4())
		if err := b.AResource(hdr, dnsmessage.AResource{A: a}); err != nil {
			return nil, err
		}
		if nsec {
			if err := b.StartAdditionals(); err != nil {
				return nil, err
			}
			if err := b.UnknownResource(hdr, nsecRR); err != nil {
				return nil, err
			}
		}
	} else if nsec {
		if err := b.UnknownResource(hdr, nsecRR); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// nsecData is the NSEC record's data in the form RFC 6762 section 6.1
// uses: the next-name field is the name itself, and the type bitmap
// lists only A (window 0, one bitmap byte, bit 1 set).
func (r *Responder) nsecData() []byte {
	var out []byte
	for _, label := range strings.Split(strings.TrimSuffix(r.name.String(), "."), ".") {
		out = append(out, byte(len(label)))
		out = append(out, label...)
	}
	out = append(out, 0)
	return append(out, 0, 1, 0x40)
}

// probeMessage is the start-up question "who answers to the name?", with
// the addresses this computer proposes in the authority section and the
// unicast-reply bit set (section 8.1).
func (r *Responder) probeMessage(links []lanaddr.Addr) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 128), dnsmessage.Header{})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: r.name, Type: dnsmessage.TypeALL, Class: dnsmessage.ClassINET | classTopBit}); err != nil {
		return nil, err
	}
	if err := b.StartAuthorities(); err != nil {
		return nil, err
	}
	for _, l := range links {
		var a [4]byte
		copy(a[:], l.IP.To4())
		if err := b.AResource(dnsmessage.ResourceHeader{Name: r.name, Class: dnsmessage.ClassINET, TTL: hostTTL}, dnsmessage.AResource{A: a}); err != nil {
			return nil, err
		}
	}
	return b.Finish()
}

// selfQuestion is askSelf's question for the name with ip already listed
// as a known answer at full TTL, so no responder answers it.
func (r *Responder) selfQuestion(ip net.IP) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 128), dnsmessage.Header{})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return nil, err
	}
	if err := b.Question(dnsmessage.Question{Name: r.name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}); err != nil {
		return nil, err
	}
	if err := b.StartAnswers(); err != nil {
		return nil, err
	}
	var a [4]byte
	copy(a[:], ip.To4())
	if err := b.AResource(dnsmessage.ResourceHeader{Name: r.name, Class: dnsmessage.ClassINET, TTL: hostTTL}, dnsmessage.AResource{A: a}); err != nil {
		return nil, err
	}
	return b.Finish()
}

// sendSocket writes msg to dst, or to the multicast group on link's
// network when dst is nil.
func (r *Responder) sendSocket(link lanaddr.Addr, msg []byte, dst *net.UDPAddr) {
	r.sockMu.Lock()
	defer r.sockMu.Unlock()
	if dst != nil {
		if _, err := r.conn.WriteToUDP(msg, dst); err != nil {
			r.logf("mdns: answering %s: %v", dst, err)
		}
		return
	}
	if err := r.pc.SetMulticastInterface(&link.Iface); err != nil {
		r.logf("mdns: choosing %s: %v", link.Iface.Name, err)
		return
	}
	if _, err := r.conn.WriteToUDP(msg, groupAddr); err != nil {
		r.logf("mdns: announcing on %s: %v", link.Iface.Name, err)
	}
}
