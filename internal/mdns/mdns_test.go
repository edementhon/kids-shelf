package mdns

import (
	"net"
	"testing"

	"github.com/edementhon/kids-shelf/internal/lanaddr"
	"golang.org/x/net/dns/dnsmessage"
)

type sent struct {
	link lanaddr.Addr
	msg  []byte
	dst  *net.UDPAddr // nil: multicast
}

// testResponder answers for two networks: 198.51.100.10/24 (the best one)
// and 192.0.2.10/24, recording what it sends instead of using a socket.
func testResponder(t *testing.T) (*Responder, *[]sent) {
	t.Helper()
	r := newResponder(nil)
	link := func(ip string, index int) lanaddr.Addr {
		_, n, err := net.ParseCIDR(ip + "/24")
		if err != nil {
			t.Fatal(err)
		}
		return lanaddr.Addr{IP: net.ParseIP(ip).To4(), Net: n, Iface: net.Interface{Index: index, Name: "test", Flags: net.FlagUp | net.FlagMulticast}}
	}
	r.links = []lanaddr.Addr{link("198.51.100.10", 1), link("192.0.2.10", 2)}
	var out []sent
	r.send = func(l lanaddr.Addr, msg []byte, dst *net.UDPAddr) {
		out = append(out, sent{link: l, msg: append([]byte(nil), msg...), dst: dst})
	}
	return r, &out
}

// query builds a question packet; known, when set, is listed as a known
// answer with the given TTL.
func query(t *testing.T, id uint16, name string, typ dnsmessage.Type, class dnsmessage.Class, known net.IP, knownTTL uint32) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id})
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	n := dnsmessage.MustNewName(name)
	if err := b.Question(dnsmessage.Question{Name: n, Type: typ, Class: class}); err != nil {
		t.Fatal(err)
	}
	if known != nil {
		if err := b.StartAnswers(); err != nil {
			t.Fatal(err)
		}
		var a [4]byte
		copy(a[:], known.To4())
		if err := b.AResource(dnsmessage.ResourceHeader{Name: n, Class: dnsmessage.ClassINET, TTL: knownTTL}, dnsmessage.AResource{A: a}); err != nil {
			t.Fatal(err)
		}
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// response builds another device's answer for name.
func response(t *testing.T, name string, ip net.IP) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	var a [4]byte
	copy(a[:], ip.To4())
	if err := b.AResource(dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET | classTopBit, TTL: hostTTL}, dnsmessage.AResource{A: a}); err != nil {
		t.Fatal(err)
	}
	msg, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

type parsed struct {
	h         dnsmessage.Header
	questions []dnsmessage.Question
	answers   []dnsmessage.Resource
}

func parse(t *testing.T, msg []byte) parsed {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		t.Fatal(err)
	}
	qs, err := p.AllQuestions()
	if err != nil {
		t.Fatal(err)
	}
	as, err := p.AllAnswers()
	if err != nil {
		t.Fatal(err)
	}
	return parsed{h: h, questions: qs, answers: as}
}

// answerIP checks that m holds exactly one A answer for the name and
// returns its address, class and TTL.
func answerIP(t *testing.T, m parsed) (net.IP, dnsmessage.Class, uint32) {
	t.Helper()
	if !m.h.Response || !m.h.Authoritative {
		t.Fatalf("header %+v, want an authoritative response", m.h)
	}
	if len(m.answers) != 1 {
		t.Fatalf("answers = %d, want 1", len(m.answers))
	}
	a := m.answers[0]
	body, ok := a.Body.(*dnsmessage.AResource)
	if !ok || a.Header.Name.String() != Name+"." {
		t.Fatalf("answer %v, want an A record for %s", a, Name)
	}
	return net.IP(body.A[:]), a.Header.Class, a.Header.TTL
}

