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

	conn *net.UDPConn
	pc   *ipv4.PacketConn

	// probing is set while the start-up probe runs: questions are not
	// answered yet, and answers from other devices are checked for a
	// conflict.
	probing  atomic.Bool
	conflict atomic.Bool

	mu       sync.Mutex
	links    []lanaddr.Addr // the home networks answered on, best first
	joined   map[int]bool   // interface index -> group joined
	lastSent map[int]time.Time

	// send is how answers leave: dst nil means multicast on link's
	// network. Tests replace it.
	send func(link lanaddr.Addr, msg []byte, dst *net.UDPAddr)
	wmu  sync.Mutex // the multicast interface option and the write

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
	// ListenMulticastUDP binds 0.0.0.0:5353 with the address-reuse options
	// each OS needs to share the port with its own responder, and joins
	// the group on the best network; the others are joined below.
	conn, err := net.ListenMulticastUDP("udp4", &links[0].Iface, groupAddr)
	if err != nil {
		return nil, err
	}
	r.conn = conn
	r.pc = ipv4.NewPacketConn(conn)
	// Loopback on, so this computer's own resolver (and a test from this
	// computer) hears the answers too; Go turns it off by default.
	if err := r.pc.SetMulticastLoopback(true); err != nil {
		r.logf("mdns: multicast loopback: %v", err)
	}
	if err := r.pc.SetMulticastTTL(255); err != nil {
		r.logf("mdns: multicast TTL: %v", err)
	}
	r.send = r.sendSocket
	r.joined[links[0].Iface.Index] = true
	r.setLinks(links)

	r.wg.Add(1)
	go r.readLoop()

	if err := r.probe(); err != nil {
		r.shutdown(false)
		return nil, err
	}
	r.announce(r.currentLinks())
	r.wg.Add(1)
	go r.refreshLoop()
	return r, nil
}

func newResponder(logf func(format string, args ...any)) *Responder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Responder{
		name:     dnsmessage.MustNewName(Name + "."),
		logf:     logf,
		joined:   map[int]bool{},
		lastSent: map[int]time.Time{},
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
			if msg, err := r.answer(0, nil, l.IP, 0, true); err == nil {
				r.send(l, msg, nil)
			}
		}
	}
	close(r.done)
	r.conn.Close()
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
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range links {
		if r.joined[l.Iface.Index] || r.pc == nil {
			continue
		}
		if err := r.pc.JoinGroup(&l.Iface, groupAddr); err != nil {
			r.logf("mdns: listening on %s: %v", l.Iface.Name, err)
			continue
		}
		r.joined[l.Iface.Index] = true
	}
	r.links = links
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
		if msg, err := r.answer(0, nil, l.IP, hostTTL, true); err == nil {
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
			if msg, err := r.answer(0, nil, l.IP, hostTTL, true); err == nil {
				r.send(l, msg, nil)
			}
		}
	}()
}

// refreshLoop follows network changes: a laptop that moves to another
// Wi-Fi or gets a new address from the router announces the new one.
func (r *Responder) refreshLoop() {
	defer r.wg.Done()
	t := time.NewTicker(refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
		}
		now := multicastLinks(lanaddr.All())
		changed := changedLinks(r.currentLinks(), now)
		r.setLinks(now)
		if len(changed) > 0 {
			r.announce(changed)
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

func (r *Responder) readLoop() {
	defer r.wg.Done()
	buf := make([]byte, 9000)
	for {
		n, src, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-r.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Windows reports an ICMP "port unreachable" for an earlier
			// unicast answer as a read error; keep reading.
			time.Sleep(10 * time.Millisecond)
			continue
		}
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
	var want []dnsmessage.Question
	unicast := false
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
		if q.Type != dnsmessage.TypeA && q.Type != dnsmessage.TypeALL {
			continue
		}
		want = append(want, q)
		if q.Class&classTopBit != 0 {
			unicast = true
		}
	}
	if len(want) == 0 {
		return
	}
	link, ok := r.linkFor(src.IP)
	if !ok {
		return
	}
	if src.Port != mdnsPort {
		// A one-shot resolver: answer it directly, with its query id and
		// question, a short TTL and no cache-flush bit (section 6.7).
		if msg, err := r.answer(h.ID, want, link.IP, legacyTTL, false); err == nil {
			r.send(link, msg, src)
		}
		return
	}
	if r.alreadyKnown(&p, link.IP) {
		return // the asker listed our answer as known (section 7.1)
	}
	msg, err := r.answer(0, nil, link.IP, hostTTL, true)
	if err != nil {
		return
	}
	if unicast {
		r.send(link, msg, src)
	}
	if r.multicastDue(link) {
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
// one network.
func (r *Responder) multicastDue(link lanaddr.Addr) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if last, ok := r.lastSent[link.Iface.Index]; ok && now.Sub(last) < minMulticastGap {
		return false
	}
	r.lastSent[link.Iface.Index] = now
	return true
}

// answer builds a response holding ip as the name's address. A non-nil
// questions list is repeated (one-shot answers need it).
func (r *Responder) answer(id uint16, questions []dnsmessage.Question, ip net.IP, ttl uint32, cacheFlush bool) ([]byte, error) {
	b := dnsmessage.NewBuilder(make([]byte, 0, 128), dnsmessage.Header{ID: id, Response: true, Authoritative: true})
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
	var a [4]byte
	copy(a[:], ip.To4())
	if err := b.AResource(dnsmessage.ResourceHeader{Name: r.name, Class: class, TTL: ttl}, dnsmessage.AResource{A: a}); err != nil {
		return nil, err
	}
	return b.Finish()
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

// sendSocket writes msg to dst, or to the multicast group on link's
// network when dst is nil.
func (r *Responder) sendSocket(link lanaddr.Addr, msg []byte, dst *net.UDPAddr) {
	if dst != nil {
		if _, err := r.conn.WriteToUDP(msg, dst); err != nil {
			r.logf("mdns: answering %s: %v", dst, err)
		}
		return
	}
	r.wmu.Lock()
	defer r.wmu.Unlock()
	if err := r.pc.SetMulticastInterface(&link.Iface); err != nil {
		r.logf("mdns: choosing %s: %v", link.Iface.Name, err)
		return
	}
	if _, err := r.conn.WriteToUDP(msg, groupAddr); err != nil {
		r.logf("mdns: announcing on %s: %v", link.Iface.Name, err)
	}
}
