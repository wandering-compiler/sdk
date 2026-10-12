package eventbus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"google.golang.org/protobuf/encoding/protowire"

	"github.com/wandering-compiler/sdk/go/core/observx"
)

// EnvelopeMetadataField is the field number of an event envelope's
// `map<string, string> metadata`. Every generated envelope declares it there,
// in the framework-owned range above the payload oneof, so the bus can read
// it without knowing the envelope's type.
const EnvelopeMetadataField protowire.Number = 50001

// ErrTransactionKeyInEnvelope is returned for a delivery whose envelope
// metadata carries a transaction key (`w17-tx-id` or `w17-conn-id`).
//
// An event never carries a transaction. Its emit is deferred until commit, so
// the transaction the key names is over by the time anything consumes it, or
// it belongs to a request that is still running somewhere else. A subscriber
// that replayed the key into its outgoing calls would make storage adopt that
// transaction. This SDK never publishes the keys (Publish strips them), so an
// envelope that has them came from a publisher outside it, or was persisted
// before the strip existed. The delivery fails rather than drop the key
// silently, so whoever sends it learns that it does not work.
var ErrTransactionKeyInEnvelope = errors.New("event envelope carries a transaction key; events never carry a transaction")

// transactionKeys are the metadata keys an envelope must not carry. They are
// part of reservedMetadataKeys too; this set is the subset that refuses the
// delivery instead of being dropped from it.
var transactionKeys = map[string]bool{
	"w17-tx-id":   true,
	"w17-conn-id": true,
}

// envelopeTransactionKey returns the first transaction key found in the
// envelope's metadata map, or "" when there is none. It reads only the
// top-level metadata field. Bytes that do not parse yield "": the dispatch's
// own unmarshal rejects them with a better error.
func envelopeTransactionKey(envelope []byte) string {
	b := envelope
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return ""
		}
		b = b[n:]
		if num == EnvelopeMetadataField && typ == protowire.BytesType {
			entry, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return ""
			}
			if k := mapEntryKey(entry); transactionKeys[strings.ToLower(k)] {
				return k
			}
			b = b[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, b)
		if m < 0 {
			return ""
		}
		b = b[m:]
	}
	return ""
}

// mapEntryKey returns the key (field 1) of one encoded map entry.
func mapEntryKey(entry []byte) string {
	var key string
	for len(entry) > 0 {
		num, typ, n := protowire.ConsumeTag(entry)
		if n < 0 {
			return key
		}
		entry = entry[n:]
		if num == 1 && typ == protowire.BytesType {
			v, m := protowire.ConsumeBytes(entry)
			if m < 0 {
				return key
			}
			key = string(v) // the last occurrence wins, as in proto.Unmarshal
			entry = entry[m:]
			continue
		}
		m := protowire.ConsumeFieldValue(num, typ, entry)
		if m < 0 {
			return key
		}
		entry = entry[m:]
	}
	return key
}

// refuseTransactionKeys wraps a HandlerFunc so a delivery whose envelope
// carries a transaction key never reaches it. The delivery fails with
// ErrTransactionKeyInEnvelope through the transport's normal failure path
// (retry, then the dead-letter queue), and each refusal is reported to the
// operator. Every transport's Subscribe applies it, through deliveryHandler.
func refuseTransactionKeys(h HandlerFunc) HandlerFunc {
	return func(ctx context.Context, topic string, envelope []byte) error {
		if k := envelopeTransactionKey(envelope); k != "" {
			err := fmt.Errorf("%w (%q on topic %q)", ErrTransactionKeyInEnvelope, k, topic)
			observx.ReportError(ctx, err)
			return err
		}
		return h(ctx, topic, envelope)
	}
}

// deliveryHandler is what every transport feeds deliveries to: the
// transaction-key refusal inside the panic recovery.
func deliveryHandler(h HandlerFunc) HandlerFunc {
	return recoverHandler(refuseTransactionKeys(h))
}
