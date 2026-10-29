// SPDX-License-Identifier: MIT

package settings

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	ber "github.com/go-asn1-ber/asn1-ber"
	"github.com/go-ldap/ldap/v3"
)

// fakeLDAP answers simple binds and searches from a list of entries; a
// search matches an entry if the decompiled filter names one of its keys.
type fakeLDAP struct {
	bindDN, password string
	entries          []fakeEntry
	mu               sync.Mutex
	filters          []string
	binds            []string
}

type fakeEntry struct {
	keys  []string // filter fragments that select it, e.g. "(mail=anna@example.org)"
	attrs map[string]string
}

func (f *fakeLDAP) start(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return "ldap://" + ln.Addr().String()
}

func reply(id int64, op ber.Tag, children ...*ber.Packet) []byte {
	p := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
	p.AppendChild(ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagInteger, id, ""))
	r := ber.Encode(ber.ClassApplication, ber.TypeConstructed, op, nil, "")
	for _, c := range children {
		r.AppendChild(c)
	}
	p.AppendChild(r)
	return p.Bytes()
}

func result(code int64) []*ber.Packet {
	return []*ber.Packet{
		ber.NewInteger(ber.ClassUniversal, ber.TypePrimitive, ber.TagEnumerated, code, ""),
		ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""),
		ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "", ""),
	}
}

func (f *fakeLDAP) serve(c net.Conn) {
	defer c.Close()
	for {
		p, err := ber.ReadPacket(c)
		if err != nil || len(p.Children) < 2 {
			return
		}
		id := p.Children[0].Value.(int64)
		op := p.Children[1]
		switch op.Tag {
		case ldap.ApplicationBindRequest:
			dn, pw := op.Children[1].Data.String(), op.Children[2].Data.String()
			f.mu.Lock()
			f.binds = append(f.binds, dn)
			f.mu.Unlock()
			code := int64(ldap.LDAPResultSuccess)
			if dn != f.bindDN || pw != f.password {
				code = ldap.LDAPResultInvalidCredentials
			}
			c.Write(reply(id, ldap.ApplicationBindResponse, result(code)...))
		case ldap.ApplicationSearchRequest:
			flt, _ := ldap.DecompileFilter(op.Children[6])
			f.mu.Lock()
			f.filters = append(f.filters, flt)
			f.mu.Unlock()
			for _, e := range f.entries {
				if !containsAny(flt, e.keys) {
					continue
				}
				attrs := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
				for k, v := range e.attrs {
					a := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSequence, nil, "")
					a.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, k, ""))
					vals := ber.Encode(ber.ClassUniversal, ber.TypeConstructed, ber.TagSet, nil, "")
					vals.AppendChild(ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, v, ""))
					a.AppendChild(vals)
					attrs.AppendChild(a)
				}
				c.Write(reply(id, ldap.ApplicationSearchResultEntry,
					ber.NewString(ber.ClassUniversal, ber.TypePrimitive, ber.TagOctetString, "uid=x,ou=People,dc=example,dc=org", ""), attrs))
			}
			c.Write(reply(id, ldap.ApplicationSearchResultDone, result(ldap.LDAPResultSuccess)...))
		case ldap.ApplicationUnbindRequest:
			return
		}
	}
}

func containsAny(s string, subs []string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

func TestLDAPLookup(t *testing.T) {
	f := &fakeLDAP{bindDN: "uid=binduser,ou=People,dc=example,dc=org", password: "geheim", entries: []fakeEntry{
		{keys: []string{"(mailAlias=anna.beispiel@example.org)"}, attrs: map[string]string{
			"businessCategory": "Technik | Museum Technology", "title": "Leitung | Head",
			"telephoneNumber": "+49 30 1234-1234", "roomNumber": "B 2.13"}},
		{keys: []string{"(uid=bert)"}, attrs: map[string]string{"businessCategory": "Presse"}},
		// two entries share this alias: not a unique person
		{keys: []string{"(mail=team@example.org)"}, attrs: map[string]string{"title": "A"}},
		{keys: []string{"(mail=team@example.org)"}, attrs: map[string]string{"title": "B"}},
	}}
	l := LDAP{URL: f.start(t), BaseDN: "ou=People,dc=example,dc=org", BindDN: f.bindDN, Password: f.password}
	ctx := context.Background()

	e, err := l.Lookup(ctx, "anna.beispiel@example.org", "anna")
	if err != nil || e != (Entry{Department: "Technik", Role: "Leitung", Phone: "+49 30 1234-1234", Room: "B 2.13"}) {
		t.Fatalf("by alias: %+v %v", e, err)
	}
	if e, err := l.Lookup(ctx, "bert@example.org", "bert"); err != nil || e.Department != "Presse" {
		t.Errorf("by uid: %+v %v", e, err)
	}
	if _, err := l.Lookup(ctx, "team@example.org", ""); !errors.Is(err, ErrUnknown) {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := l.Lookup(ctx, `x*)(uid=*`, ""); !errors.Is(err, ErrUnknown) {
		t.Errorf("injection: %v", err)
	}
	f.mu.Lock()
	last := f.filters[len(f.filters)-1]
	f.mu.Unlock()
	if !strings.Contains(last, `\2a`) {
		t.Errorf("filter not escaped: %s", last)
	}
	l.Password = "falsch"
	if _, err := l.Lookup(ctx, "anna.beispiel@example.org", ""); err == nil || errors.Is(err, ErrUnknown) {
		t.Errorf("bad bind: %v", err)
	}
}
