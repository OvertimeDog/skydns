// Copyright (c) 2014 The SkyDNS Authors. All rights reserved.
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package main

import (
	"log"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-etcd/etcd" // Implement this by directly using the API of etcd
	"github.com/miekg/dns"
)

type Server interface {
	Start() (*sync.WaitGroup, error)
	Stop()
	ServeDNS(dns.ResponseWriter, *dns.Msg)
	ServeDNSForward(dns.ResponseWriter, *dns.Msg)
	// GetRecords? GetSRVRecords
}

type server struct {
	nameservers  []string // nameservers to forward to
	domain       string
	domainLabels int
	client       *etcd.Client

	waiter *sync.WaitGroup

	dnsUDPServer *dns.Server
	dnsTCPServer *dns.Server
	dnsHandler   *dns.ServeMux

	DnsAddr string
	// DNSSEC key material
	PubKey  *dns.DNSKEY
	KeyTag  uint16
	PrivKey dns.PrivateKey

	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	RoundRobin   bool
	Ttl          uint32
	MinTtl       uint32
}

// Newserver returns a new server.
// TODO(miek): multiple ectdAddrs
func NewServer(domain, dnsAddr string, nameservers []string, etcdAddr string) *server {
	s := &server{
		domain:       dns.Fqdn(strings.ToLower(domain)),
		domainLabels: dns.CountLabel(dns.Fqdn(domain)),
		DnsAddr:      dnsAddr,
		client:       etcd.NewClient([]string{etcdAddr}),
		dnsHandler:   dns.NewServeMux(),
		waiter:       new(sync.WaitGroup),
		nameservers:  nameservers,
		Ttl:          3600,
		MinTtl:       60,
	}

	// DNS
	s.dnsHandler.Handle(".", s)
	return s
}

// Start starts a DNS server and blocks waiting to be killed.
func (s *server) Start() (*sync.WaitGroup, error) {
	log.Printf("initializing server. DNS Addr: %q, Forwarders: %q", s.DnsAddr, s.nameservers)

	s.dnsTCPServer = &dns.Server{
		Addr:         s.DnsAddr,
		Net:          "tcp",
		Handler:      s.dnsHandler,
		ReadTimeout:  s.ReadTimeout,
		WriteTimeout: s.WriteTimeout,
	}

	s.dnsUDPServer = &dns.Server{
		Addr:         s.DnsAddr,
		Net:          "udp",
		Handler:      s.dnsHandler,
		ReadTimeout:  s.ReadTimeout,
		WriteTimeout: s.WriteTimeout,
	}

	go s.listenAndServe()
	s.waiter.Add(1)
	go s.run()
	return s.waiter, nil
}

// Stop stops a server.
func (s *server) Stop() {
	log.Println("Stopping server")
	s.waiter.Done()
}

func (s *server) run() {
	var sig = make(chan os.Signal)
	signal.Notify(sig, os.Interrupt)

	for {
		select {
		case <-sig:
			s.Stop()
			return
		}
	}
}

