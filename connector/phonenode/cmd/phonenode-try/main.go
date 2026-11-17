// Command phonenode-try is the phone's node on a Mac terminal. It redeems a
// Desk QR code, joins 6net as the phone will, and dials a Mac's door, so the
// 6net half of the phone can be exercised independently of Swift.
//
//	pbpaste | phonenode-try claim --url - --name "Morgan's harness" --state DIR
//	phonenode-try join --state DIR
//	phonenode-try door --state DIR --address fd06:6e7:… --session ID
//
// The code, the poll secret, the bearer and the pre-auth key go to
// DIR/phone.json (0600) and nowhere else: never to stdout, stderr or a log.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var err error
	switch os.Args[1] {
	case "claim":
		err = claimCommand(ctx, os.Args[2:], os.Stdin, os.Stdout)
	case "join":
		err = joinCommand(os.Args[2:], os.Stdout)
	case "door":
		err = doorCommand(ctx, os.Args[2:], os.Stdout)
	default:
		usage()
	}
	if errors.Is(err, flag.ErrHelp) {
		return // the flags were printed: -h is not a failure
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "phonenode-try:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: phonenode-try claim|join|door [flags]; each answers -h")
	os.Exit(2)
}