// TestMulticastQuestionGetsTheAskersNetwork: an iPad on 192.0.2.0/24 asks
// the normal way (from port 5353); the answer is multicast once, carries the
// address on the iPad's network (not the computer's best one), tells caches
// to replace older answers, and lasts 120 s.
func TestMulticastQuestionGetsTheAskersNetwork(t *testing.T) {
	r, out := testResponder(t)
	r.handle(query(t, 0, Name+".", dnsmessage.TypeA, dnsmessage.ClassINET, nil, 0), &net.UDPAddr{IP: net.ParseIP("192.0.2.50"), Port: mdnsPort})
	if len(*out) != 1 || (*out)[0].dst != nil {
		t.Fatalf("sent %+v, want one multicast answer", *out)
	}
	if got := (*out)[0].link.IP.String(); got != "192.0.2.10" {
		t.Errorf("multicast on the network of %s, want 192.0.2.10's", got)
	}
	m := parse(t, (*out)[0].msg)
	ip, class, ttl := answerIP(t, m)
	if ip.String() != "192.0.2.10" || class != dnsmessage.ClassINET|classTopBit || ttl != hostTTL {
		t.Errorf("answer %s class %#x ttl %d, want 192.0.2.10, IN with cache flush, %d", ip, uint16(class), ttl, hostTTL)
	}
	if m.h.ID != 0 || len(m.questions) != 0 {
		t.Errorf("multicast answer has id %d and %d questions, want 0 and 0", m.h.ID, len(m.questions))
	}

	// A second question within a second is not multicast again.
	r.handle(query(t, 0, Name+".", dnsmessage.TypeA, dnsmessage.ClassINET, nil, 0), &net.UDPAddr{IP: net.ParseIP("192.0.2.51"), Port: mdnsPort})
	if len(*out) != 1 {
		t.Errorf("a second multicast answer within a second: %d sends", len(*out))
	}
}

// TestOneShotQuestionGetsADirectAnswer: a resolver asking once from a random
// port (Android's kind) only hears a direct answer to that port, with its
// query id and question repeated, a short TTL and no cache-flush bit
// (RFC 6762 section 6.7). Without it, such tablets never get the address.
func TestOneShotQuestionGetsADirectAnswer(t *testing.T) {
	r, out := testResponder(t)
	src := &net.UDPAddr{IP: net.ParseIP("192.0.2.77"), Port: 40123}
	r.handle(query(t, 0x1234, "Kids-Shelf.Local.", dnsmessage.TypeA, dnsmessage.ClassINET, nil, 0), src)
	if len(*out) != 1 || (*out)[0].dst != src {
		t.Fatalf("sent %+v, want one direct answer to %s", *out, src)
	}
	m := parse(t, (*out)[0].msg)
	ip, class, ttl := answerIP(t, m)
	if ip.String() != "192.0.2.10" || class != dnsmessage.ClassINET || ttl > legacyTTL {
		t.Errorf("answer %s class %#x ttl %d, want 192.0.2.10, plain IN, at most %d", ip, uint16(class), ttl, legacyTTL)
	}
	if m.h.ID != 0x1234 || len(m.questions) != 1 || m.questions[0].Type != dnsmessage.TypeA {
		t.Errorf("id %#x questions %v, want the query's id and question", m.h.ID, m.questions)
	}
}

// TestUnicastBitGetsBoth: a question with the unicast-reply bit (what a
// tablet sends right after joining the Wi-Fi) is answered to the asker and
// on the network.
func TestUnicastBitGetsBoth(t *testing.T) {
	r, out := testResponder(t)
	src := &net.UDPAddr{IP: net.ParseIP("198.51.100.30"), Port: mdnsPort}
	r.handle(query(t, 0, Name+".", dnsmessage.TypeALL, dnsmessage.ClassINET|classTopBit, nil, 0), src)
	if len(*out) != 2 || (*out)[0].dst != src || (*out)[1].dst != nil {
		t.Fatalf("sent %+v, want a direct answer, then a multicast one", *out)
	}
	if ip, _, _ := answerIP(t, parse(t, (*out)[0].msg)); ip.String() != "198.51.100.10" {
		t.Errorf("answer %s, want 198.51.100.10", ip)
	}
}

