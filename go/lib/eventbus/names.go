package eventbus

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// Name rules shared by every transport. A channel or topic that one broker
// accepts and another rejects would make the same declaration work on one
// transport and fail at the first emit or subscribe on the other, so every
// adapter — the in-memory one included — applies the strictest common rule,
// before the name reaches a broker.
var (
	// ErrInvalidChannel is returned (wrapped) for a channel name outside
	// [ValidateChannel]'s rule.
	ErrInvalidChannel = errors.New("eventbus: invalid channel name")

	// ErrInvalidTopic is returned (wrapped) for a topic outside
	// [ValidateTopic]'s rule.
	ErrInvalidTopic = errors.New("eventbus: invalid topic")
)

// maxChannelLen keeps every name derived from a channel — the JetStream
// stream and its `<channel>_dlq` dead-letter stream — under JetStream's
// 255-byte name limit.
const maxChannelLen = 200

// ValidateChannel reports whether name is usable as a channel on every
// transport: 1 to 200 characters from `A–Z a–z 0–9 _ -`. A channel names a
// JetStream stream, which forbids `.`, `*`, `>`, path separators and
// whitespace, and a Redis stream key, which allows anything; the rule is the
// intersection.
func ValidateChannel(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidChannel)
	}
	if len(name) > maxChannelLen {
		return fmt.Errorf("%w: %q is longer than %d characters", ErrInvalidChannel, name, maxChannelLen)
	}
	for _, r := range name {
		if !isNameRune(r) {
			return fmt.Errorf("%w: %q contains %q (allowed: letters, digits, '_' and '-')", ErrInvalidChannel, name, r)
		}
	}
	return nil
}

// ValidateTopic reports whether topic is usable as an emitted topic on every
// transport: one or more `.`-separated non-empty tokens, none containing
// whitespace, control characters or the subject wildcards `*` and `>`.
//
// An empty topic is refused: on NATS it would publish to the bare channel
// subject, outside the stream's `<channel>.>` capture, and on the other
// transports it would reach only catch-all subscriptions, so the same emit
// would behave differently per transport.
func ValidateTopic(topic string) error {
	if topic == "" {
		return fmt.Errorf("%w: empty", ErrInvalidTopic)
	}
	for _, tok := range strings.Split(topic, ".") {
		if tok == "" {
			return fmt.Errorf("%w: %q has an empty segment", ErrInvalidTopic, topic)
		}
		for _, r := range tok {
			if r == '*' || r == '>' || unicode.IsSpace(r) || unicode.IsControl(r) {
				return fmt.Errorf("%w: %q contains %q", ErrInvalidTopic, topic, r)
			}
		}
	}
	return nil
}

func isNameRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-'
}

// validateEmit is the emit-side check every Dispatch runs first.
func validateEmit(channel, topic string) error {
	if err := ValidateChannel(channel); err != nil {
		return err
	}
	return ValidateTopic(topic)
}

// validateChannelKeys checks the channel names a per-channel option map is
// keyed by, so a typo in configuration fails at construction instead of
// silently never matching.
func validateChannelKeys[V any](field string, m map[string]V) error {
	for ch := range m {
		if err := ValidateChannel(ch); err != nil {
			return fmt.Errorf("%s: %w", field, err)
		}
	}
	return nil
}
