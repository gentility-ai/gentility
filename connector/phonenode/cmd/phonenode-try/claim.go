package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// pollEvery is the phone's own poll interval. A test shortens it.
var pollEvery = 2 * time.Second

// claimLifetime is the five-minute claim lifetime: how long the poll goes on
// when the claim's expires_at cannot be read.
const claimLifetime = 5 * time.Minute

// claimClient follows no redirect: a 307 or 308 would resend the body, the
// code or the poll secret, to wherever it pointed. The 3xx is answered as
// the refusal it is.
func claimClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type claimOptions struct{ Link, Name, Dir string }

func claimCommand(ctx context.Context, args []string, stdin io.Reader, out io.Writer) error {
	flags := flag.NewFlagSet("claim", flag.ContinueOnError)
	link := flags.String("url", "", `the QR's link, or "-" to read it from stdin (keeps the code out of shell history)`)
	name := flags.String("name", "", "the phone name the Mac's Allow card shows")
	dir := flags.String("state", "", "the directory phone.json and the node are kept in")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *link == "" || *name == "" || *dir == "" {
		return errors.New("claim needs --url, --name and --state")
	}
	return claim(ctx, claimClient(nil), claimOptions{Link: *link, Name: *name, Dir: *dir}, stdin, out)
}

func claim(ctx context.Context, client *http.Client, o claimOptions, stdin io.Reader, out io.Writer) error {
	raw := o.Link
	if raw == "-" {
		line, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && line == "" {
			return errors.New("no link on stdin")
		}
		raw = strings.TrimSpace(line)
	}
	origin, code, err := parseClaimLink(raw)
	if err != nil {
		return err
	}
	// Before the claim, so a directory that cannot hold the bearer does not
	// spend a code and leave an Allow card on the Mac.
	if err := stateDir(o.Dir); err != nil {
		return err
	}

	var claimed struct {
		MachineLabel string `json:"machine_label"`
		ExpiresAt    string `json:"expires_at"`
		PollSecret   string `json:"poll_secret"`
		Match        string `json:"match"`
	}
	if err := post(ctx, client, origin+"/oauth/phone/claim",
		map[string]string{"code": code, "phone_name": o.Name}, http.StatusAccepted, &claimed); err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	fmt.Fprintf(out, "Check on your Mac: type %s on %s's Allow card (the code expires %s)\n",
		claimed.Match, claimed.MachineLabel, claimed.ExpiresAt)

	reply, err := pollUntilDecided(ctx, client, origin, claimed.PollSecret, pollDeadline(claimed.ExpiresAt, time.Now()), out)
	if err != nil {
		return err
	}
	if err := writePaired(o.Dir, paired{
		Origin:      origin,
		PhoneName:   o.Name,
		AccessToken: reply.AccessToken,
		TokenType:   reply.TokenType,
		ExpiresIn:   reply.ExpiresIn,
		Scope:       reply.Scope,
		Enrollment:  reply.Enrollment,
	}); err != nil {
		return err
	}
	fmt.Fprintf(out, "allowed: the bearer and the join material are in %s (0600); run join within ten minutes\n",
		filepath.Join(o.Dir, pairedFile))
	return nil
}

// parseClaimLink reads the QR's link as the app will:
// https, or http for localhost and *.localhost only; the path /phone/claim;
// the code in the fragment. It answers the origin the claim is posted to.
// No refusal quotes the link: its fragment is the code.
func parseClaimLink(raw string) (origin, code string, err error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", errors.New("the link is not a URL") // url.Error would quote it, code and all
	}
	if u.User != nil {
		return "", "", errors.New("the link names a user before its host, which no Desk QR does")
	}
	host := u.Hostname()
	if host == "" {
		return "", "", errors.New("the link names no host")
	}
	local := host == "localhost" || strings.HasSuffix(host, ".localhost")
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && local:
	default:
		return "", "", fmt.Errorf("the link must be https (http only for localhost), not %q", u.Scheme)
	}
	if u.Path != "/phone/claim" {
		return "", "", fmt.Errorf("the link's path is %q, not /phone/claim", u.Path)
	}
	if u.Fragment == "" {
		return "", "", errors.New("the link carries no code after #")
	}
	return u.Scheme + "://" + u.Host, u.Fragment, nil
}

