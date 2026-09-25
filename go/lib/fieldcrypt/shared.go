package fieldcrypt

import (
	"fmt"
	"sync"
)

// The process keyring.
//
// Generated storage code calls the three helpers below and holds no state of
// its own: a keyring is deployment configuration, not per-handler data, and
// threading one through every emitted signature would put a crypto object in
// the shape of every generated struct for a column most projects do not have.
//
// It is set ONCE, at start-up, by Init. Nothing lazily reads the environment
// on first use — that is the shape where a missing key surfaces as a failed
// query hours later, on a machine nobody is watching, instead of as a service
// that refused to start.
var (
	sharedMu  sync.RWMutex
	sharedKR  *Keyring
	sharedErr = errNotInitialised
)

var errNotInitialised = fmt.Errorf(
	"fieldcrypt: the process keyring was never initialised — a service with a CRYPTED_SECRET "+
		"column must call fieldcrypt.Init() during start-up so a missing or malformed %s fails "+
		"the boot rather than the first query that touches the column", EnvKeys)

// Init reads the environment and installs the process keyring.
//
// Called by a generated service's boot path when its schema has at least one
// CRYPTED_SECRET column. Returning an error here is the point: the caller is
// expected to refuse to start.
func Init() error {
	kr, err := FromEnv()
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if err != nil {
		sharedKR, sharedErr = nil, err
		return err
	}
	sharedKR, sharedErr = kr, nil
	return nil
}

// SetShared installs a keyring directly. For tests, and for a host that gets
// its key material from somewhere other than the environment.
func SetShared(kr *Keyring) {
	sharedMu.Lock()
	defer sharedMu.Unlock()
	if kr == nil {
		sharedKR, sharedErr = nil, errNotInitialised
		return
	}
	sharedKR, sharedErr = kr, nil
}

func shared() (*Keyring, error) {
	sharedMu.RLock()
	defer sharedMu.RUnlock()
	return sharedKR, sharedErr
}

// DecryptShared reads a stored value back through the process keyring.
func DecryptShared(stored string) (string, error) {
	kr, err := shared()
	if err != nil {
		return "", err
	}
	return kr.Decrypt(stored)
}

// EncryptSharedRandomised is the default write path: a fresh nonce per value.
func EncryptSharedRandomised(plain string) (string, error) {
	kr, err := shared()
	if err != nil {
		return "", err
	}
	return kr.EncryptRandomised(plain)
}

// EncryptSharedDeterministic is the write path for a column declared `unique`,
// where an index and a value lookup have to work.
func EncryptSharedDeterministic(plain string) (string, error) {
	kr, err := shared()
	if err != nil {
		return "", err
	}
	return kr.EncryptDeterministic(plain)
}
