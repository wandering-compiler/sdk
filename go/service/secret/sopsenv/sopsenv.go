// Package sopsenv reads and writes sops-encrypted dotenv files with age keys —
// byte-compatible with the sops CLI (`sops encrypt --input-type dotenv`), so a
// file sealed here decrypts with `sops decrypt` and the reverse.
//
// Why not the sops library: its packages import every key source sops knows
// (AWS KMS, GCP KMS, Azure Key Vault, HashiCorp Vault, PGP) — about 800
// packages — and w17ctl is a minimal public client. The format is small and
// documented by sops's own code (stores/dotenv, aes, age keysource, the tree
// MAC); compatibility is proven by round trips through the real sops image in
// both directions (scripts/check-sops-compat.sh), not assumed.
//
// The format, as sops writes it for a dotenv file:
//
//   - every value: `ENC[AES256_GCM,data:…,iv:…,tag:…,type:str]`, AES-256-GCM
//     under a random 32-byte data key, a 32-byte IV, the key name plus ":" as
//     additional data. An empty value stays empty; a key ending in
//     `_unencrypted` stays in clear.
//   - every comment: `#ENC[…,type:comment]`, additional data ":".
//   - the MAC: SHA-512 over the plaintext values in order (comments excluded),
//     upper-case hex, itself encrypted under the data key with the
//     `sops_lastmodified` timestamp as additional data.
//   - the data key: an ASCII-armoured age file per recipient, its newlines
//     written as the two characters `\n`.
//   - metadata as `sops_*` lines after the values.
package sopsenv

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
)

// Version is the sops version this writes as `sops_version`: the one the
// format was taken from and the compatibility check runs.
const Version = "3.13.3"

const (
	unencryptedSuffix = "_unencrypted"
	metaPrefix        = "sops_"
	nonceSize         = 32
)

// Line is one line of a dotenv file: a comment (Value is the text after
// `#`) or a KEY=VALUE pair. Empty lines carry nothing and are dropped, as
// sops drops them.
type Line struct {
	Comment bool
	Key     string
	Value   string
}

// Parse reads a plaintext dotenv file the way sops does: `#` starts a
// comment, everything else is KEY=VALUE split at the first `=`, the two
// characters `\n` in a value are a newline.
func Parse(b []byte) ([]Line, error) {
	var out []Line
	for i, raw := range bytes.Split(b, []byte("\n")) {
		line := strings.TrimSuffix(string(raw), "\r")
		if line == "" {
			continue
		}
		if line[0] == '#' {
			out = append(out, Line{Comment: true, Value: line[1:]})
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: %q is neither a comment nor KEY=VALUE", i+1, line)
		}
		out = append(out, Line{Key: key, Value: strings.ReplaceAll(value, `\n`, "\n")})
	}
	return out, nil
}

// Format writes lines back as a dotenv file, newlines in values as `\n`.
func Format(lines []Line) []byte {
	var b bytes.Buffer
	for _, l := range lines {
		if l.Comment {
			b.WriteString("#" + l.Value + "\n")
			continue
		}
		b.WriteString(l.Key + "=" + strings.ReplaceAll(l.Value, "\n", `\n`) + "\n")
	}
	return b.Bytes()
}

// Values returns the KEY=VALUE pairs of lines as a map (comments ignored).
func Values(lines []Line) map[string]string {
	out := map[string]string{}
	for _, l := range lines {
		if !l.Comment {
			out[l.Key] = l.Value
		}
	}
	return out
}

// Encrypt seals plaintext lines for the age recipients ("age1…").
func Encrypt(lines []Line, recipients []string, now time.Time) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("sopsenv: no age recipient to encrypt for")
	}
	for _, l := range lines {
		if !l.Comment && strings.HasPrefix(l.Key, metaPrefix) {
			return nil, fmt.Errorf("sopsenv: key %s uses the %s prefix sops reserves for its metadata", l.Key, metaPrefix)
		}
	}
	dataKey := make([]byte, 32)
	if _, err := rand.Read(dataKey); err != nil {
		return nil, err
	}
	out := make([]Line, 0, len(lines)+8)
	mac := sha512.New()
	for _, l := range lines {
		if l.Comment {
			v, err := encryptValue(l.Value, dataKey, ":", "comment")
			if err != nil {
				return nil, err
			}
			out = append(out, Line{Comment: true, Value: v})
			continue
		}
		mac.Write([]byte(l.Value))
		v := l.Value
		if !strings.HasSuffix(l.Key, unencryptedSuffix) {
			var err error
			if v, err = encryptValue(l.Value, dataKey, l.Key+":", "str"); err != nil {
				return nil, err
			}
		}
		out = append(out, Line{Key: l.Key, Value: v})
	}
	ages, err := ageEntries(dataKey, recipients)
	if err != nil {
		return nil, err
	}
	lastModified := now.UTC().Format(time.RFC3339)
	encMAC, err := encryptValue(fmt.Sprintf("%X", mac.Sum(nil)), dataKey, lastModified, "str")
	if err != nil {
		return nil, err
	}
	meta := ages
	meta = append(meta,
		Line{Key: "sops_lastmodified", Value: lastModified},
		Line{Key: "sops_mac", Value: encMAC},
		Line{Key: "sops_unencrypted_suffix", Value: unencryptedSuffix},
		Line{Key: "sops_version", Value: Version},
	)
	return Format(append(out, meta...)), nil
}

