// Command agetool is the e2e test's stand-in for the age CLI, which is not
// installed on CI: "agetool keygen <key file>" writes a new identity and
// prints its public key, "agetool decrypt <key file> <file>" writes the
// decrypted file to stdout, like `age -d -i <key file> <file>`.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"filippo.io/age"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "agetool:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	switch {
	case len(args) == 2 && args[0] == "keygen":
		id, err := age.GenerateX25519Identity()
		if err != nil {
			return err
		}
		if err := os.WriteFile(args[1], []byte(id.String()+"\n"), 0o600); err != nil {
			return err
		}
		fmt.Println(id.Recipient())
		return nil
	case len(args) == 3 && args[0] == "decrypt":
		key, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		id, err := age.ParseX25519Identity(strings.TrimSpace(string(key)))
		if err != nil {
			return err
		}
		in, err := os.Open(args[2])
		if err != nil {
			return err
		}
		defer in.Close()
		r, err := age.Decrypt(in, id)
		if err != nil {
			return err
		}
		_, err = io.Copy(os.Stdout, r)
		return err
	}
	return fmt.Errorf("usage: agetool keygen <key file> | agetool decrypt <key file> <file>")
}
