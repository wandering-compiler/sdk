// Command sopscompat drives sopsenv from a shell, for scripts/check-sops-compat.sh:
// the compatibility proof needs this package and the real sops CLI on the
// same files, in both directions.
//
//	sopscompat encrypt <recipient> <plain> <out>
//	sopscompat decrypt <keyfile> <enc> <out>
package main

import (
	"fmt"
	"os"
	"time"

	"github.com/wandering-compiler/sdk/go/service/secret/sopsenv"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sopscompat:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: sopscompat encrypt|decrypt <recipient|keyfile> <in> <out>")
	}
	in, err := os.ReadFile(args[2])
	if err != nil {
		return err
	}
	var out []byte
	switch args[0] {
	case "encrypt":
		lines, err := sopsenv.Parse(in)
		if err != nil {
			return err
		}
		if out, err = sopsenv.Encrypt(lines, []string{args[1]}, time.Now()); err != nil {
			return err
		}
	case "decrypt":
		ids, err := sopsenv.LoadIdentities(func(string) string { return "" }, args[1])
		if err != nil {
			return err
		}
		lines, err := sopsenv.Decrypt(in, ids)
		if err != nil {
			return err
		}
		out = sopsenv.Format(lines)
	default:
		return fmt.Errorf("unknown verb %q", args[0])
	}
	return os.WriteFile(args[3], out, 0o600)
}
