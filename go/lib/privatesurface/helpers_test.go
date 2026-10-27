package privatesurface_test

import "crypto/tls"

func tlsConfigInsecure() tls.Config {
	return tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} //nolint:gosec // the test proves the server refuses a client without a certificate
}
