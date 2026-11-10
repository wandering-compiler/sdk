package eventbus

import (
	"fmt"
	"log"
	"sync"
)

// Domain stamping. Channel names are unique per domain, not per broker, and a
// project runs one broker for all its domains, so two domains that declare
// the same channel name share one stream (NATS `<channel>`, Redis key
// `<channel>`). Renaming the streams would strand every existing deployment's
// backlog and consumers, so the stream stays shared and each message says
// whose it is instead: a bus configured with a Domain stamps it on every emit
// (NATS header [DomainHeader], Redis entry field `domain`), and its
// subscribers settle a message stamped with another domain without handing it
// to their handler. A message without a stamp — from a producer without a
// Domain, which is every producer built before the option existed — is
// delivered as before, so producers and subscribers can adopt the option in
// any order.

// DomainHeader is the NATS message header carrying the emitting domain.
const DomainHeader = "W17-Domain"

// redisFieldDomain is the Redis stream entry field carrying it.
const redisFieldDomain = "domain"

// validateDomain checks a bus's Domain option: empty (no stamping, no
// filtering) or letters, digits, '_' and '-'.
func validateDomain(field, domain string) error {
	for _, r := range domain {
		if !isNameRune(r) {
			return fmt.Errorf("%s: Domain %q contains %q (allowed: letters, digits, '_' and '-')", field, domain, r)
		}
	}
	return nil
}

// foreignDomain reports whether a delivery stamped with `stamped` belongs to
// another domain than `own`. A bus without a Domain, or a message without a
// stamp, is never foreign.
func foreignDomain(own, stamped string) bool {
	return own != "" && stamped != "" && stamped != own
}

// domainCollisions remembers the (channel, foreign domain) pairs already
// logged, so a shared channel is reported once per process, not per message.
var domainCollisions sync.Map

// noteForeignDomain logs, once per (own domain, channel, foreign domain), that
// a channel carries another domain's messages: settling them is correct, but
// the operator should know the two domains share a stream (its retention is
// rewritten by whichever process ensures it last).
func noteForeignDomain(own, channel, foreign string) {
	key := own + "\x00" + channel + "\x00" + foreign
	if _, seen := domainCollisions.LoadOrStore(key, struct{}{}); seen {
		return
	}
	log.Printf("eventbus: channel %q carries messages of domain %q; domain %q settles them unread — the two domains share the stream %q", channel, foreign, own, channel)
}