// Sealed is an encrypted file split into its body and its metadata, readable
// without any key: the key names, the recipients, the timestamps.
type Sealed struct {
	Body []Line            // values and comments as stored (encrypted)
	Meta map[string]string // the sops_* lines
}

// Read splits an encrypted file. It does not decrypt anything.
func Read(enc []byte) (*Sealed, error) {
	lines, err := Parse(enc)
	if err != nil {
		return nil, err
	}
	s := &Sealed{Meta: map[string]string{}}
	for _, l := range lines {
		if !l.Comment && strings.HasPrefix(l.Key, metaPrefix) {
			s.Meta[l.Key] = l.Value
			continue
		}
		s.Body = append(s.Body, l)
	}
	if s.Meta["sops_mac"] == "" || s.Meta["sops_lastmodified"] == "" {
		return nil, errors.New("not a sops file: no sops_mac / sops_lastmodified")
	}
	return s, nil
}

// Keys are the variable names the file carries, in order.
func (s *Sealed) Keys() []string {
	var out []string
	for _, l := range s.Body {
		if !l.Comment {
			out = append(out, l.Key)
		}
	}
	return out
}

// Recipients are the age recipients the data key is encrypted for.
func (s *Sealed) Recipients() []string {
	var out []string
	for _, i := range s.ageIndexes() {
		out = append(out, s.Meta[fmt.Sprintf("sops_age__list_%d__map_recipient", i)])
	}
	return out
}

func (s *Sealed) ageIndexes() []int {
	re := regexp.MustCompile(`^sops_age__list_(\d+)__map_enc$`)
	var idx []int
	for k := range s.Meta {
		if m := re.FindStringSubmatch(k); m != nil {
			n, _ := strconv.Atoi(m[1])
			idx = append(idx, n)
		}
	}
	sort.Ints(idx)
	return idx
}

// dataKey recovers the data key with any of the identities.
func (s *Sealed) dataKey(ids []age.Identity) ([]byte, error) {
	if len(ids) == 0 {
		return nil, errors.New("no age identity to decrypt with")
	}
	idx := s.ageIndexes()
	if len(idx) == 0 {
		return nil, errors.New("the file has no age recipient (sealed for another key source)")
	}
	var last error
	for _, i := range idx {
		armored := s.Meta[fmt.Sprintf("sops_age__list_%d__map_enc", i)]
		r, err := age.Decrypt(armor.NewReader(strings.NewReader(armored)), ids...)
		if err != nil {
			last = err
			continue
		}
		key, err := io.ReadAll(r)
		if err != nil {
			return nil, err
		}
		if len(key) != 32 {
			return nil, fmt.Errorf("data key is %d bytes, want 32", len(key))
		}
		return key, nil
	}
	return nil, fmt.Errorf("none of the identities opens the data key (recipients %s): %w", strings.Join(s.Recipients(), ", "), last)
}

// Decrypt opens an encrypted file and verifies its MAC.
func Decrypt(enc []byte, ids []age.Identity) ([]Line, error) {
	s, err := Read(enc)
	if err != nil {
		return nil, err
	}
	key, err := s.dataKey(ids)
	if err != nil {
		return nil, err
	}
	out := make([]Line, 0, len(s.Body))
	mac := sha512.New()
	for _, l := range s.Body {
		if l.Comment {
			v, err := decryptValue(l.Value, key, ":")
			if err != nil {
				v = l.Value // a comment sealed in clear, as sops tolerates
			}
			out = append(out, Line{Comment: true, Value: v})
			continue
		}
		v := l.Value
		if !strings.HasSuffix(l.Key, unencryptedSuffix) {
			if v, err = decryptValue(l.Value, key, l.Key+":"); err != nil {
				return nil, fmt.Errorf("%s: %w", l.Key, err)
			}
		}
		mac.Write([]byte(v))
		out = append(out, Line{Key: l.Key, Value: v})
	}
	want, err := decryptValue(s.Meta["sops_mac"], key, s.Meta["sops_lastmodified"])
	if err != nil {
		return nil, fmt.Errorf("sops_mac: %w", err)
	}
	if got := fmt.Sprintf("%X", mac.Sum(nil)); !strings.EqualFold(got, want) {
		return nil, errors.New("MAC mismatch: the file was changed after it was sealed")
	}
	return out, nil
}