// ServeDNS is the handler for DNS requests, responsible for parsing DNS request, possibly forwarding
// it to a real dns server and returning a response.
func (s *server) ServeDNS(w dns.ResponseWriter, req *dns.Msg) {
	//stats.RequestCount.Inc(1)

	q := req.Question[0]
	// Ensure we lowercase question so that proper matching against anchor domain takes place
	name := strings.ToLower(q.Name)

	log.Printf("Received DNS Request for %q from %q with type %d", q.Name, w.RemoteAddr(), q.Qtype)

	// If the query does not fall in our s.domain, forward it
	if !strings.HasSuffix(name, s.domain) {
		s.ServeDNSForward(w, req)
		return
	}
	m := new(dns.Msg)
	m.SetReply(req)
	m.Authoritative = true
	m.RecursionAvailable = true
	m.Answer = make([]dns.RR, 0, 10)
	defer func() {
		// Check if we need to do DNSSEC and sign the reply
		if s.PubKey != nil {
			if opt := req.IsEdns0(); opt != nil && opt.Do() {
				s.nsec(m)
				s.sign(m, opt.UDPSize())
			}
		}
		w.WriteMsg(m)
	}()

	if name == s.domain {
		switch q.Qtype {
		case dns.TypeDNSKEY:
			if s.PubKey != nil {
				m.Answer = append(m.Answer, s.PubKey)
				return
			}
		case dns.TypeSOA:
			m.Answer = []dns.RR{s.SOA()}
			return
		}
	}
	if q.Qtype == dns.TypeA || q.Qtype == dns.TypeAAAA {
		records, err := s.AddressRecords(q)
		if err != nil {
			m.SetRcode(req, dns.RcodeNameError)
			m.Ns = []dns.RR{s.SOA()}
			return
		}
		m.Answer = append(m.Answer, records...)
	}
	records, extra, err := s.SRVRecords(q)
	if err != nil && len(m.Answer) == 0 {
		// We are authoritative for this name, but it does not exist: NXDOMAIN
		m.SetRcode(req, dns.RcodeNameError)
		m.Ns = []dns.RR{s.SOA()}
		return
	}
	if q.Qtype == dns.TypeANY || q.Qtype == dns.TypeSRV {
		m.Answer = append(m.Answer, records...)
		m.Extra = append(m.Extra, extra...)
	}

	if len(m.Answer) == 0 { // Send back a NODATA response
		m.Ns = []dns.RR{s.SOA()}
	}
}

// ServeDNSForward forwards a request to a nameservers and returns the response.
func (s *server) ServeDNSForward(w dns.ResponseWriter, req *dns.Msg) {
	if len(s.nameservers) == 0 {
		log.Printf("Error: Failure to Forward DNS Request, no servers configured %q", dns.ErrServ)
		m := new(dns.Msg)
		m.SetReply(req)
		m.SetRcode(req, dns.RcodeServerFailure)
		m.Authoritative = false     // no matter what set to false
		m.RecursionAvailable = true // and this is still true
		w.WriteMsg(m)
		return
	}
	network := "udp"
	if _, ok := w.RemoteAddr().(*net.TCPAddr); ok {
		network = "tcp"
	}
	// TODO(miek): use commandline timeout stuff?
	c := &dns.Client{Net: network, ReadTimeout: 5 * time.Second}

	// Use request Id for "random" nameserver selection
	nsid := int(req.Id) % len(s.nameservers)
	try := 0
Redo:
	r, _, err := c.Exchange(req, s.nameservers[nsid])
	if err == nil {
		log.Printf("Forwarded DNS Request %q to %q", req.Question[0].Name, s.nameservers[nsid])
		w.WriteMsg(r)
		return
	}
	// Seen an error, this can only mean, "server not reached", try again
	// but only if we have not exausted our nameservers
	if try < len(s.nameservers) {
		log.Printf("Error: Failure to Forward DNS Request %q to %q", err, s.nameservers[nsid])
		try++
		nsid = (nsid + 1) % len(s.nameservers)
		goto Redo
	}

	log.Printf("Error: Failure to Forward DNS Request %q", err)
	m := new(dns.Msg)
	m.SetReply(req)
	m.SetRcode(req, dns.RcodeServerFailure)
	w.WriteMsg(m)
}

