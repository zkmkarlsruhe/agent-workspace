// SPDX-License-Identifier: MIT

package settings

import (
	"cmp"
	"context"
	"errors"
	"net"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
)

// Entry contains the directory attributes used in a person’s profile.
type Entry struct {
	Department, Role, Phone, Room string
}

// ErrUnknown: the directory has no entry for the person.
var ErrUnknown = errors.New("profil: not in the directory")

// LDAP uses a read-only service bind to map department, role, phone and room
// attributes into a profile. Bilingual department and role values keep the
// first part before "|".
type LDAP struct {
	URL      string // ldap://ldap.example.org:389 or ldaps://...
	BaseDN   string // ou=People,dc=example,dc=org
	BindDN   string // "" = anonymous bind
	Password string
	Timeout  time.Duration
}

var ldapAttrs = []string{"businessCategory", "title", "telephoneNumber", "roomNumber"}

// Lookup finds the person by e-mail (mail or mailAlias), else by uid = username.
func (l LDAP) Lookup(ctx context.Context, email, username string) (Entry, error) {
	to := cmp.Or(l.Timeout, 5*time.Second)
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < to {
		to = time.Until(dl)
	}
	c, err := ldap.DialURL(l.URL, ldap.DialWithDialer(&net.Dialer{Timeout: to}))
	if err != nil {
		return Entry{}, err
	}
	defer c.Close()
	c.SetTimeout(to)
	if l.BindDN == "" {
		err = c.UnauthenticatedBind("")
	} else {
		err = c.Bind(l.BindDN, l.Password)
	}
	if err != nil {
		return Entry{}, err
	}
	var filters []string
	if email != "" {
		e := ldap.EscapeFilter(email)
		filters = append(filters, "(|(mail="+e+")(mailAlias="+e+"))")
	}
	if username != "" {
		filters = append(filters, "(uid="+ldap.EscapeFilter(username)+")")
	}
	for _, f := range filters {
		res, err := c.Search(ldap.NewSearchRequest(l.BaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
			2, int(to/time.Second), false, f, ldapAttrs, nil))
		if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
			return Entry{}, err
		}
		// exactly one hit: two people with the same alias are not this person for sure
		if res == nil || len(res.Entries) != 1 {
			continue
		}
		e := res.Entries[0]
		return Entry{
			Department: germanPart(e.GetAttributeValue("businessCategory")),
			Role:       germanPart(e.GetAttributeValue("title")),
			Phone:      e.GetAttributeValue("telephoneNumber"),
			Room:       e.GetAttributeValue("roomNumber"),
		}, nil
	}
	return Entry{}, ErrUnknown
}

func germanPart(v string) string {
	de, _, _ := strings.Cut(v, "|")
	return strings.TrimSpace(de)
}