// Rekey re-encrypts the data key for a new set of recipients, leaving every
// value, the MAC and the timestamp as they are — `sops updatekeys`.
func Rekey(enc []byte, ids []age.Identity, recipients []string) ([]byte, error) {
	if len(recipients) == 0 {
		return nil, errors.New("sopsenv: no age recipient to rekey for")
	}
	if _, err := Decrypt(enc, ids); err != nil { // MAC first: never re-seal a tampered file
		return nil, err
	}
	s, _ := Read(enc)
	key, err := s.dataKey(ids)
	if err != nil {
		return nil, err
	}
	ages, err := ageEntries(key, recipients)
	if err != nil {
		return nil, err
	}
	out := append([]Line(nil), s.Body...)
	out = append(out, ages...)
	var rest []string
	for k := range s.Meta {
		if !strings.HasPrefix(k, "sops_age__") {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	for _, k := range rest {
		out = append(out, Line{Key: k, Value: s.Meta[k]})
	}
	return Format(out), nil
}

func ageEntries(dataKey []byte, recipients []string) ([]Line, error) {
	var out []Line
	for i, r := range recipients {
		rec, err := age.ParseX25519Recipient(strings.TrimSpace(r))
		if err != nil {
			return nil, fmt.Errorf("sopsenv: recipient %q: %w", r, err)
		}
		var buf bytes.Buffer
		aw := armor.NewWriter(&buf)
		w, err := age.Encrypt(aw, rec)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(dataKey); err != nil {
			return nil, err
		}
		if err := w.Close(); err != nil {
			return nil, err
		}
		if err := aw.Close(); err != nil {
			return nil, err
		}
		armored := buf.String()
		if !strings.HasSuffix(armored, "\n") {
			armored += "\n"
		}
		out = append(out,
			Line{Key: fmt.Sprintf("sops_age__list_%d__map_enc", i), Value: armored},
			Line{Key: fmt.Sprintf("sops_age__list_%d__map_recipient", i), Value: strings.TrimSpace(r)})
	}
	return out, nil
}

var encRe = regexp.MustCompile(`^ENC\[AES256_GCM,data:(.*),iv:(.+),tag:(.+),type:(.+)\]$`)

func encryptValue(plain string, key []byte, ad, typ string) (string, error) {
	if plain == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, nonceSize)
	if err != nil {
		return "", err
	}
	iv := make([]byte, nonceSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), []byte(ad))
	n := len(sealed) - gcm.Overhead()
	return fmt.Sprintf("ENC[AES256_GCM,data:%s,iv:%s,tag:%s,type:%s]",
		base64.StdEncoding.EncodeToString(sealed[:n]), base64.StdEncoding.EncodeToString(iv),
		base64.StdEncoding.EncodeToString(sealed[n:]), typ), nil
}

func decryptValue(value string, key []byte, ad string) (string, error) {
	if value == "" {
		return "", nil
	}
	m := encRe.FindStringSubmatch(value)
	if m == nil {
		return "", errors.New("not an ENC[AES256_GCM,…] value")
	}
	data, err1 := base64.StdEncoding.DecodeString(m[1])
	iv, err2 := base64.StdEncoding.DecodeString(m[2])
	tag, err3 := base64.StdEncoding.DecodeString(m[3])
	if err := errors.Join(err1, err2, err3); err != nil {
		return "", fmt.Errorf("base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCMWithNonceSize(block, len(iv))
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, iv, append(data, tag...), []byte(ad))
	if err != nil {
		return "", errors.New("does not decrypt with this data key (wrong key, or the value was changed)")
	}
	return string(plain), nil
}

// LoadIdentities reads age identities the way sops does: the key itself from
// SOPS_AGE_KEY, a key file from SOPS_AGE_KEY_FILE, and any explicit files.
// No identity at all is not an error here — callers decide what that means.
func LoadIdentities(getenv func(string) string, files ...string) ([]age.Identity, error) {
	var ids []age.Identity
	add := func(src, text string) error {
		parsed, err := age.ParseIdentities(strings.NewReader(text))
		if err != nil {
			return fmt.Errorf("%s: %w", src, err)
		}
		ids = append(ids, parsed...)
		return nil
	}
	if v := getenv("SOPS_AGE_KEY"); strings.TrimSpace(v) != "" {
		if err := add("SOPS_AGE_KEY", v); err != nil {
			return nil, err
		}
	}
	if f := getenv("SOPS_AGE_KEY_FILE"); f != "" {
		files = append(files, f)
	}
	for _, f := range files {
		b, err := os.ReadFile(f) // #nosec G304 -- an operator-named key file
		if err != nil {
			return nil, fmt.Errorf("age key file: %w", err)
		}
		if err := add(f, string(b)); err != nil {
			return nil, err
		}
	}
	return ids, nil
}