func (s *server) AddressRecords(q dns.Question) (records []dns.RR, err error) {
	name := strings.ToLower(q.Name)
	if name == "master."+s.domain || name == s.domain {
		for _, m := range s.client.GetCluster() {
			u, e := url.Parse(m)
			if e != nil {
				continue
			}
			h, _, e := net.SplitHostPort(u.Host)
			if e != nil {
				continue
			}
			ip := net.ParseIP(h)
			switch {
			case ip.To4() != nil && q.Qtype == dns.TypeA:
				records = append(records, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: s.Ttl}, A: ip.To4()})
			case ip.To16() != nil:
				records = append(records, &dns.AAAA{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: s.Ttl}, AAAA: ip.To16()})
			default:
				// TODO(miek): really?
				panic("skydns: internal error")
			}
		}
		return
	}
	path := questionToPath(name)
	r, err := s.client.Get(path, false, true)
	if err != nil {
		return nil, err
	}
	if !r.Node.Dir { // single element
		// facter out, used twice already
		// TODO(miek): seems not work :(
		if strings.HasSuffix(r.Node.Key, "/A") && q.Qtype == dns.TypeA {
			// Error checking! + TTL is 0 etc.
			a := new(dns.A)
			a.Hdr = dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: uint32(r.Node.TTL)}
			a.A = net.ParseIP(r.Node.Value).To4()
			records = append(records, a)
		}
		if strings.HasSuffix(r.Node.Key, "/AAAA") && q.Qtype == dns.TypeAAAA {
			aaaa := new(dns.AAAA)
			aaaa.Hdr = dns.RR_Header{Name: q.Name, Rrtype: q.Qtype, Class: dns.ClassINET, Ttl: uint32(r.Node.TTL)}
			aaaa.AAAA = net.ParseIP(r.Node.Value).To16()
			records = append(records, aaaa)
		}
	} else {
		// size of this, may overflow dns packet, etc... etc...
		records = append(records, loopAddressNodes(&r.Node.Nodes, q.Name, q.Qtype)...)
	}
	if s.RoundRobin {
		switch l := len(records); l {
		case 1:
		case 2:
			if dns.Id()%2 == 0 {
				records[0], records[1] = records[1], records[0]
			}
		default:
			// Do a minimum of l swap, maximum of 4l swaps
			for j := 0; j < l*(int(dns.Id())%4+1); j++ {
				q := int(dns.Id()) % l
				p := int(dns.Id()) % l
				if q == p {
					p = (p + 1) % l
				}
				records[q], records[p] = records[p], records[q]
			}
		}
	}
	return records, nil
}

// loopNodes recursively loops through the nodes and returns all the
// values in a string slice.
func loopAddressNodes(n *etcd.Nodes, q string, t uint16) (r []dns.RR) {
	for _, n := range *n {
		if n.Dir {
			r = append(r, loopAddressNodes(&n.Nodes, q, t)...)
			continue
		}
		if strings.HasSuffix(n.Key, "/A") && t == dns.TypeA {
			// Error checking! + TTL is 0 etc.
			a := new(dns.A)
			a.Hdr = dns.RR_Header{Name: q, Rrtype: t, Class: dns.ClassINET, Ttl: uint32(n.TTL)}
			a.A = net.ParseIP(n.Value).To4()
			r = append(r, a)
			continue
		}
		if strings.HasSuffix(n.Key, "/AAAA") && t == dns.TypeAAAA {
			aaaa := new(dns.AAAA)
			aaaa.Hdr = dns.RR_Header{Name: q, Rrtype: t, Class: dns.ClassINET, Ttl: uint32(n.TTL)}
			aaaa.AAAA = net.ParseIP(n.Value).To16()
			r = append(r, aaaa)
			continue
		}
	}
	return
}