// TestNotAnswered: other names, address types the shelf does not serve
// (AAAA), a question that already lists our answer, and every question
// during the start-up probe get no answer.
func TestNotAnswered(t *testing.T) {
	src := &net.UDPAddr{IP: net.ParseIP("192.0.2.50"), Port: mdnsPort}
	for _, tc := range []struct {
		name    string
		pkt     func(t *testing.T) []byte
		probing bool
	}{
		{"another name", func(t *testing.T) []byte {
			return query(t, 0, "printer.local.", dnsmessage.TypeA, dnsmessage.ClassINET, nil, 0)
		}, false},
		{"AAAA", func(t *testing.T) []byte {
			return query(t, 0, Name+".", dnsmessage.TypeAAAA, dnsmessage.ClassINET, nil, 0)
		}, false},
		{"known answer", func(t *testing.T) []byte {
			return query(t, 0, Name+".", dnsmessage.TypeA, dnsmessage.ClassINET, net.ParseIP("192.0.2.10"), hostTTL)
		}, false},
		{"while probing", func(t *testing.T) []byte {
			return query(t, 0, Name+".", dnsmessage.TypeA, dnsmessage.ClassINET, nil, 0)
		}, true},
		{"not DNS", func(t *testing.T) []byte { return []byte("hello") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, out := testResponder(t)
			r.probing.Store(tc.probing)
			r.handle(tc.pkt(t), src)
			if len(*out) != 0 {
				t.Errorf("sent %d packets, want none", len(*out))
			}
		})
	}

	// A known answer about to expire does not count: answer again.
	r, out := testResponder(t)
	r.handle(query(t, 0, Name+".", dnsmessage.TypeA, dnsmessage.ClassINET, net.ParseIP("192.0.2.10"), hostTTL/2-1), src)
	if len(*out) != 1 {
		t.Errorf("an expiring known answer got %d sends, want 1", len(*out))
	}
}

// TestProbeConflict: while probing, another device answering to the name
// with its own address is a conflict (the shelf then stays silent); an
// answer with this computer's address (its own OS, or itself) is not, and
// answers outside the probe are ignored.
func TestProbeConflict(t *testing.T) {
	src := &net.UDPAddr{IP: net.ParseIP("192.0.2.99"), Port: mdnsPort}

	r, _ := testResponder(t)
	r.probing.Store(true)
	r.handle(response(t, Name+".", net.ParseIP("192.0.2.10")), src)
	r.handle(response(t, "printer.local.", net.ParseIP("192.0.2.99")), src)
	if r.conflict.Load() {
		t.Error("conflict from our own address or another name")
	}
	r.handle(response(t, "KIDS-SHELF.local.", net.ParseIP("192.0.2.99")), src)
	if !r.conflict.Load() {
		t.Error("no conflict when another device answers to the name")
	}

	r2, _ := testResponder(t)
	r2.handle(response(t, Name+".", net.ParseIP("192.0.2.99")), src)
	if r2.conflict.Load() {
		t.Error("conflict recorded outside the probe")
	}
}

// TestProbeMessage: the probe asks for any record of the name with the
// unicast-reply bit and offers this computer's addresses as the proposed
// answer, so a device that owns the name answers and a simultaneous prober
// can compare.
func TestProbeMessage(t *testing.T) {
	r, _ := testResponder(t)
	msg, err := r.probeMessage(r.links)
	if err != nil {
		t.Fatal(err)
	}
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || h.Response {
		t.Fatalf("header %+v err %v, want a query", h, err)
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 || qs[0].Type != dnsmessage.TypeALL || qs[0].Class != dnsmessage.ClassINET|classTopBit || qs[0].Name.String() != Name+"." {
		t.Fatalf("questions %v err %v, want one ANY question for %s with the unicast bit", qs, err, Name)
	}
	if err := p.SkipAllAnswers(); err != nil {
		t.Fatal(err)
	}
	auth, err := p.AllAuthorities()
	if err != nil || len(auth) != 2 {
		t.Fatalf("authorities %v err %v, want both addresses", auth, err)
	}
}