type pollReply struct {
	Status      string          `json:"status"`
	Failure     string          `json:"failure"`
	AccessToken string          `json:"access_token"`
	TokenType   string          `json:"token_type"`
	ExpiresIn   int             `json:"expires_in"`
	Scope       string          `json:"scope"`
	Enrollment  json.RawMessage `json:"enrollment"`
}

// pollDeadline is when the claim's code expires, by this clock: the server
// answers expired from then on, so nothing is gained polling past it.
func pollDeadline(expiresAt string, now time.Time) time.Time {
	at, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return now.Add(claimLifetime)
	}
	return at
}

// pollUntilDecided polls until the Mac's decision, or the code's expiry at
// until. A poll that gets no answer, or a 5xx, is tried again; any other
// refusal ends it.
func pollUntilDecided(ctx context.Context, client *http.Client, origin, secret string, until time.Time, out io.Writer) (pollReply, error) {
	polling, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	ended := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return fmt.Errorf("the code expired (%s) before the Mac decided", until.Format(time.RFC3339))
	}
	last := ""
	say := func(line string) {
		if line != last {
			fmt.Fprintln(out, line)
			last = line
		}
	}
	for {
		var reply pollReply
		err := post(polling, client, origin+"/oauth/phone/poll",
			map[string]string{"poll_secret": secret}, http.StatusOK, &reply)
		// An answer is read before the deadline is: an allowed that lands as
		// the code expires carries single-use join material, and dropping it
		// would spend the pairing for nothing.
		var again *retryable
		switch {
		case err != nil && polling.Err() != nil:
			return pollReply{}, ended()
		case errors.As(err, &again):
			say(fmt.Sprintf("poll: %s; trying again", err))
		case err != nil:
			return pollReply{}, fmt.Errorf("poll: %w", err)
		default:
			say("poll: " + reply.Status)
			switch reply.Status {
			case "allowed":
				if reply.AccessToken == "" {
					return pollReply{}, errors.New("the poll said allowed and handed over no bearer")
				}
				if len(reply.Enrollment) == 0 {
					return pollReply{}, errors.New("the poll said allowed and handed over no join material")
				}
				return reply, nil
			case "pending", "asking", "allowed_pending":
			case "failed":
				return pollReply{}, fmt.Errorf("the server could not finish the pairing: %s", reply.Failure)
			default: // denied, spent, expired, and any word this build does not know
				return pollReply{}, fmt.Errorf("the pairing ended %s", reply.Status)
			}
		}
		select {
		case <-polling.Done():
			return pollReply{}, ended()
		case <-time.After(pollEvery):
		}
	}
}

// retryable is a failure the same request may not meet again: no answer at
// all, or a 5xx.
type retryable struct{ err error }

func (r *retryable) Error() string { return r.err.Error() }
func (r *retryable) Unwrap() error { return r.err }

// post sends body as JSON and decodes a want-status answer into into. A
// refusal is its error word and sentence; nothing else of a body is echoed.
func post(ctx context.Context, client *http.Client, endpoint string, body any, want int, into any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return &retryable{err}
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return &retryable{err}
	}
	if response.StatusCode != want {
		refusal := fmt.Errorf("HTTP %d", response.StatusCode)
		var refused struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		if json.Unmarshal(data, &refused) == nil && refused.Error != "" {
			refusal = fmt.Errorf("%s: %s", refused.Error, refused.Description)
		}
		if response.StatusCode >= 500 {
			return &retryable{refusal}
		}
		return refusal
	}
	return json.Unmarshal(data, into)
}