// SRVRecords return SRV records from etcd.
// If the Target is not an name but an IP address, an name is created .
func (s *server) SRVRecords(q dns.Question) (records []dns.RR, extra []dns.RR, err error) {
	name := strings.ToLower(q.Name)
	path := questionToPath(name)
	_, err = s.client.Get(path, false, true)
	return nil, nil, err
	/*
		weight = 0
		if len(services) > 0 {
			weight = uint16(math.Floor(float64(100 / len(services))))
		}

		for _, serv := range services {
			// TODO: Dynamically set weight
			// a Service may have an IP as its Host"name", in this case
			// substitute UUID + "." + s.domain+"." an add an A record
			// with the name and IP in the additional section.
			// TODO(miek): check if resolvers actually grok this
			ip := net.ParseIP(serv.Host)
			switch {
			case ip == nil:
				records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: serv.TTL},
					Priority: 10, Weight: weight, Port: serv.Port, Target: serv.Host + "."})
				continue
			case ip.To4() != nil:
				extra = append(extra, &dns.A{Hdr: dns.RR_Header{Name: serv.UUID + "." + s.domain + ".", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: serv.TTL}, A: ip.To4()})
				records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: serv.TTL},
					Priority: 10, Weight: weight, Port: serv.Port, Target: serv.UUID + "." + s.domain + "."})
			case ip.To16() != nil:
				extra = append(extra, &dns.AAAA{Hdr: dns.RR_Header{Name: serv.UUID + "." + s.domain + ".", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: serv.TTL}, AAAA: ip.To16()})
				records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: serv.TTL},
					Priority: 10, Weight: weight, Port: serv.Port, Target: serv.UUID + "." + s.domain + "."})
			default:
				panic("skydns: internal error")
			}
		}

		// Append matching entries in different region than requested with a higher priority
		labels := dns.SplitDomainName(key)

		pos := len(labels) - 4
		if len(labels) >= 4 && labels[pos] != "*" {
				region := labels[pos]
				labels[pos] = "*"

				// TODO: This is pretty much a copy of the above, and should be abstracted
				additionalServices := make([]msg.Service, len(services))
				additionalServices, err = s.registry.Get(strings.Join(labels, "."))

				if err != nil {
					return
				}

				weight = 0
				if len(additionalServices) <= len(services) {
					return
				}

				weight = uint16(math.Floor(float64(100 / (len(additionalServices) - len(services)))))
				for _, serv := range additionalServices {
					// Exclude entries we already have
					if strings.ToLower(serv.Region) == region {
						continue
					}
					// TODO: Dynamically set priority and weight
					// TODO(miek): same as above: abstract away
					ip := net.ParseIP(serv.Host)
					switch {
					case ip == nil:
						records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: serv.TTL},
							Priority: 20, Weight: weight, Port: serv.Port, Target: serv.Host + "."})
						continue
					case ip.To4() != nil:
						extra = append(extra, &dns.A{Hdr: dns.RR_Header{Name: serv.UUID + "." + s.domain + ".", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: serv.TTL}, A: ip.To4()})
						records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: serv.TTL},
							Priority: 20, Weight: weight, Port: serv.Port, Target: serv.UUID + "." + s.domain + "."})
					case ip.To16() != nil:
						extra = append(extra, &dns.AAAA{Hdr: dns.RR_Header{Name: serv.UUID + "." + s.domain + ".", Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: serv.TTL}, AAAA: ip.To16()})
						records = append(records, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: serv.TTL},
							Priority: 20, Weight: weight, Port: serv.Port, Target: serv.UUID + "." + s.domain + "."})
					default:
						panic("skydns: internal error")
					}
				}
		}
	*/
	return
}

// listenAndServe binds to DNS ports and starts accepting connections.
func (s *server) listenAndServe() {
	go func() {
		err := s.dnsTCPServer.ListenAndServe()
		if err != nil {
			log.Fatalf("Start %s listener on %s failed:%s", s.dnsTCPServer.Net, s.dnsTCPServer.Addr, err.Error())
		}
	}()

	go func() {
		err := s.dnsUDPServer.ListenAndServe()
		if err != nil {
			log.Fatalf("Start %s listener on %s failed:%s", s.dnsUDPServer.Net, s.dnsUDPServer.Addr, err.Error())
		}
	}()
}

// SOA returns a SOA record for this SkyDNS instance.
func (s *server) SOA() dns.RR {
	return &dns.SOA{Hdr: dns.RR_Header{Name: s.domain, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: s.Ttl},
		Ns:      "master." + s.domain,
		Mbox:    "hostmaster." + s.domain,
		Serial:  uint32(time.Now().Truncate(time.Hour).Unix()),
		Refresh: 28800,
		Retry:   7200,
		Expire:  604800,
		Minttl:  s.MinTtl,
	}
}

// questionToPath convert a domainname to a etcd path. If the question
// looks like service.staging.skydns.local., the resulting key
// will by /local/skydns/staging/service .
func questionToPath(s string) string {
	l := dns.SplitDomainName(s)
	for i, j := 0, len(l)-1; i < j; i, j = i+1, j-1 {
		l[i], l[j] = l[j], l[i]
	}
	// TODO(miek): escape slashes in s.
	return strings.Join(l, "/")
}
